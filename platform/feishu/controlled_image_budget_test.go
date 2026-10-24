package feishu

import (
	"bytes"
	"crypto/sha256"
	"net/http"
	"os"
	"strconv"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

func TestControlledImageUsesFileBudgetAndPreservesOriginal(t *testing.T) {
	// Synthetic transport fixture at the incident size; private originals stay out of Git.
	data := append(feishuImageFixture(t, 1), make([]byte, 18350890)...)
	data = data[:18350890]
	if path := os.Getenv("ANC_TEST_ORIGINAL_IMAGE"); path != "" {
		var err error
		data, err = os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(data) <= core.MaxImageBytes || len(data) > core.DefaultFileInputLimit {
		t.Fatal("fixture must exercise the gap between visual and file limits")
	}
	for _, chunked := range []bool{false, true} {
		t.Run(strconv.FormatBool(chunked), func(t *testing.T) {
			p, got := fileWorkFixture(t, func(core.Message, string) bool { return false }, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("type") != "image" {
					t.Error("not an image resource")
				}
				w.Header().Set("Content-Type", "image/png")
				if !chunked {
					w.Header().Set("Content-Length", strconv.Itoa(len(data)))
				}
				w.WriteHeader(http.StatusOK)
				w.(http.Flusher).Flush()
				_, _ = w.Write(data)
			})
			if err := p.SetFileWorkImagesEnabled(true); err != nil {
				t.Fatal(err)
			}
			msg := receiveFileWorkMessage(t, p, got, fileWorkEvent("original", "image", `{"image_key":"fixture"}`, "p2p", "", false))
			if len(msg.Files) != 1 {
				t.Fatalf("controlled file count=%d", len(msg.Files))
			}
			if msg.Files[0].ReceiveError != "" || sha256.Sum256(msg.Files[0].Data) != sha256.Sum256(data) {
				t.Fatalf("controlled original: error=%s bytes=%d sha256=%x", msg.Files[0].ReceiveError, len(msg.Files[0].Data), sha256.Sum256(msg.Files[0].Data))
			}
			if !msg.Files[0].RequireSave || len(msg.Images) != 0 {
				t.Fatal("original was routed to visual input instead of file work")
			}
			legacy := p.receiveImage("original", "fixture")
			if legacy.ReceiveError != core.MsgImageLimit || len(legacy.Data) != 0 {
				t.Fatal("ordinary visual limit weakened")
			}
		})
	}
}

func TestControlledImageFileLimitsAndFailures(t *testing.T) {
	for _, tc := range []struct {
		name            string
		size, remaining int
		declared        bool
		want            core.MsgKey
	}{
		{"exact", core.DefaultFileInputLimit, maxFileInputBatchBytes, true, ""},
		{"declared-over", core.DefaultFileInputLimit + 1, maxFileInputBatchBytes, true, core.MsgFileInputTooLarge},
		{"chunked-over", core.DefaultFileInputLimit + 1, maxFileInputBatchBytes, false, core.MsgFileInputTooLarge},
		{"remaining-exact", 1024, 1024, false, ""},
		{"remaining-over", 1025, 1024, false, core.MsgFileInputTooLarge},
		{"truncated", 1024, maxFileInputBatchBytes, true, core.MsgFileInputUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := append(feishuImageFixture(t, 1), bytes.Repeat([]byte{0}, tc.size)...)[:tc.size]
			p := imageReceiverFixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "image/png")
				if tc.declared {
					length := tc.size
					if tc.name == "truncated" {
						length++
					}
					w.Header().Set("Content-Length", strconv.Itoa(length))
				}
				w.WriteHeader(http.StatusOK)
				w.(http.Flusher).Flush()
				_, _ = w.Write(data)
			})
			budget := &fileReceiveBudget{bytes: int64(maxFileInputBatchBytes - tc.remaining)}
			f := p.receiveWorkImage(t.Context(), "original", "fixture", budget)
			if f.ReceiveError != tc.want {
				t.Fatalf("error=%s want=%s", f.ReceiveError, tc.want)
			}
			if tc.want != "" {
				if len(f.Data) != 0 || budget.failure != tc.want {
					t.Fatal("partial input or lost failure")
				}
			} else if !bytes.Equal(f.Data, data) {
				t.Fatal("accepted bytes changed")
			}
		})
	}
}
