package feishu

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

func TestFileWorkPostContentV2PreservesControlledIntake(t *testing.T) {
	p, got := fileWorkFixture(t, func(core.Message, string) bool { return false }, func(http.ResponseWriter, *http.Request) {
		t.Error("text intake must not fetch resources")
	})
	const body = `{"title":"","content":[[{"tag":"text","text":"Show my permitted records","style":[]}]],"content_v2":[[{"style":[],"text":"Show my permitted records","tag":"text"}]]}`
	for i, raw := range []string{body, `{"en_us":` + body + `}`} {
		msg := receiveFileWorkMessage(t, p, got, fileWorkEvent(fmt.Sprintf("v2-%d", i), "post", raw, "p2p", "", false))
		if msg.Content != "Show my permitted records" || len(msg.Files) != 0 {
			t.Fatalf("mobile text rejected or altered: content=%q files=%v", msg.Content, msg.Files)
		}
	}
}

func TestFileWorkPostContentV2CannotHideDifferentContent(t *testing.T) {
	p := &Platform{}
	for _, raw := range []string{
		`{"content":[[{"tag":"text","text":"safe"}]],"content_v2":[[{"tag":"img","image_key":"hidden"}]]}`,
		`{"content":[[{"tag":"text","text":"safe"}]],"content_v2":[[{"tag":"text","text":"different"}]]}`,
		`{"content":[[{"tag":"text","text":"safe"}]],"content_v2":null}`,
		`{"content_v2":[[{"tag":"text","text":"unsupported v2-only"}]]}`,
		`{"content":[[{"tag":"media","file_key":"unsupported"}]],"content_v2":[[{"tag":"media","file_key":"unsupported"}]]}`,
		`{"content":[],"content_v2":[],"unknown":[]}`,
	} {
		for _, body := range []string{raw, `{"en_us":{"content":[[{"tag":"text","text":"safe"}]]},"zh_cn":` + raw + `}`} {
			if _, _, _, ok := p.fileWorkPostContent(body, true); ok {
				t.Fatalf("unvalidated alternate content accepted: %s", body)
			}
		}
	}
}

func TestFileWorkPostContentV2KeepsImageGate(t *testing.T) {
	p := &Platform{}
	const body = `{"content":[[{"tag":"img","image_key":"fixture-image"}],[{"tag":"text","text":"Use this image"}]],"content_v2":[[{"tag":"img","image_key":"fixture-image"}],[{"tag":"text","text":"Use this image"}]]}`
	text, images, files, ok := p.fileWorkPostContent(body, true)
	if !ok || text != "Use this image" || len(images) != 1 || images[0] != "fixture-image" || len(files) != 0 {
		t.Fatalf("validated image lost: ok=%v text=%q images=%v files=%v", ok, text, images, files)
	}
	if _, _, _, ok := p.fileWorkPostContent(body, false); ok {
		t.Fatal("alternate content bypassed disabled images")
	}
}
