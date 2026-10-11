package crmfollowup

import (
	"strings"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

func TestHandoffReceiptSeparatesNotificationFromVerifiedBusiness(t *testing.T) {
	for _, status := range []string{"verified", "verification_unknown"} {
		card := receiptCard(map[string]any{"status": status, "code": "handoff_write_verified", "handoff": map[string]any{"status": "待接手"}, "notification_note": "untrusted notice text"}, core.LangChinese)
		found := false
		for _, element := range card.Elements {
			if text, ok := element.(core.CardPlainText); ok {
				if text.Content == core.NewI18n(core.LangChinese).T(core.MsgHandoffNotificationSeparate) {
					found = true
				}
				if strings.Contains(text.Content, "untrusted") {
					t.Fatal("raw notice text displayed")
				}
			}
		}
		if found != (status == "verified") {
			t.Fatal("wrong notification receipt", status)
		}
	}
}

func TestBanquetPreviewShowsRecipientVersionAndEveryFrozenMaterial(t *testing.T) {
	preview := map[string]any{"operation_type": "banquet_handoff_submit", "actor": "Fixture owner",
		"handoff": map[string]any{"name": "Synthetic banquet", "version": 2,
			"recipient": []any{map[string]any{"id": "ou_SECRET", "name": "Fixture recipient"}},
			"status":    "待接手", "missing": "Menu unconfirmed", "materials": []any{
				map[string]any{"name": "handoff.docx", "version": 1, "sha256": strings.Repeat("a", 64)},
				map[string]any{"name": "appendix.docx", "version": 3, "sha256": strings.Repeat("b", 64)},
			}}, "expires_at": "2026-10-07T10:00:00+08:00"}
	for _, language := range []core.Language{core.LangEnglish, core.LangChinese, core.LangTraditionalChinese, core.LangJapanese, core.LangSpanish} {
		body := renderPreview(preview, core.NewI18n(language))
		for _, expected := range []string{"Fixture recipient", "handoff.docx", "appendix.docx", strings.Repeat("b", 64), "Menu unconfirmed"} {
			if !strings.Contains(body, expected) {
				t.Fatalf("%s missing %s: %s", language, expected, body)
			}
		}
		if strings.Contains(body, "ou_SECRET") {
			t.Fatal("transport identity displayed")
		}
	}
}

func TestBanquetLabelsUseSharedTranslations(t *testing.T) {
	for _, language := range []core.Language{core.LangEnglish, core.LangChinese, core.LangTraditionalChinese, core.LangJapanese, core.LangSpanish} {
		i18n := core.NewI18n(language)
		for field, key := range banquetLabels {
			label := i18n.T(key)
			if label == "" || label == string(key) {
				t.Fatalf("missing %s translation for %s", language, field)
			}
		}
		body := renderBanquetReceipt(map[string]any{"venue": "Fixture hall", "url": "https://example.feishu.cn/base/test"}, i18n)
		if !strings.Contains(body, i18n.T(core.MsgBanquetVenue)) || !strings.Contains(body, i18n.T(core.MsgBanquetOpenRecord)) {
			t.Fatalf("card bypassed %s translations: %s", language, body)
		}
	}
}
