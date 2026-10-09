package core

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func (p *cujStreamingPlatform) StartTaskProgress(ctx context.Context, route any, content string) (any, error) {
	return p.SendPreviewStart(ctx, route, content)
}

func (p *cujStreamingPlatform) UpdateTaskProgress(ctx context.Context, handle any, content string) error {
	return p.UpdateMessage(ctx, handle, content)
}

func TestCUJ_I4_TaskProgressKeepsFinalOnlyAndQueuedRoutes(t *testing.T) {
	env, s, send := newFinalDisplayJourney(t)
	env.engine.SetDisplayConfig(DisplayCfg{Mode: "quiet", FinalResponseOnly: true, TaskProgress: true})
	send("first long action")
	env.waitFor("agent started", time.Second, func() bool { return len(s.getSentPrompts()) == 1 })
	s.events <- Event{Type: EventThinking, Content: "PRIVATE_THOUGHT"}
	s.events <- Event{Type: EventText, Content: "PRIVATE_NARRATION"}
	s.events <- Event{Type: EventToolUse, ToolName: "PRIVATE_COMMAND", ToolInput: "PRIVATE_PATH"}
	s.events <- Event{Type: EventToolResult, ToolResult: "PRIVATE_RESULT"}
	s.events <- Event{Type: EventResult, Content: "PRIVATE_PARTIAL", Done: false}
	env.waitFor("30-second host status", 35*time.Second, func() bool { return len(env.streamingPlat().getPreviewOpens()) == 1 })
	if got := env.streamingPlat().getPreviewOpens()[0]; got != "Tool activity received; processing continues." {
		t.Fatalf("unsafe initial status: %q", got)
	}
	send("second queued action")
	s.events <- Event{Type: EventResult, Content: "first answer", Done: true}
	env.waitFor("queued send", time.Second, func() bool { return len(s.getSentPrompts()) == 2 })
	s.events <- Event{Type: EventResult, Content: "second answer", Done: true}
	env.waitFor("queued answer", time.Second, func() bool {
		return env.sentContains("second answer") && !env.engine.sessions.GetOrCreateActive("test:owner").Busy()
	})
	send("third short action")
	env.waitFor("third send", time.Second, func() bool { return len(s.getSentPrompts()) == 3 })
	s.events <- Event{Type: EventError, Error: errors.New("synthetic failed turn")}
	env.waitFor("third error", time.Second, func() bool { return env.sentContains("synthetic failed turn") })
	if len(env.streamingPlat().getPreviewOpens()) != 1 {
		t.Fatal("short/queued work created another card")
	}
	env.waitFor("first card terminal", time.Second, func() bool { return len(env.streamingPlat().getPreviewUpdates()) > 0 })
	for _, update := range env.streamingPlat().getPreviewUpdates() {
		if !strings.Contains(update.Content, "This turn has ended") {
			t.Fatalf("queued/failed turn overwrote earlier card: %+v", update)
		}
	}
	if strings.Contains(strings.Join(env.plat.getSent(), "\n"), "PRIVATE_") {
		t.Fatal("model content leaked")
	}
}

// Manual events model the external process boundary; all user turns still go
// through ReceiveMessage and assertions observe the actual platform output.
func newFinalDisplayJourney(t *testing.T) (*cujEnv, *cujAgentSession, func(string)) {
	t.Helper()
	p := &cujStreamingPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	s := newCUJAgentSession()
	s.pendingEvents = []Event{}
	e := NewEngine("test", &controllableAgent{nextSession: s}, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangEnglish)
	e.SetReplyFooterEnabled(false)
	t.Cleanup(func() { _ = e.Stop() })
	env := &cujEnv{t: t, engine: e, plat: &p.stubPlatformEngine}
	send := func(content string) {
		t.Helper()
		s.mu.Lock()
		s.pendingEvents = []Event{}
		s.mu.Unlock()
		e.ReceiveMessage(p, &Message{SessionKey: "test:owner", Platform: "test", UserID: "owner", MessageID: content, Content: content, ReplyCtx: "owner"})
	}
	return env, s, send
}

func TestFinalResponseOnlyUnexpectedExit(t *testing.T) {
	env, s, send := newFinalDisplayJourney(t)
	env.engine.SetDisplayConfig(DisplayCfg{Mode: "quiet", FinalResponseOnly: true})
	send("create my reminder")
	env.waitFor("agent started", time.Second, func() bool { return len(s.getSentPrompts()) == 1 })
	s.events <- Event{Type: EventText, Content: "I will create it now."}
	s.events <- Event{Type: EventToolUse, ToolName: "Bash"}
	close(s.events) // abrupt process exit without a terminal result
	env.waitFor("interrupted result", time.Second, func() bool { return len(env.plat.getSent()) > 0 })
	got := strings.Join(env.plat.getSent(), "\n")
	if strings.Contains(got, "I will") || !strings.Contains(got, "not verified") {
		t.Fatalf("unexpected exit exposed a promise instead of the unknown outcome: %q", got)
	}
}

func TestFinalResponseOnlyBackgroundResults(t *testing.T) {
	env, s, send := newFinalDisplayJourney(t)
	env.engine.SetDisplayConfig(DisplayCfg{Mode: "quiet", FinalResponseOnly: true, HideAgentFooter: true})
	send("start work")
	env.waitFor("agent started", time.Second, func() bool { return len(s.getSentPrompts()) == 1 })
	s.events <- Event{Type: EventResult, Content: "started", Done: true}
	env.waitFor("foreground complete", time.Second, func() bool {
		return env.sentContains("started") && !env.engine.sessions.GetOrCreateActive("test:owner").Busy()
	})
	for i, tc := range []struct {
		result string
		want   string
	}{
		{"已完成。\nclaude-sonnet-5 · out 10 · in 2 · ctx 23%", "已完成。"},
		{"", "safe fallback"},
	} {
		s.events <- Event{Type: EventText, Content: "Internal next_id bookkeeping"}
		s.events <- Event{Type: EventResult, Content: "Internal partial result", Done: false}
		s.events <- Event{Type: EventToolUse, ToolName: "Bash"}
		s.events <- Event{Type: EventText, Content: "safe fallback"}
		s.events <- Event{Type: EventResult, Content: tc.result, Done: true}
		env.waitFor("background final", time.Second, func() bool { return env.sentContains(tc.want) })
		got := env.plat.getSent()
		if len(got) != i+2 || strings.TrimSpace(got[i+1]) != tc.want {
			t.Fatalf("background turn leaked intermediate text/footer: %q", got)
		}
	}
	s.events <- Event{Type: EventText, Content: "Internal preparation"}
	s.events <- Event{Type: EventToolUse, ToolName: "Bash"}
	close(s.events)
	env.waitFor("background interrupted result", time.Second, func() bool { return env.sentContains("not verified") })
	if got := strings.Join(env.plat.getSent(), "\n"); strings.Contains(got, "Internal") {
		t.Fatalf("background exit leaked narration: %q", got)
	}
}

func TestCUJ_I3_FinalOnlyReloadBeforeQueuedTurn(t *testing.T) {
	env, s, send := newFinalDisplayJourney(t)
	env.engine.SetDisplayConfig(DisplayCfg{Mode: "quiet"})
	send("first action")
	env.waitFor("agent started", time.Second, func() bool { return len(s.getSentPrompts()) == 1 })
	send("second action") // queued while the first turn is still awaiting events
	env.engine.SetDisplayConfig(DisplayCfg{Mode: "quiet", FinalResponseOnly: true})
	s.events <- Event{Type: EventResult, Content: "first complete", Done: true}
	env.waitFor("queued send", time.Second, func() bool { return len(s.getSentPrompts()) == 2 })
	before := len(env.streamingPlat().getPreviewOpens())
	s.events <- Event{Type: EventText, Content: strings.Repeat("Internal processing ", 30)}
	s.events <- Event{Type: EventToolUse, ToolName: "Bash"}
	s.events <- Event{Type: EventText, Content: "second complete"}
	s.events <- Event{Type: EventResult, Content: "second complete", Done: true}
	env.waitFor("queued result", time.Second, func() bool { return !env.engine.sessions.GetOrCreateActive("test:owner").Busy() })
	if len(env.streamingPlat().getPreviewOpens()) != before || !env.sentContains("second complete") {
		t.Fatalf("queued turn used stale preview settings: previews=%q sent=%q", env.streamingPlat().getPreviewOpens(), env.plat.getSent())
	}
	send("third action")
	env.waitFor("next foreground send", time.Second, func() bool { return len(s.getSentPrompts()) == 3 })
	s.events <- Event{Type: EventText, Content: strings.Repeat("Internal processing ", 30)}
	s.events <- Event{Type: EventToolUse, ToolName: "Bash"}
	s.events <- Event{Type: EventResult, Content: "third complete", Done: true}
	env.waitFor("next foreground result", time.Second, func() bool { return env.sentContains("third complete") })
	if len(env.streamingPlat().getPreviewOpens()) != before {
		t.Fatal("new foreground turn did not preserve the reloaded final-only setting")
	}
}
