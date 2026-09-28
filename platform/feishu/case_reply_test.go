package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
	lark "github.com/larksuite/oapi-sdk-go/v3"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
)

// Real Feishu callback/reply handling and Engine/SessionManager; only the remote
// API, model process and privileged action/file hosts are replaced.
func TestCaseConfirmationReplyContinuesOriginalWork(t *testing.T) {
	for _, answer := range []string{"askq:0:1", "askq:0:2"} {
		t.Run(answer, func(t *testing.T) {
			const key = "feishu:chat-fixture:sender-fixture"
			var replies atomic.Int32
			results := make(chan string, 8)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case strings.HasSuffix(r.URL.Path, "/tenant_access_token/internal"):
					writeJSON(t, w, map[string]any{"code": 0, "expire": 7200, "tenant_access_token": "fictional"})
				case strings.HasSuffix(r.URL.Path, "/reply"):
					var body map[string]any
					_ = json.NewDecoder(r.Body).Decode(&body)
					id := fmt.Sprintf("reply-%d", replies.Add(1))
					if strings.Contains(r.URL.Path, "/original-request/") {
						id = "question-card"
					}
					writeJSON(t, w, map[string]any{"code": 0, "data": map[string]any{"message_id": id}})
					if strings.Contains(fmt.Sprint(body["content"]), "fixture-result") {
						if !strings.Contains(r.URL.Path, "/question-card/") && !strings.Contains(r.URL.Path, "/follow-up/") {
							t.Error("reply target changed to a synthetic answer ID", r.URL.Path)
						}
						results <- id
					}
				case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/question-card"):
					writeJSON(t, w, map[string]any{"code": 0})
				case strings.Contains(r.URL.Path, "/reactions"):
					writeJSON(t, w, map[string]any{"code": 0, "data": map[string]any{"reaction_id": "fixture-reaction"}})
				default:
					t.Errorf("unexpected API request: %s %s", r.Method, r.URL.Path)
				}
			}))
			defer srv.Close()
			p := &interactivePlatform{Platform: &Platform{
				platformName: "feishu", domain: srv.URL, appID: t.Name(), strictRoutes: true,
				allowFrom: "sender-fixture", allowChat: "chat-fixture", botOpenID: "bot-fixture", dedup: &core.MessageDedup{},
				client: lark.NewClient(t.Name(), "fictional", lark.WithOpenBaseUrl(srv.URL), lark.WithHttpClient(srv.Client())),
			}}
			p.userNameCache.Store("sender-fixture", "Employee")
			p.chatNameCache.Store("chat-fixture", "Fixture group")
			agent := &caseReplyAgent{events: make(chan core.Event, 4), sends: make(chan string, 4)}
			e := core.NewEngine("fixture-project", agent, []core.Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), core.LangEnglish)
			defer e.Stop()
			if err := p.Prepare(e.ReceiveMessage); err != nil {
				t.Fatal(err)
			}
			s := e.GetSessions().GetOrCreateActive(key)
			principal := core.ActionPrincipal{Platform: "feishu", UserID: "sender-fixture", ChatID: "chat-fixture", SessionKey: key, Project: "fixture-project", MessageID: "original-request"}
			host := &caseReplyHost{t: t, principal: principal, root: t.TempDir(),
				ref:      core.FileWorkRef{WorkID: "fixture-work", SessionID: fmt.Sprintf("%s@%d", s.ID, s.CreatedAt.UnixNano())},
				messages: map[string]bool{"original-request": true}}
			e.SetActionHost(&caseReplyActionHost{host: host})
			e.SetSharedCasesEnabled(true)
			e.SetCaseConfirmationsEnabled(true)
			if err := e.SetFileWorkHost(host, func(id string) (string, int, error) {
				if id != host.ref.SessionID {
					t.Error("prepare changed original session", id)
				}
				return host.root, 0, nil
			}); err != nil {
				t.Fatal(err)
			}
			rc := replyContext{messageID: principal.MessageID, chatID: principal.ChatID, sessionKey: key, controlledFileWork: true}
			id, err := p.ReplyHostedActionPlaceholder(context.Background(), rc, core.NewCard().PlainText("Preparing").Build())
			if err != nil {
				t.Fatal(err)
			}
			q := &core.CaseQuestion{ID: "case:fixture", Command: "case-update", Input: json.RawMessage(`{"case_name":"Fixture case","expected_revision":0}`),
				Principal: principal, Context: core.CaseContext{WorkID: host.ref.WorkID}, Question: "Share fixture case?",
				Status: "awaiting", Receipt: id, AskedAtMs: time.Now().UnixMilli(), Expires: time.Now().Add(time.Minute)}
			s.CaseQuestion = q
			e.GetSessions().Save()
			card := core.NewCard().Buttons(core.CardButton{Text: "Confirm", Value: "askq:0:1"}, core.CardButton{Text: "Cancel", Value: "askq:0:2"}).Build()
			card.Interaction = &core.CardInteraction{RequestID: q.ID + ":0", Principal: principal}
			if err := p.RefreshCardMessage(context.Background(), id, key, card); err != nil {
				t.Fatal(err)
			}
			click := func(user, chat, receipt, action string) *callback.CardActionTriggerResponse {
				t.Helper()
				response, err := p.onCardAction(&callback.CardActionTriggerEvent{Event: &callback.CardActionTriggerRequest{
					Operator: &callback.Operator{OpenID: user}, Context: &callback.Context{OpenChatID: chat, OpenMessageID: receipt},
					Action: &callback.CallBackAction{Value: map[string]any{"action": action, "session_key": "forged"}},
				}})
				if err != nil {
					t.Fatal(err)
				}
				return response
			}
			for _, invalid := range [][4]string{{"other", principal.ChatID, id, answer}, {principal.UserID, "other-chat", id, answer}, {principal.UserID, principal.ChatID, "old-card", answer}, {principal.UserID, principal.ChatID, id, "askq:0:9"}} {
				if click(invalid[0], invalid[1], invalid[2], invalid[3]) != nil {
					t.Fatal("foreign or unbound confirmation accepted")
				}
			}
			if click(principal.UserID, principal.ChatID, id, answer) == nil {
				t.Fatal("bound answer rejected")
			}
			if click(principal.UserID, principal.ChatID, id, answer) != nil {
				t.Fatal("duplicate confirmation accepted")
			}
			if answer == "askq:0:2" {
				deadline := time.Now().Add(3 * time.Second)
				for replies.Load() < 2 && time.Now().Before(deadline) {
					time.Sleep(time.Millisecond)
				}
				if replies.Load() != 2 || host.calls.Load() != 0 || len(agent.sends) != 0 {
					t.Fatal("cancellation executed business/model work, or was not acknowledged")
				}
				return
			}
			receipt := awaitCaseReply(t, results)
			awaitCaseIdle(t, s)
			if ref, err := host.FindByMessage(context.Background(), principal, receipt); err != nil || ref != host.ref {
				t.Fatalf("confirmed continuation reply was not registered to original work: receipt=%s ref=%+v err=%v", receipt, ref, err)
			}
			// A real unmentioned group reply must now resume that same work, not
			// create another session or repeat the already-confirmed operation.
			if err := p.onMessage(context.Background(), fileWorkEvent("follow-up", "text", `{"text":"Continue the spreadsheet only"}`, "group", receipt, false)); err != nil {
				t.Fatal(err)
			}
			awaitCaseReply(t, results)
			awaitCaseIdle(t, s)
			if host.calls.Load() != 1 || len(agent.sends) != 2 || e.GetSessions().ActiveSessionID(key) != s.ID || len(e.GetSessions().ListSessions(key)) != 1 {
				t.Fatal("follow-up changed work/session or repeated the confirmed operation")
			}
			if first, second := <-agent.sends, <-agent.sends; first != q.ID || second != "follow-up" {
				t.Fatal("engine lost trusted answer or follow-up identity", first, second)
			}
		})
	}
}

func awaitCaseIdle(t *testing.T, s *core.Session) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for s.Busy() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if s.Busy() {
		t.Fatal("continued turn did not finish")
	}
}

func awaitCaseReply(t *testing.T, replies <-chan string) string {
	t.Helper()
	select {
	case id := <-replies:
		return id
	case <-time.After(5 * time.Second):
		t.Fatal("continued result was not sent")
		return ""
	}
}

type caseReplyAgent struct {
	core.Agent
	core.AgentSession
	events chan core.Event
	sends  chan string
	closed atomic.Bool
}

func (a *caseReplyAgent) Name() string                           { return "fixture" }
func (a *caseReplyAgent) Stop() error                            { return a.Close() }
func (a *caseReplyAgent) ForFileWork(string) (core.Agent, error) { return a, nil }
func (a *caseReplyAgent) StartSession(context.Context, string) (core.AgentSession, error) {
	return a, nil
}
func (a *caseReplyAgent) Send(_ string, id string, _ []core.ImageAttachment, _ []core.FileAttachment) error {
	a.sends <- id
	a.events <- core.Event{Type: core.EventResult, Content: "fixture-result", Done: true}
	return nil
}
func (a *caseReplyAgent) Events() <-chan core.Event { return a.events }
func (a *caseReplyAgent) CurrentSessionID() string  { return "fixture-agent-session" }
func (a *caseReplyAgent) Alive() bool               { return !a.closed.Load() }
func (a *caseReplyAgent) Close() error              { a.closed.Store(true); return nil }

// The privileged file-host boundary stores exact receipt membership; unknown
// IDs fail closed, matching the production FindByMessage/RecordReply contract.
type caseReplyHost struct {
	core.FileWorkHost
	t         *testing.T
	principal core.ActionPrincipal
	root      string
	ref       core.FileWorkRef
	mu        sync.Mutex
	messages  map[string]bool
	calls     atomic.Int32
}

type caseReplyActionHost struct {
	core.ActionHost
	host *caseReplyHost
}

func (a *caseReplyActionHost) Kind() string { return "fixture" }
func (a *caseReplyActionHost) Tool(ctx context.Context, command string, _ json.RawMessage, p core.ActionPrincipal, _ string, _ core.Language) (map[string]any, *core.ActionHostResult, error) {
	h := a.host
	h.calls.Add(1)
	binding, ok := core.TrustedCaseContext(ctx)
	if command != "case-update" || p != h.principal || !ok || binding.WorkID != h.ref.WorkID || binding.Confirmation == nil || binding.Confirmation.AnswerMessageID != "case:fixture" {
		h.t.Error("business confirmation lost its original principal/work/answer", command, p, binding)
	}
	return map[string]any{"status": "recorded"}, nil, nil
}
func (h *caseReplyHost) sameScope(p core.ActionPrincipal) bool {
	p.MessageID = h.principal.MessageID
	return p == h.principal
}
func (h *caseReplyHost) FindByMessage(_ context.Context, p core.ActionPrincipal, id string) (core.FileWorkRef, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.sameScope(p) || !h.messages[id] {
		return core.FileWorkRef{}, core.ErrFileWorkNotFound
	}
	return h.ref, nil
}
func (h *caseReplyHost) RecordReply(_ context.Context, p core.ActionPrincipal, id string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.sameScope(p) || !h.messages[p.MessageID] {
		return core.ErrFileWorkNotFound
	}
	h.messages[id] = true
	return nil
}
func (h *caseReplyHost) Bind(_ context.Context, b core.FileWorkBinding) (core.FileWorkContext, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.sameScope(b.Principal) || b.SessionID != h.ref.SessionID || b.WorkRoot != h.root || !b.Group || b.SourceReceipt != "" {
		h.t.Error("continuation changed original work or recipient scope", b)
		return core.FileWorkContext{}, core.ErrFileWorkNotFound
	}
	h.messages[b.Principal.MessageID] = true
	return core.FileWorkContext{Enabled: true, WorkID: h.ref.WorkID, WorkRoot: h.root}, nil
}
