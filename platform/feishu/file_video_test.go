package feishu

import (
	"bytes"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

func TestControlledVideoUsesOriginalResourceAndExistingLimit(t *testing.T) {
	data := []byte("synthetic transport bytes; Host decoder is a separate boundary")
	var requests atomic.Int32
	p, got := fileWorkFixture(t, func(_ core.Message, parent string) bool { return parent == "known" }, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Query().Get("type") != "file" || !strings.Contains(r.URL.Path, "original-video") {
			t.Error("downloaded cover or wrong resource", r.URL.Path)
		}
		_, _ = w.Write(data)
	})
	body := `{"file_key":"original-video","image_key":"cover-never-fetch"}`
	msg := receiveFileWorkMessage(t, p, got, fileWorkEvent("video-disabled", "media", body, "group", "known", false))
	if requests.Load() != 0 || len(msg.Files) != 1 || msg.Files[0].ReceiveError == "" {
		t.Fatal("disabled video downloaded")
	}
	if err := p.SetFileWorkVideoEnabled(true); err != nil {
		t.Fatal(err)
	}
	msg = receiveFileWorkMessage(t, p, got, fileWorkEvent("video-enabled", "media", body, "group", "known", false))
	if len(msg.Files) != 1 || !bytes.Equal(msg.Files[0].Data, data) || msg.Files[0].FileName != "video.mp4" || !msg.Files[0].RequireSave {
		t.Fatal("video original not preserved")
	}
	before := requests.Load()
	msg = receiveFileWorkMessage(t, p, got, fileWorkEvent("unknown-video", "media", body, "p2p", "unknown", false))
	if len(msg.Files) != 0 || requests.Load() != before {
		t.Fatal("unknown quote downloaded video")
	}
	data = make([]byte, core.DefaultFileInputLimit+1)
	msg = receiveFileWorkMessage(t, p, got, fileWorkEvent("large-video", "media", body, "group", "known", false))
	if len(msg.Files) != 1 || msg.Files[0].ReceiveError != core.MsgFileInputTooLarge || len(msg.Files[0].Data) != 0 {
		t.Fatal("video exceeded original file budget")
	}
}
