package feishu

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func TestControlledResourceRangeKeepsOriginalAndBounds(t *testing.T) {
	payload := bytes.Repeat([]byte("synthetic-video-original-"), 100000)
	for _, tc := range []struct {
		name  string
		fault string
		limit int64
		want  core.MsgKey
	}{
		{"legal", "", int64(len(payload)), ""},
		{"oversize", "", int64(len(payload) - 1), core.MsgFileInputTooLarge},
		{"wrong_start", "start", int64(len(payload)), core.MsgFileInputUnavailable},
		{"changed_total", "total", int64(len(payload)), core.MsgFileInputUnavailable},
		{"truncated", "short", int64(len(payload)), core.MsgFileInputUnavailable},
		{"range_ignored_midway", "mid200", int64(len(payload)), core.MsgFileInputUnavailable},
		{"changed_entity", "etag", int64(len(payload)), core.MsgFileInputUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			p := imageReceiverFixture(t, func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.URL.Query().Get("type") != "file" {
					t.Error("wrong resource type")
				}
				var start, end int
				if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"code":234037,"msg":"download interrupted"}`))
					return
				}
				if end >= len(payload) || start < 0 || end < start {
					t.Error("out of range request")
					w.WriteHeader(400)
					return
				}
				total, declaredStart := len(payload), start
				body := payload[start : end+1]
				status := http.StatusPartialContent
				etag := `"fictional-original"`
				if requests > 1 {
					switch tc.fault {
					case "start":
						declaredStart++
					case "total":
						total++
					case "short":
						body = body[:len(body)-1]
					case "mid200":
						status = http.StatusOK
					case "etag":
						etag = `"changed-original"`
					}
				}
				w.Header().Set("Content-Type", "application/octet-stream")
				w.Header().Set("ETag", etag)
				w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", declaredStart, end, total))
				w.Header().Set("Content-Length", strconv.Itoa(len(body)))
				w.WriteHeader(status)
				_, _ = w.Write(body)
			})
			got, err := p.downloadFileBounded(context.Background(), "original-message", "original-file", tc.limit)
			if tc.want != "" {
				requireFileInputError(t, err, tc.want)
				if got != nil {
					t.Fatal("partial data escaped")
				}
			} else if err != nil || sha256.Sum256(got) != sha256.Sum256(payload) {
				t.Fatalf("original not received: bytes=%d err=%v", len(got), err)
			}
			if tc.name == "oversize" && requests != 1 {
				t.Fatal("oversize was downloaded")
			}
		})
	}
}

func TestControlledResourceRangeUsesOneDeadline(t *testing.T) {
	p := imageReceiverFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.Header.Get("Range"), "bytes=0-0") {
			w.Header().Set("Content-Range", "bytes 0-0/2")
			w.WriteHeader(206)
			_, _ = w.Write([]byte("x"))
			return
		}
		<-r.Context().Done()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	got, err := p.downloadFileBounded(ctx, "message", "file", 2)
	requireFileInputError(t, err, core.MsgFileInputUnavailable)
	if got != nil || time.Since(started) > time.Second {
		t.Fatal("range reset deadline or returned partial input")
	}
}
