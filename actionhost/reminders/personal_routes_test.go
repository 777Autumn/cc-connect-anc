package reminders

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func TestMemberReminderKeepsPhysicalSourceAndUsesPersonalApp(t *testing.T) {
	h, s, now, _ := fixture(t)
	routes := []Route{
		{User: "personal-a", Chat: "personal-chat-a", Project: "ap", PrivateChat: "personal-chat-a", Member: "a", AppID: "app-a", PrivateProject: "ap", PrivateUser: "personal-a", PrivateAppID: "app-a"},
		{User: "a", Chat: "group", Project: "ag", PrivateChat: "personal-chat-a", Member: "a", AppID: "public", PrivateProject: "ap", PrivateUser: "personal-a", PrivateAppID: "app-a"},
		{User: "personal-b", Chat: "personal-chat-b", Project: "bp", PrivateChat: "personal-chat-b", Member: "b", AppID: "app-b", PrivateProject: "bp", PrivateUser: "personal-b", PrivateAppID: "app-b"},
		{User: "b", Chat: "group", Project: "bg", PrivateChat: "personal-chat-b", Member: "b", AppID: "public", PrivateProject: "bp", PrivateUser: "personal-b", PrivateAppID: "app-b"},
	}
	var checkedProject, checkedUser string
	newHost, err := New(h.store, routes, map[string]Sender{"a": s, "b": &senderStub{}}, func(project, user string) bool { checkedProject, checkedUser = project, user; return true })
	if err != nil {
		t.Fatal(err)
	}
	newHost.now = h.now
	h = newHost
	created := create(t, h, "ag", "cross-app-create", "private reminder")
	if created["status"] != "created" || checkedUser != "a" || checkedProject != "ag" {
		t.Fatal("source authorization changed", created)
	}
	p := core.ActionPrincipal{Platform: "feishu", UserID: "personal-a", ChatID: "personal-chat-a", Project: "ap", SessionKey: "personal-session", MessageID: "private-list"}
	listed, _, err := h.Tool(context.Background(), "reminder-list", json.RawMessage(`{}`), p, "", core.LangChinese)
	if err != nil || listed["status"] != "ok" {
		t.Fatal("personal sender cannot list member reminder", listed, err)
	}
	if rows, ok := listed["reminders"].([]map[string]any); !ok || len(rows) != 1 || rows[0]["content"] != "private reminder" {
		t.Fatal("member reminder absent from personal list", listed)
	}
	plan := planFixture(h, "ag", 1)
	if !h.crmRouteCurrent(plan) || plan.Principal.UserID != "a" || plan.Binding.PrivateUser != "personal-a" || plan.Binding.PrivateAppID != "app-a" {
		t.Fatal("CRM physical/source binding lost")
	}
	applyPlan(t, h, plan)
	currentBridge(h, &plan)
	*now = now.Add(2 * time.Hour)
	if err := h.Tick(context.Background(), core.LangChinese); err != nil {
		t.Fatal(err)
	}
	if len(s.calls) == 0 {
		t.Fatal("personal sender was not used")
	}
	for _, call := range s.calls {
		if call.chat != "personal-chat-a" {
			t.Fatal("sent to wrong app chat", call.chat)
		}
	}
	for _, field := range []string{"member", "app", "user", "project"} {
		bad := plan
		switch field {
		case "member":
			bad.Binding.Member = "b"
		case "app":
			bad.Binding.PrivateAppID = "app-b"
		case "user":
			bad.Binding.PrivateUser = "personal-b"
		case "project":
			bad.Binding.PrivateProject = "bp"
		}
		if h.crmRouteCurrent(bad) {
			t.Fatal("cross-app frozen binding was accepted", field)
		}
	}
	p.UserID = "a"
	p.MessageID = "forged-user"
	blocked, _, _ := h.Tool(context.Background(), "reminder-list", json.RawMessage(`{}`), p, "", core.LangChinese)
	if blocked["code"] != "invalid_identity" {
		t.Fatal("member alias accepted as physical sender")
	}
}
