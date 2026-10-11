package feishu

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chenhg5/cc-connect/core"
	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkevent "github.com/larksuite/oapi-sdk-go/v3/event"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

func TestPersonalRouteChecksActualSourceBeforeAttachmentsAndCards(t *testing.T) {
	p := &Platform{platformName: "feishu", appID: "personal-source-check", sourceTenantKey: "tenant", memberID: "owner", strictRoutes: true, allowFrom: "physical-owner", allowChat: "private", dedup: &core.MessageDedup{}}
	if err := p.Prepare(func(core.Platform, *core.Message) { t.Error("rejected source reached core") }); err != nil {
		t.Fatal(err)
	}
	defer unregisterSharedWS(p)
	p.trustedCardActionHandler = func(core.TrustedCardAction) core.TrustedCardActionResponse {
		t.Error("rejected callback reached core")
		return core.TrustedCardActionResponse{}
	}
	for _, bad := range []string{"header-app", "header-tenant", "sender-tenant", "sender", "chat", "missing-header", "sender-type", "group-in-personal-chat", "missing-open-id"} {
		t.Run(bad, func(t *testing.T) {
			app, tenant, actorTenant, actor, chat, kind, senderType, content, id := p.appID, "tenant", "tenant", "physical-owner", "private", "file", "user", `{"file_key":"never-download"}`, bad
			chatType := "p2p"
			header := &larkevent.EventV2Base{Header: &larkevent.EventHeader{AppID: app, TenantKey: tenant}}
			switch bad {
			case "header-app":
				header.Header.AppID = "other"
			case "header-tenant":
				header.Header.TenantKey = "other"
			case "sender-tenant":
				actorTenant = "other"
			case "sender":
				actor = "other"
			case "chat":
				chat = "other"
			case "missing-header":
				header = nil
			case "sender-type":
				senderType = "app"
			case "group-in-personal-chat":
				chatType = "group"
			}
			event := &larkim.P2MessageReceiveV1{EventV2Base: header, Event: &larkim.P2MessageReceiveV1Data{Sender: &larkim.EventSender{SenderId: &larkim.UserId{OpenId: &actor}, TenantKey: &actorTenant, SenderType: &senderType}, Message: &larkim.EventMessage{ChatId: &chat, ChatType: &chatType, MessageType: &kind, Content: &content, MessageId: &id}}}
			if bad == "missing-open-id" {
				event.Event.Sender.SenderId = &larkim.UserId{UserId: &actor}
			}
			if err := p.onMessage(context.Background(), event); err != nil {
				t.Fatal(err)
			}
			if bad == "sender-type" || bad == "group-in-personal-chat" || bad == "missing-open-id" {
				return
			}
			_, err := p.onCardAction(&callback.CardActionTriggerEvent{EventV2Base: header, Event: &callback.CardActionTriggerRequest{Operator: &callback.Operator{OpenID: actor, TenantKey: &actorTenant}, Context: &callback.Context{OpenChatID: chat, OpenMessageID: id}, Action: &callback.CallBackAction{Value: map[string]any{"kind": "fixture", "approval_id": "never-claim"}}}})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
	var received core.ActionPrincipal
	p.trustedCardActionHandler = func(action core.TrustedCardAction) core.TrustedCardActionResponse {
		received = action.Principal
		return core.TrustedCardActionResponse{}
	}
	tenant := "tenant"
	_, err := p.onCardAction(&callback.CardActionTriggerEvent{EventV2Base: &larkevent.EventV2Base{Header: &larkevent.EventHeader{AppID: p.appID, TenantKey: tenant}}, Event: &callback.CardActionTriggerRequest{Operator: &callback.Operator{OpenID: p.allowFrom, TenantKey: &tenant}, Context: &callback.Context{OpenChatID: p.allowChat, OpenMessageID: "valid-card"}, Action: &callback.CallBackAction{Value: map[string]any{"kind": "fixture", "approval_id": "current"}}}})
	if err != nil || received.UserID != p.allowFrom || received.UserID == p.memberID || received.ChatID != p.allowChat {
		t.Fatal("accepted callback lost physical identity", received, err)
	}
}

func TestLegacyPrivateOnlySendsFixedRedirectAndNeverLoadsAttachments(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "tenant_access_token") {
			writeJSON(t, w, map[string]any{"code": 0, "expire": 7200, "tenant_access_token": "fixture-redirect-token"})
			return
		}
		if r.URL.Path != "/open-apis/im/v1/messages" {
			t.Error("attachment resource or unexpected request", r.URL.Path)
			w.WriteHeader(400)
			return
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body["receive_id"] != "legacy-private" || body["content"] != `{"text":"Open the owner personal bot."}` {
			t.Error("redirect destination or fixed content changed", body)
		}
		requests++
		writeJSON(t, w, map[string]any{"code": 0, "data": map[string]any{"message_id": "redirect-receipt"}})
	}))
	defer server.Close()
	p := &Platform{platformName: "feishu", appID: "redirect-public-app", sourceTenantKey: "tenant", memberID: "owner", strictRoutes: true, allowFrom: "public-owner", allowChat: "group", legacyPrivateChat: "legacy-private", legacyPrivateRedirect: "Open the owner personal bot.", dedup: &core.MessageDedup{}, client: lark.NewClient("redirect-public-app", "fixture", lark.WithOpenBaseUrl(server.URL), lark.WithHttpClient(server.Client()))}
	if err := p.Prepare(func(core.Platform, *core.Message) { t.Error("old private entered model environment") }); err != nil {
		t.Fatal(err)
	}
	defer unregisterSharedWS(p)
	for _, kind := range []string{"text", "file", "image", "audio"} {
		actor, tenant, chat, senderType, chatType, content := p.allowFrom, "tenant", "legacy-private", "user", "p2p", `{"file_key":"never-download"}`
		event := &larkim.P2MessageReceiveV1{EventV2Base: &larkevent.EventV2Base{Header: &larkevent.EventHeader{AppID: p.appID, TenantKey: tenant}}, Event: &larkim.P2MessageReceiveV1Data{Sender: &larkim.EventSender{SenderId: &larkim.UserId{OpenId: &actor}, TenantKey: &tenant, SenderType: &senderType}, Message: &larkim.EventMessage{ChatId: &chat, ChatType: &chatType, MessageType: &kind, Content: &content, MessageId: &kind}}}
		if err := p.onMessage(context.Background(), event); err != nil {
			t.Fatal(err)
		}
		actor = "other"
		if err := p.onMessage(context.Background(), event); err != nil {
			t.Fatal(err)
		}
	}
	if requests != 4 {
		t.Fatalf("got %d redirects", requests)
	}
	if p.acceptFixedRoute(p.allowFrom, p.legacyPrivateChat) {
		t.Fatal("old cards accepted")
	}
}
