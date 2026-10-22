package feishu

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	lark "github.com/larksuite/oapi-sdk-go/v3"
)

func TestFileCitationReplyDoesNotExposeInternalPath(t *testing.T) {
	citation := `:codex-file-citation{path="/private/var/lib/example/inputs/source.pdf" purpose="source"}`
	for _, body := range []string{"Next question?", "**Pending**. Next question?", `<at user_id="fictional">Owner</at> Next question?`, strings.Repeat("| A | B |\n|---|---|\n| 1 | 2 |\n\n", 6)} {
		wantType, want := buildReplyContent(body)
		gotType, got := buildReplyContent(body + citation)
		if gotType != wantType || got != want {
			t.Errorf("citation changed reply: got %s %s; want %s %s", gotType, got, wantType, want)
		}
	}
	for _, render := range []func(string) string{buildCardJSON, buildPostMdJSON, sanitizeCardMarkdownForCard} {
		if got, want := render("Next question?"+citation), render("Next question?"); got != want {
			t.Errorf("citation leaked from card/post: %s", got)
		}
	}
	if got, want := buildCardJSONWithStatusFooter("Answer"+citation, "Footer"+citation), buildCardJSONWithStatusFooter("Answer", "Footer"); got != want {
		t.Errorf("citation leaked from footer card: %s", got)
	}
}

func TestFileCitationQuotedBracesAndIncompleteFrames(t *testing.T) {
	for _, citation := range []string{
		`:codex-file-citation{path="/private/example/a}b.pdf" purpose="source"}`,
		`:codex-file-citation{path='C:\private\a}b.pdf' purpose='source'}`,
		`:codex-file-citation{path="/private/example/a\"}b.pdf"}`,
	} {
		if got := sanitizeMarkdownURLs("Before" + citation + "After" + citation); got != "BeforeAfter" {
			t.Errorf("quoted attribute escaped suppression: %q", got)
		}
		for i := len(":codex-file-citation{"); i <= len(citation); i++ {
			if got := sanitizeMarkdownURLs("Before" + citation[:i]); got != "Before" {
				t.Fatalf("partial citation leaked at byte %d: %q", i, got)
			}
		}
	}
	ordinary := "[Source](https://example.com/report) and `code` /tmp/example.txt :ordinary{value}"
	if got := sanitizeMarkdownURLs(ordinary); got != ordinary {
		t.Errorf("unrelated text changed: %q", got)
	}
}

func TestFileCitationUnicodeMarkerDoesNotExposeInternalPath(t *testing.T) {
	citation := "\ue200" + `codex-file-citation{path="/private/example/a}b.pdf" purpose="source"}`
	for _, body := range []string{"Next question?", "**Pending**. Next question?", `<at user_id="fictional">Owner</at> Next question?`, strings.Repeat("| A | B |\n|---|---|\n| 1 | 2 |\n\n", 6)} {
		wantType, want := buildReplyContent(body)
		if gotType, got := buildReplyContent(body + citation); gotType != wantType || got != want {
			t.Errorf("Unicode citation changed reply: got %s %s; want %s %s", gotType, got, wantType, want)
		}
	}
	for _, render := range []func(string) string{buildCardJSON, buildPostMdJSON, sanitizeCardMarkdownForCard} {
		if got, want := render("Before"+citation+"After"), render("BeforeAfter"); got != want {
			t.Errorf("Unicode citation leaked from card/post: %s", got)
		}
	}
	if got, want := buildCardJSONWithStatusFooter("Answer"+citation, "Footer"+citation), buildCardJSONWithStatusFooter("Answer", "Footer"); got != want {
		t.Errorf("Unicode citation leaked from footer card: %s", got)
	}
	for i := len("\ue200codex-file-citation{"); i <= len(citation); i++ {
		if got := stripFileCitationDirectives("Before" + citation[:i]); got != "Before" {
			t.Fatalf("partial Unicode citation leaked at byte %d: %q", i, got)
		}
	}
	if got := stripFileCitationDirectives("Before" + citation + ":codex-file-citation{path='C:\\private\\file.pdf'}After"); got != "BeforeAfter" {
		t.Errorf("mixed citation markers leaked: %q", got)
	}
	ordinary := "[Source](https://example.com/report) \ue200ordinary{value} /tmp/example.txt"
	if got := stripFileCitationDirectives(ordinary); got != ordinary {
		t.Errorf("unrelated text changed: %q", got)
	}
}

func TestFileCitationStreamPayloadDoesNotExposeInternalPath(t *testing.T) {
	var payloads []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/tenant_access_token/internal") {
			writeJSON(t, w, map[string]any{"code": 0, "expire": 7200, "tenant_access_token": "fictional"})
			return
		}
		if r.Method != http.MethodPut || !strings.HasSuffix(r.URL.Path, "/content") {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var body struct {
			Content string `json:"content"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		payloads = append(payloads, body.Content)
		writeJSON(t, w, map[string]any{"code": 0})
	}))
	defer srv.Close()
	p := &Platform{platformName: "feishu", domain: srv.URL, appID: t.Name(), client: lark.NewClient(t.Name(), "fictional", lark.WithOpenBaseUrl(srv.URL), lark.WithHttpClient(srv.Client()))}
	h := &feishuPreviewHandle{cardID: "fictional-card"}
	for _, text := range []string{`Answer:codex-file-citation{path="/private/example`, `Answer:codex-file-citation{path="/private/example.pdf"} next`, "Answer\ue200codex-file-citation{path=\"/private/example", "Answer\ue200codex-file-citation{path=\"/private/example.pdf\"} next"} {
		if err := p.StreamRichCardText(context.Background(), h, text); err != nil {
			t.Fatal(err)
		}
	}
	if len(payloads) != 4 || payloads[0] != "Answer" || payloads[1] != "Answer next" || payloads[2] != "Answer" || payloads[3] != "Answer next" {
		t.Fatalf("stream payloads leaked citation: %q", payloads)
	}
}
