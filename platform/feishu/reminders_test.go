package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chenhg5/cc-connect/core"
	lark "github.com/larksuite/oapi-sdk-go/v3"
)

func TestReminderSendPinsRouteUUIDAndReceipt(t *testing.T) {
	var bodies []map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "tenant_access_token") {
			writeJSON(t, w, map[string]any{"code": 0, "expire": 7200, "tenant_access_token": "fixture-reminder-token"})
			return
		}
		if r.URL.Path != "/open-apis/im/v1/messages" || r.URL.Query().Get("receive_id_type") != "chat_id" {
			t.Errorf("unexpected destination API")
			w.WriteHeader(404)
			return
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		bodies = append(bodies, body)
		writeJSON(t, w, map[string]any{"code": 0, "data": map[string]any{"message_id": "accepted-fixture"}})
	}))
	defer server.Close()
	p := &Platform{strictRoutes: true, allowChat: "private-fixture", client: lark.NewClient("reminder-test-app", "fixture-secret", lark.WithOpenBaseUrl(server.URL), lark.WithHttpClient(server.Client()))}
	for i := 0; i < 2; i++ {
		id, err := p.SendReminder(context.Background(), "private-fixture", "fixed body", "fixed-uuid")
		if err != nil || id != "accepted-fixture" {
			t.Fatalf("send: %s %v", id, err)
		}
	}
	if len(bodies) != 2 || bodies[0]["uuid"] != "fixed-uuid" || bodies[0]["content"] != bodies[1]["content"] || bodies[0]["receive_id"] != "private-fixture" {
		t.Fatal("frozen payload changed")
	}
	if _, err := p.SendReminder(context.Background(), "other-chat", "fixed body", "fixed-uuid"); !errors.Is(err, core.ErrReminderPermission) || len(bodies) != 2 {
		t.Fatal("alternate destination accepted")
	}
	p.platformName, p.allowFrom, p.memberID = "feishu", "personal-open", "owner"
	principal := p.HandoffPrincipal()
	if principal.UserID != "personal-open" || principal.ChatID != p.allowChat || principal.SessionKey != "feishu:private-fixture:personal-open" {
		t.Fatal("physical handoff identity lost")
	}
	card := core.NewCard().Title("Handoff received", "blue").Build()
	if id, err := p.SendHandoffCard(context.Background(), principal, card, "handoff-uuid"); err != nil || id != "accepted-fixture" {
		t.Fatal("handoff receipt missing", id, err)
	}
	if len(bodies) != 3 || bodies[2]["uuid"] != "handoff-uuid" || bodies[2]["msg_type"] != "interactive" || bodies[2]["receive_id"] != p.allowChat {
		t.Fatal("handoff destination/uuid changed")
	}
	principal.UserID = "owner"
	if _, err := p.SendHandoffCard(context.Background(), principal, card, "handoff-uuid"); !errors.Is(err, core.ErrReminderPermission) || len(bodies) != 3 {
		t.Fatal("member alias accepted as physical handoff destination")
	}
	p.legacyPrivateChat = "old-private"
	if p.HandoffPrincipal().UserID != "" {
		t.Fatal("public group app can publish personal handoff cards")
	}
}

func TestHandoffReplyContextRetainsPhysicalCardForFileRoute(t *testing.T) {
	p := &Platform{platformName: "feishu", appID: "handoff-reply-test", sourceTenantKey: "tenant", memberID: "owner", strictRoutes: true, allowFrom: "personal-owner", allowChat: "personal-chat"}
	if err := p.Prepare(func(core.Platform, *core.Message) {}); err != nil {
		t.Fatal(err)
	}
	defer unregisterSharedWS(p)
	principal := p.HandoffPrincipal()
	principal.Project = "owner-private"
	principal.MessageID = "handoff-card-receipt"
	rc, err := p.HandoffReplyContext(principal)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := p.FileReplyRoute(rc)
	if err != nil {
		t.Fatal("handoff file route rejected", err)
	}
	var route fileReplyRoute
	if err := json.Unmarshal(raw, &route); err != nil {
		t.Fatal(err)
	}
	if route.MessageID != principal.MessageID || route.AppID != p.appID || route.SenderID != p.allowFrom || route.ChatID != p.allowChat {
		t.Fatal("handoff card physical route lost", route)
	}
	for _, bad := range []core.ActionPrincipal{{Platform: "feishu", UserID: "owner", ChatID: p.allowChat, SessionKey: principal.SessionKey, MessageID: principal.MessageID}, p.HandoffPrincipal()} {
		if _, err := p.HandoffReplyContext(bad); !errors.Is(err, core.ErrFileNotSubmitted) {
			t.Fatal("unbound handoff reply accepted")
		}
	}
}
