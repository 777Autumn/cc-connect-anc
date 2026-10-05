package feishu

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

func TestFileWorkPostAttachments(t *testing.T) {
	for _, body := range []string{
		`{"content":[[{"tag":"text","text":"review"}]],"files":[{"file_key":"quote","file_name":"quote.txt"}]}`,
		`{"files":[{"file_key":"quote","file_name":"quote.txt"}]}`,
		`{"en_us":{"files":[{"file_key":"quote","file_name":"quote.txt"}]},"zh_cn":{"files":[{"file_key":"quote","file_name":"quote.txt"}]}}`,
		`{"content":[[{"tag":"img","image_key":"picture"}]],"files":[{"file_key":"quote","file_name":"quote.txt"}]}`,
	} {
		t.Run(body, func(t *testing.T) {
			var picture bytes.Buffer
			if err := png.Encode(&picture, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
				t.Fatal(err)
			}
			var requests atomic.Int32
			p, got := fileWorkFixture(t, func(_ core.Message, parent string) bool { return parent == "known" }, func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if !strings.Contains(r.URL.Path, "/messages/current/resources/") {
					t.Error("wrong message ownership", r.URL.Path)
				}
				if r.URL.Query().Get("type") == "image" {
					_, _ = w.Write(picture.Bytes())
					return
				}
				_, _ = io.WriteString(w, "fictional quote 123")
			})
			if err := p.SetFileWorkImagesEnabled(true); err != nil {
				t.Fatal(err)
			}
			msg := receiveFileWorkMessage(t, p, got, fileWorkEvent("current", "post", body, "group", "known", false))
			want := 1
			if strings.Contains(body, "picture") {
				want++
			}
			if len(msg.Files) != want || requests.Load() != int32(want) {
				t.Fatalf("attachments=%d requests=%d, want %d", len(msg.Files), requests.Load(), want)
			}
			file := msg.Files[len(msg.Files)-1]
			if file.ReceiveError != "" || !file.RequireSave || file.FileName != "quote.txt" || string(file.Data) != "fictional quote 123" {
				t.Fatalf("file lost: %#v", file)
			}
		})
	}
}

func TestFileWorkPostRejectsIncompleteBatch(t *testing.T) {
	for _, body := range []string{
		`{"files":[{"file_key":"quote","file_name":"quote.txt"}],"zh_cn":{"files":[{"file_key":"folder","file_name":"folder","is_folder":true}]}}`,
		`{"content":[[{"tag":"text","text":"review"}]],"files":[{"file_key":"quote"}]}`,
		`{"content":[],"files":[{"file_name":"quote.txt"}]}`,
		`{"content":[],"files":[{"file_key":"quote","file_name":"folder","is_folder":true}]}`,
		`{"en_us":{"content":[],"files":[{"file_key":"a","file_name":"a.txt"}]},"zh_cn":{"content":[]}}`,
		`{"content":[[{"tag":"img","image_key":"p1"},{"tag":"img","image_key":"p2"},{"tag":"img","image_key":"p3"},{"tag":"img","image_key":"p4"}]],"files":[{"file_key":"quote","file_name":"quote.txt"}]}`,
	} {
		t.Run(body, func(t *testing.T) {
			var requests atomic.Int32
			p, got := fileWorkFixture(t, func(core.Message, string) bool { return true }, func(w http.ResponseWriter, _ *http.Request) { requests.Add(1); _, _ = io.WriteString(w, "unwanted") })
			if err := p.SetFileWorkImagesEnabled(true); err != nil {
				t.Fatal(err)
			}
			msg := receiveFileWorkMessage(t, p, got, fileWorkEvent("current", "post", body, "p2p", "", false))
			if requests.Load() != 0 || msg.Content != "" || len(msg.Files) != 1 || msg.Files[0].ReceiveError == "" {
				t.Fatal("invalid post was not rejected before download")
			}
		})
	}
}

func TestFileWorkPostDownloadFailureDiscardsWholeBatch(t *testing.T) {
	for _, failure := range []string{"denied", "oversize"} {
		t.Run(failure, func(t *testing.T) {
			var requests atomic.Int32
			p, got := fileWorkFixture(t, func(core.Message, string) bool { return true }, func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if strings.HasSuffix(r.URL.Path, "/failed") {
					if failure == "oversize" {
						w.Header().Set("Content-Length", strconv.FormatInt(core.DefaultFileInputLimit+1, 10))
						w.WriteHeader(http.StatusOK)
						return
					}
					w.WriteHeader(http.StatusForbidden)
					return
				}
				_, _ = io.WriteString(w, "fictional")
			})
			body := `{"content":[[{"tag":"text","text":"review"}]],"files":[{"file_key":"ok","file_name":"ok.txt"},{"file_key":"failed","file_name":"failed.txt"},{"file_key":"unreached","file_name":"unreached.txt"}]}`
			msg := receiveFileWorkMessage(t, p, got, fileWorkEvent("current", "post", body, "p2p", "", false))
			if requests.Load() != 2 || msg.Content != "" || len(msg.Files) != 1 || msg.Files[0].ReceiveError == "" || len(msg.Files[0].Data) != 0 {
				t.Fatal("failed batch leaked partial materials or kept downloading")
			}
		})
	}
}

func TestFileWorkPostAuthorizationPrecedesDownloads(t *testing.T) {
	body := `{"content":[[{"tag":"text","text":"review"}]],"files":[{"file_key":"private","file_name":"private.txt"}]}`
	for _, scenario := range []string{"other sender", "other chat", "unknown private parent", "unknown group parent with files"} {
		t.Run(scenario, func(t *testing.T) {
			var requests atomic.Int32
			p, got := fileWorkFixture(t, func(core.Message, string) bool { return false }, func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				_, _ = io.WriteString(w, "must not read")
			})
			if err := p.SetFileWorkImagesEnabled(true); err != nil {
				t.Fatal(err)
			}
			event := fileWorkEvent("current", "post", body, "p2p", "", false)
			switch scenario {
			case "other sender":
				event.Event.Sender.SenderId.OpenId = strPtr("other-sender")
			case "other chat":
				event.Event.Message.ChatId = strPtr("other-chat")
			case "unknown private parent":
				event.Event.Message.ParentId = strPtr("unknown-parent")
			case "unknown group parent with files":
				event = fileWorkEvent("current", "post", body, "group", "unknown-parent", true)
			}
			if strings.HasPrefix(scenario, "unknown") {
				msg := receiveFileWorkMessage(t, p, got, event)
				if len(msg.Files) != 0 || msg.FileWorkNewInput {
					t.Fatal("unknown parent accepted new file input")
				}
			} else {
				if err := p.onMessage(context.Background(), event); err != nil {
					t.Fatal(err)
				}
				select {
				case <-got:
					t.Fatal("unauthorized sender or chat reached the host")
				default:
				}
			}
			if requests.Load() != 0 {
				t.Fatal("unauthorized post fetched resources or parent metadata")
			}
		})
	}
}
