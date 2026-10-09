package core

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type slowStartingProgressAgent struct {
	stubAgent
	entered chan struct{}
	release chan struct{}
	session *cujAgentSession
	err     error
}

func (a *slowStartingProgressAgent) StartSession(ctx context.Context, _ string) (AgentSession, error) {
	close(a.entered)
	select {
	case <-a.release:
		return a.session, a.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestTaskProgressDuringColdStart(t *testing.T) {
	t.Parallel()
	p := &cujStreamingPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	s := newCUJAgentSession()
	s.pendingEvents = []Event{}
	a := &slowStartingProgressAgent{entered: make(chan struct{}), release: make(chan struct{}), session: s}
	e := NewEngine("test", a, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangEnglish)
	e.SetReplyFooterEnabled(false)
	e.SetDisplayConfig(DisplayCfg{Mode: "quiet", FinalResponseOnly: true, TaskProgress: true})
	defer e.Stop()
	var once sync.Once
	release := func() { once.Do(func() { close(a.release) }) }
	defer release()
	env := &cujEnv{t: t, engine: e, plat: &p.stubPlatformEngine}
	e.ReceiveMessage(p, &Message{SessionKey: "test:owner", Platform: "test", UserID: "owner", MessageID: "cold-start", Content: "perform a long task", ReplyCtx: "owner"})
	select {
	case <-a.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("StartSession was not entered")
	}
	time.Sleep(31 * time.Second)
	duringStart := len(p.getPreviewOpens())
	t.Logf("31 seconds inside StartSession: status messages = %d", duringStart)
	release()
	env.waitFor("first status after startup release", 2*time.Second, func() bool { return len(p.getPreviewOpens()) == 1 })
	t.Logf("after StartSession returned: status messages = %d", len(p.getPreviewOpens()))
	env.waitFor("agent received the prompt", 2*time.Second, func() bool { return len(s.getSentPrompts()) == 1 })
	s.events <- Event{Type: EventResult, Content: "finished", Done: true}
	env.waitFor("turn completed", 2*time.Second, func() bool { return !e.sessions.GetOrCreateActive("test:owner").Busy() })
	if duringStart == 0 {
		t.Error("a running turn exceeded 30 seconds with no status while StartSession blocked")
	}
}

func TestTaskProgressColdStartFailureEndsOriginalStatus(t *testing.T) {
	t.Parallel()
	p := &taskStatusPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	a := &slowStartingProgressAgent{entered: make(chan struct{}), release: make(chan struct{}), err: errors.New("startup failure")}
	e := NewEngine("test", a, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangEnglish)
	e.SetDisplayConfig(DisplayCfg{FinalResponseOnly: true, TaskProgress: true})
	defer e.Stop()
	var once sync.Once
	release := func() { once.Do(func() { close(a.release) }) }
	defer release()
	e.ReceiveMessage(p, &Message{SessionKey: "test:owner", Platform: "test", UserID: "owner", MessageID: "failed-start", Content: "start a task", ReplyCtx: "owner/failed-start"})
	select {
	case <-a.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("StartSession was not entered")
	}
	env := &cujEnv{t: t, engine: e, plat: &p.stubPlatformEngine}
	env.waitFor("status while startup is blocked", 33*time.Second, func() bool { return len(p.snapshotCalls()) == 1 })
	release()
	env.waitFor("startup failure terminal", 3*time.Second, func() bool { return len(p.snapshotCalls()) == 2 })
	calls := p.snapshotCalls()
	if !calls[0].create || calls[1].create || calls[1].text != NewI18n(LangEnglish).T(MsgTaskInterrupted) || calls[1].route != "owner/failed-start" {
		t.Fatalf("wrong startup failure status: %+v", calls)
	}
	env.waitFor("failed turn released", time.Second, func() bool { return !e.sessions.GetOrCreateActive("test:owner").Busy() })
}
