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
		{"missing_entity", "noetag", int64(len(payload)), core.MsgFileInputUnavailable},
		{"weak_entity", "weaketag", int64(len(payload)), core.MsgFileInputUnavailable},
		{"multiple_entities", "multietag", int64(len(payload)), core.MsgFileInputUnavailable},
		{"invalid_entity", "spaceetag", int64(len(payload)), core.MsgFileInputUnavailable},
		{"chunked_ranges", "chunked", int64(len(payload)), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			p := imageReceiverFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("type") != "file" {
					t.Error("wrong resource type")
				}
				var start, end int
				if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"code":234037,"msg":"download interrupted"}`))
					return
				}
				requests++
				if end >= len(payload) || start < 0 || end < start {
					t.Error("out of range request")
					w.WriteHeader(400)
					return
				}
				total, declaredStart := len(payload), start
				body := payload[start : end+1]
				status := http.StatusPartialContent
				etag := `"fictional-original"`
				if tc.fault == "noetag" {
					etag = ""
				} else if tc.fault == "weaketag" {
					etag = `W/"fictional-original"`
				} else if tc.fault == "multietag" {
					etag = `"v1", "v2"`
				} else if tc.fault == "spaceetag" {
					etag = `"v 1"`
				}
				if requests > 1 {
					if r.Header.Get("If-Match") != `"fictional-original"` {
						t.Error("missing original version precondition")
					}
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
				if tc.fault != "chunked" {
					w.Header().Set("Content-Length", strconv.Itoa(len(body)))
				}
				w.WriteHeader(status)
				if tc.fault == "chunked" {
					w.(http.Flusher).Flush()
				}
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
		if r.Header.Get("Range") == "" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"code":234037}`))
			return
		}
		if strings.HasPrefix(r.Header.Get("Range"), "bytes=0-0") {
			w.Header().Set("ETag", `"deadline-fixture"`)
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
	got, err := p.downloadFileBounded(ctx, "message", "file", 128)
	requireFileInputError(t, err, core.MsgFileInputUnavailable)
	if got != nil || time.Since(started) > time.Second {
		t.Fatal("range reset deadline or returned partial input")
	}
}

func TestControlledResourceSingleGetKeepsWorkingWithoutETag(t *testing.T) {
	payload := bytes.Repeat([]byte("unchanged original"), 100)
	requests := 0
	p := imageReceiverFixture(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("Range") != "" {
			t.Error("working download unexpectedly depends on ranged ETag")
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(payload)
	})
	got, err := p.downloadFileBounded(context.Background(), "message", "file", int64(len(payload)))
	if err != nil || requests != 1 || sha256.Sum256(got) != sha256.Sum256(payload) {
		t.Fatalf("working single download changed: calls=%d err=%v", requests, err)
	}
}

func TestControlledResourceDoesNotRetryOtherAPIErrors(t *testing.T) {
	requests := 0
	p := imageReceiverFixture(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":99991672,"msg":"synthetic permission refusal"}`))
	})
	data, err := p.downloadFileBounded(context.Background(), "message", "file", 1024)
	requireFileInputError(t, err, core.MsgFileInputUnavailable)
	if requests != 1 || data != nil {
		t.Fatal("permission failure retried or returned as original")
	}
}

func TestControlledResourceHTTPRefusalNeverNegotiates(t *testing.T) {
	for _, status := range []int{401, 403, 429, 500} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			requests := 0
			p := imageReceiverFixture(t, func(w http.ResponseWriter, r *http.Request) {
				requests++
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"code":234037}`))
			})
			data, err := p.downloadFileBounded(context.Background(), "message", "file", 1024)
			requireFileInputError(t, err, core.MsgFileInputUnavailable)
			if requests != 1 || data != nil {
				t.Fatal("HTTP refusal retried or returned as original")
			}
		})
	}
}
