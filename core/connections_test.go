package core

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestPersonalConnectionConfirmationTakesConnectionLockBeforeQuestionLock(t *testing.T) {
	e, _, p, state, s := caseQuestionFixture(t)
	q := askFixtureCaseQuestion(t, e, state, s)
	e.SetConnectionHost(&connectionHostFixture{})
	// Hold the inner lock: the answer must reserve the outer lock while waiting.
	e.caseConfirmationMu.Lock()
	answerDone := make(chan struct{})
	go func() {
		m := answerFixture(q, "取消", true)
		e.handleCaseAnswer(p, m, m.Content)
		close(answerDone)
	}()
	deadline := time.After(2 * time.Second)
	for e.connectionToolsMu.TryLock() {
		e.connectionToolsMu.Unlock()
		select {
		case <-deadline:
			e.caseConfirmationMu.Unlock()
			<-answerDone
			t.Fatal("answer waited for confirmation without first reserving connection scope")
		default:
			runtime.Gosched()
		}
	}
	toolDone := make(chan struct{})
	go func() {
		actionToolRequest(e.ActionToolHandler(), "POST", "/tool", state.actionToken, questionToolRequest)
		close(toolDone)
	}()
	e.caseConfirmationMu.Unlock()
	for _, done := range []chan struct{}{answerDone, toolDone} {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("confirmation and tool dispatch deadlocked")
		}
	}
}

type connectionHostFixture struct {
	result     map[string]any
	principal  ActionPrincipal
	guardError error
	guardCalls int
}

func (h *connectionHostFixture) ConnectionOperation(text string) string {
	switch text {
	case "connect my Feishu":
		return "connect"
	case "connection status":
		return "status"
	case "disconnect Feishu":
		return "disconnect"
	}
	return ""
}
func (h *connectionHostFixture) ConnectionCommand(_ context.Context, _ string, p ActionPrincipal) (map[string]any, error) {
	h.principal = p
	return h.result, nil
}
func (h *connectionHostFixture) ConnectionGuard(_ context.Context, _ string, p ActionPrincipal, _, _, _ string) error {
	h.guardCalls++
	h.principal = p
	return h.guardError
}

func TestPersonalConnectionGuardPrecedesEveryToolAndFileDispatch(t *testing.T) {
	e, tools, _, state := actionToolFixture(t)
	h := &connectionHostFixture{guardError: errors.New("fixture private work")}
	e.SetConnectionHost(h)
	for _, command := range []string{"customer", "customers", "stage", "result", "knowledge_propose", "case-publish", "file-deliver", "work-context"} {
		w := actionToolRequest(e.ActionToolHandler(), "POST", "/tool", state.actionToken, `{"command":"`+command+`","input":{}}`)
		if w.Code != 403 || tools.toolCalls != 0 || h.principal != state.currentPrincipal {
			t.Fatalf("scope bypass for %s: %d %s", command, w.Code, w.Body.String())
		}
	}
	h.guardError = nil
	w := actionToolRequest(e.ActionToolHandler(), "POST", "/tool", state.actionToken, `{"command":"customer","input":{}}`)
	if w.Code != 200 || tools.toolCalls != 1 {
		t.Fatalf("normal read blocked: %s", w.Body.String())
	}
}

func TestPersonalConnectionInvalidationKeepsOtherUsersAndGroupRunning(t *testing.T) {
	e, _, _, owner := actionToolFixture(t)
	e.SetConnectionHost(&connectionHostFixture{guardError: errors.New("expired")})
	owner.filePrivate = true
	group := &interactiveState{agentSession: newCUJAgentSession(), currentPrincipal: owner.currentPrincipal}
	other := &interactiveState{agentSession: newCUJAgentSession(), currentPrincipal: ActionPrincipal{UserID: "other"}, filePrivate: true}
	e.interactiveStates["group"] = group
	e.interactiveStates["other"] = other
	e.InvalidateConnection("owner")
	if e.interactiveStates[owner.currentPrincipal.SessionKey] != nil || e.interactiveStates["group"] != group || e.interactiveStates["other"] != other {
		t.Fatal("connection failure escaped its personal scope")
	}
}

func TestPersonalConnectionTemporaryFailurePreservesActiveWork(t *testing.T) {
	e, _, _, owner := actionToolFixture(t)
	h := &connectionHostFixture{guardError: ErrConnectionTemporary}
	e.SetConnectionHost(h)
	owner.filePrivate = true
	owner.pendingMessages = []queuedMessage{{content: "fictional queued work", platform: owner.platform, replyCtx: owner.replyCtx}}
	e.InvalidateConnection("owner")
	if e.interactiveStates[owner.currentPrincipal.SessionKey] != owner || len(owner.pendingMessages) != 1 {
		t.Fatal("temporary verification failure discarded active private work")
	}
	// The same preserved work must still stop on confirmed loss of its binding.
	h.guardError = errors.New("expired")
	e.InvalidateConnection("owner")
	if e.interactiveStates[owner.currentPrincipal.SessionKey] != nil {
		t.Fatal("confirmed revocation retained private work")
	}
}

func TestPersonalConnectionPublicationCannotBypassToolGate(t *testing.T) {
	e, _, _, state := actionToolFixture(t)
	h := &connectionHostFixture{guardError: errors.New("private")}
	e.SetConnectionHost(h)
	_, err := e.selectedCaseFiles(context.Background(), state.currentPrincipal, "private-work", nil)
	if !errors.Is(err, errConnectionWorkUnavailable) || h.guardCalls != 1 {
		t.Fatal("publication bypass")
	}
}

func TestCUJ_CONNECTION1_ConnectStatusDisconnectWithoutModel(t *testing.T) {
	env := newCUJEnv(t)
	h := &connectionHostFixture{}
	env.engine.SetConnectionHost(h)
	send := func(text string, result map[string]any, want string) {
		t.Helper()
		h.result = result
		env.plat.clearSent()
		env.engine.ReceiveMessage(env.plat, &Message{SessionKey: "test:private:owner", Platform: "test", UserID: "owner", ChannelID: "private", MessageID: text, Content: text, ReplyCtx: "reply"})
		env.waitFor(text, 2*time.Second, func() bool { return env.sentContains(want) })
		if h.principal.UserID != "owner" || h.principal.ChatID != "private" {
			t.Fatal("identity lost")
		}
	}
	send("connect my Feishu", map[string]any{"status": "pending", "audience": "private", "reset": true, "verification_url": "https://accounts.feishu.cn/fixture"}, "https://accounts.feishu.cn/fixture")
	send("connection status", map[string]any{"status": "connected", "audience": "private", "reset": true}, "connection is ready")
	send("disconnect Feishu", map[string]any{"status": "disconnected", "audience": "private", "reset": true}, "stopped using")
	env.agent.mu.Lock()
	defer env.agent.mu.Unlock()
	if len(env.agent.sessions) != 0 {
		t.Fatal("enrollment reached the model")
	}
	if strings.Contains(strings.Join(env.plat.getSent(), "\n"), "profile") {
		t.Fatal("internal profile leaked")
	}
}
