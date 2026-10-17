package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The recipient sees the message even though the adapter loses its receipt.
type acceptedUnknownPlatform struct{ stubPlatformEngine }

func (p *acceptedUnknownPlatform) Send(ctx context.Context, rc any, text string) error {
	_ = p.stubPlatformEngine.Send(ctx, rc, text)
	return ErrMessageDeliveryUnconfirmed
}

type receiptLostPreview struct {
	*cujStreamingPlatform
	loseReceipt   bool
	doneReactions int
}

func (p *receiptLostPreview) SendPreviewStart(_ context.Context, _ any, text string) (any, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.previewOpens = append(p.previewOpens, text)
	if p.loseReceipt {
		p.loseReceipt = false
		return nil, ErrMessageDeliveryUnconfirmed
	}
	return fmt.Sprintf("receipt-%d", len(p.previewOpens)), nil
}

func (p *receiptLostPreview) AddDoneReaction(any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.doneReactions++
}

func (p *receiptLostPreview) SendAudio(ctx context.Context, rc any, _ []byte, _ string) error {
	return p.Send(ctx, rc, "answer-audio")
}

func newReceiptLostPreview() *receiptLostPreview {
	return &receiptLostPreview{
		cujStreamingPlatform: &cujStreamingPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}},
		loseReceipt:          true,
	}
}

func TestMessageDeliveryUnconfirmedCompactSuppressesOnlyCurrentEvent(t *testing.T) {
	p := newReceiptLostPreview()
	w := newCompactProgressWriter(context.Background(), p, progressHintReplyCtx{style: progressStyleCompact}, "agent", LangEnglish, nil)
	if !w.Append("first event") {
		_ = p.Send(context.Background(), "route", "first event")
	}
	if !w.Append("second independent event") {
		_ = p.Send(context.Background(), "route", "second independent event")
	}
	if len(p.getPreviewOpens()) != 1 || len(p.getSent()) != 1 || p.getSent()[0] != "second independent event" {
		t.Fatalf("unknown progress was replayed or next event lost: previews=%v sent=%v", p.getPreviewOpens(), p.getSent())
	}
	if w.Finalize(ProgressCardStateCompleted) || len(p.getPreviewUpdates()) != 0 {
		t.Fatal("unknown progress was treated as a confirmed editable message")
	}
}

func TestMessageDeliveryUnconfirmedStreamCannotUnfreezeIntoReplacement(t *testing.T) {
	p := newReceiptLostPreview()
	sp := newStreamPreview(StreamPreviewCfg{Enabled: true, MinDeltaChars: 1}, p, "route", context.Background(), nil)
	sp.appendText("accepted first answer")
	sp.freeze()
	sp.detachPreview()
	sp.unfreeze()
	sp.appendText("continuation of same answer")
	confirmed := sp.finish("complete answer", "")
	sp.discard()
	if confirmed || len(p.getPreviewOpens()) != 1 || len(p.getPreviewUpdates()) != 0 {
		t.Fatalf("unknown stream recreated or claimed delivery: confirmed=%v starts=%v updates=%v", confirmed, p.getPreviewOpens(), p.getPreviewUpdates())
	}
	next := newStreamPreview(StreamPreviewCfg{Enabled: true, MinDeltaChars: 1}, p, "route", context.Background(), nil)
	next.appendText("independent next answer")
	if !next.finish("independent next answer", "") || len(p.getPreviewOpens()) != 2 {
		t.Fatal("next turn was blocked by previous unknown delivery")
	}
}

// Some platforms edit their initial Send using the reply context rather than
// exposing PreviewStarter. Unknown delivery has the same semantics there.
type unknownEditableSend struct {
	acceptedUnknownPlatform
	updates int
}

func (p *unknownEditableSend) UpdateMessage(context.Context, any, string) error {
	p.updates++
	return nil
}

func TestMessageDeliveryUnconfirmedInitialSendWithoutPreviewStarter(t *testing.T) {
	p := &unknownEditableSend{acceptedUnknownPlatform: acceptedUnknownPlatform{stubPlatformEngine{n: "test"}}}
	w := newCompactProgressWriter(context.Background(), p, progressHintReplyCtx{style: progressStyleCompact}, "agent", LangEnglish, nil)
	if !w.Append("progress-one") || w.Append("progress-two") {
		t.Fatal("current event must suppress fallback while independent next event may fall back")
	}
	sp := newStreamPreview(StreamPreviewCfg{Enabled: true, MinDeltaChars: 1}, p, "route", context.Background(), nil)
	sp.appendText("answer-first")
	sp.unfreeze()
	sp.appendText("answer-second")
	if sp.finish("answer-final", "") || len(p.getSent()) != 2 || p.updates != 0 {
		t.Fatalf("unknown Send recreated or updated: sent=%v updates=%d", p.getSent(), p.updates)
	}
}

func (p *acceptedUnknownPlatform) SendWithStatusFooter(ctx context.Context, rc any, text, footer string) error {
	return p.Send(ctx, rc, appendReplyFooter(text, footer))
}

func (p *acceptedUnknownPlatform) ReconstructReplyCtx(key string) (any, error) { return key, nil }

func TestMessageDeliveryUnconfirmedFooterDoesNotCreateReplacement(t *testing.T) {
	p := &acceptedUnknownPlatform{stubPlatformEngine{n: "fixture"}}
	err := sendChunksWithStatusFooter(context.Background(), p, "route", "answer", "status", func(pl Platform, rc any, text string) error {
		return pl.Send(context.Background(), rc, text)
	})
	if !errors.Is(err, ErrMessageDeliveryUnconfirmed) || len(p.getSent()) != 1 {
		t.Fatalf("unknown footer delivery: err=%v saved=%v", err, p.getSent())
	}
}

func TestMessageDeliveryUnconfirmedRestartDoesNotCreateReplacement(t *testing.T) {
	p := &acceptedUnknownPlatform{stubPlatformEngine{n: "fixture"}}
	e := NewEngine("fixture", &stubAgent{}, []Platform{p}, "", LangEnglish)
	defer e.cancel()
	e.OnPlatformReady(p)
	err := e.dispatchRestartNotify(&RestartRequest{Platform: p.Name(), SessionKey: "fixture:chat"})
	if !errors.Is(err, ErrMessageDeliveryUnconfirmed) || len(p.getSent()) != 1 {
		t.Fatalf("unknown restart delivery: err=%v saved=%v", err, p.getSent())
	}
}

type receiptLostRichPreview struct{ *receiptLostPreview }

// The test owns closing its event stream; cleanup must not close it twice.
type deliveryEventSession struct{ *controllableAgentSession }

func (*deliveryEventSession) Close() error { return nil }

func (p *receiptLostRichPreview) BuildRichCard(status CardStatus, _ string, _ []ToolStep, markdown string, _ bool, _ string) string {
	return fmt.Sprintf("rich:%s:%s", status, markdown)
}

func TestMessageDeliveryUnconfirmedEngineAnswer(t *testing.T) {
	for _, scenario := range []string{"thinking", "tool", "result", "text", "deferred", "hidden-thinking", "hidden-tool", "visible-thinking", "visible-tool", "stream-close", "rich-close", "send-hidden-thinking", "send-hidden-tool", "send-visible-thinking", "send-visible-tool"} {
		t.Run(scenario, func(t *testing.T) {
			p := newReceiptLostPreview()
			var platform Platform = p
			sendLoss := strings.HasPrefix(scenario, "send-")
			rich := !strings.Contains(scenario, "-") || scenario == "rich-close"
			if rich {
				platform = &receiptLostRichPreview{p}
			}
			if sendLoss {
				platform = &finalReceiptLostPlatform{receiptLostRichPreview: &receiptLostRichPreview{p}, failContent: "answer-first"}
			}
			e := NewEngine("test", &stubAgent{}, []Platform{platform}, "", LangEnglish)
			defer e.cancel()
			e.streamPreview = StreamPreviewCfg{Enabled: !sendLoss, MinDeltaChars: 1}
			cfg := DisplayCfg{Mode: "compact", ThinkingMessages: !strings.Contains(scenario, "hidden"), ToolMessages: !strings.Contains(scenario, "hidden")}
			if rich {
				cfg.CardMode = "rich"
			}
			e.SetDisplayConfig(cfg)
			key := "test:unknown"
			session := e.sessions.GetOrCreateActive(key)
			as := newControllableSession("unknown")
			state := &interactiveState{agentSession: &deliveryEventSession{as}, platform: platform, replyCtx: "route"}
			e.interactiveStates[key] = state
			var events []Event
			switch scenario {
			case "thinking":
				events = append(events, Event{Type: EventThinking, Content: "reason"})
			case "tool":
				events = append(events, Event{Type: EventToolUse, ToolName: "Read", ToolInput: "file"})
			case "result":
				events = append(events, Event{Type: EventToolResult, ToolName: "Read", ToolResult: "data"})
			case "deferred":
				events = append(events, Event{Type: EventText, Content: "NO"})
			}
			events = append(events, Event{Type: EventText, Content: "answer-first"})
			if strings.HasSuffix(scenario, "thinking") {
				events = append(events, Event{Type: EventThinking, Content: "reason"})
			}
			if strings.HasSuffix(scenario, "tool") {
				events = append(events, Event{Type: EventToolUse, ToolName: "Read", ToolInput: "file"})
			}
			if rich {
				events = append(events, Event{Type: EventThinking, Content: "more reason"}, Event{Type: EventToolUse, ToolName: "Read"}, Event{Type: EventToolResult, ToolName: "Read", ToolResult: "data"})
			}
			events = append(events, Event{Type: EventText, Content: "answer-second"})
			if !strings.HasSuffix(scenario, "close") {
				events = append(events, Event{Type: EventResult, Content: "answer-final", Done: true})
			}
			for _, ev := range events {
				as.events <- ev
			}
			if strings.HasSuffix(scenario, "close") {
				close(as.events)
			}
			e.processInteractiveEvents(state, session, e.sessions, key, "message", time.Now(), nil, nil, "route", nil)
			wantPreviews := 1
			if sendLoss {
				wantPreviews = 0
			}
			if len(p.getPreviewOpens()) != wantPreviews || len(p.getPreviewUpdates()) != 0 {
				t.Errorf("same answer recreated/updated without receipt: starts=%v updates=%v", p.getPreviewOpens(), p.getPreviewUpdates())
			}
			savedAnswers := 0
			for _, sent := range p.getSent() {
				if strings.Contains(sent, "answer-") {
					savedAnswers++
					if !sendLoss || sent != "answer-first" {
						t.Errorf("same answer replayed: %q", sent)
					}
				}
			}
			if sendLoss && savedAnswers != 1 {
				t.Errorf("saved answer count=%d want1", savedAnswers)
			}
			if p.doneReactions != 0 {
				t.Error("unknown answer acknowledged as delivered")
			}
			if len(session.GetHistory(20)) == 0 {
				t.Error("answer history lost")
			}
		})
	}
}

func TestMessageDeliveryUnconfirmedShellProgress(t *testing.T) {
	command := "sleep 1; echo completed-independent"
	if runtime.GOOS == "windows" {
		command = "Start-Sleep -Milliseconds 1000; Write-Output completed-independent"
	}
	for _, kind := range []string{"shell", "timer", "cron"} {
		t.Run(kind, func(t *testing.T) {
			p := newReceiptLostPreview()
			e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
			defer e.cancel()
			var err error
			switch kind {
			case "shell":
				err = e.runShellWithProgress(p, "route", command, t.TempDir(), 10*time.Second, 2000)
			case "timer":
				err = e.executeTimerShell(p, "route", &TimerJob{Exec: command, WorkDir: t.TempDir()})
			case "cron":
				err = e.executeCronShell(p, "route", &CronJob{Exec: command, WorkDir: t.TempDir()})
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(p.getPreviewOpens()) != 1 || len(p.getPreviewUpdates()) != 0 || len(p.getSent()) != 1 || !strings.Contains(p.getSent()[0], "completed-independent") {
				t.Fatalf("unknown working status replayed or terminal result blocked: starts=%v updates=%v sent=%v", p.getPreviewOpens(), p.getPreviewUpdates(), p.getSent())
			}
		})
	}
}

func TestMessageDeliveryUnconfirmedShellStillReportsFailureAndTimeout(t *testing.T) {
	for _, outcome := range []string{"failure", "timeout"} {
		t.Run(outcome, func(t *testing.T) {
			p := newReceiptLostPreview()
			e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
			defer e.cancel()
			command := "sleep 1; exit 2"
			if runtime.GOOS == "windows" {
				command = "Start-Sleep -Milliseconds 1000; exit 2"
			}
			timeout := 5 * time.Second
			if outcome == "timeout" {
				timeout = 700 * time.Millisecond
			}
			err := e.runShellWithProgress(p, "route", command, t.TempDir(), timeout, 2000)
			if err == nil || len(p.getPreviewOpens()) != 1 || len(p.getPreviewUpdates()) != 0 || len(p.getSent()) != 1 {
				t.Fatalf("independent terminal outcome lost or working status repeated: err=%v starts=%v updates=%v sent=%v", err, p.getPreviewOpens(), p.getPreviewUpdates(), p.getSent())
			}
			if outcome == "timeout" && !strings.Contains(p.getSent()[0], "(timeout)") {
				t.Fatalf("missing timeout notice: %v", p.getSent())
			}
			if outcome == "failure" && !strings.Contains(p.getSent()[0], "❌") {
				t.Fatalf("missing failure notice: %v", p.getSent())
			}
		})
	}
}

func testCUJUnknownPreviewPermission(t *testing.T) {
	t.Run("stream", func(t *testing.T) { testUnknownPreviewPermission(t, false) })
	t.Run("rich", func(t *testing.T) { testUnknownPreviewPermission(t, true) })
}

func testUnknownPreviewPermission(t *testing.T, rich bool) {
	env := newCUJEnv(t)
	p := newReceiptLostPreview()
	var platform Platform = p
	if rich {
		platform = &receiptLostRichPreview{p}
		env.engine.SetDisplayConfig(DisplayCfg{CardMode: "rich"})
	}
	env.plat = &p.stubPlatformEngine
	env.engine.platforms = []Platform{platform}
	env.engine.streamPreview = StreamPreviewCfg{Enabled: !rich, MinDeltaChars: 1}
	key := "test:owner"
	send := func(id, content string) {
		env.engine.ReceiveMessage(platform, &Message{SessionKey: key, Platform: "test", MessageID: id, UserID: "owner", Content: content, ReplyCtx: "route"})
	}
	env.agent.setNextSessionEvents([]Event{
		{Type: EventText, Content: "answer-before"},
		{Type: EventPermissionRequest, ToolName: "AskUserQuestion", RequestID: "unknown-permission", Questions: []UserQuestion{{Question: "Continue unknown?", Options: []UserQuestionOption{{Label: "Yes"}, {Label: "No"}}}}},
		{Type: EventText, Content: "answer-after"},
		{Type: EventResult, Content: "answer-final", Done: true},
	}, 0)
	send("one", "perform work")
	env.waitFor("independent permission remains visible", 3*time.Second, func() bool { return env.sentContains("Continue unknown?") })
	send("two", "1")
	env.waitFor("unknown turn cleanup", 3*time.Second, func() bool { return !env.activeSession(key).Busy() })
	if len(p.getPreviewOpens()) != 1 || len(p.getPreviewUpdates()) != 0 {
		t.Fatalf("unknown answer recreated: %v %v", p.getPreviewOpens(), p.getPreviewUpdates())
	}
	for _, sent := range p.getSent() {
		if strings.Contains(sent, "answer-") {
			t.Fatalf("answer replayed: %q", sent)
		}
	}
	p.mu.Lock()
	done := p.doneReactions
	p.mu.Unlock()
	if done != 0 {
		t.Fatal("unknown answer got success acknowledgement")
	}
	if !strings.Contains(fmt.Sprint(env.activeSession(key).GetHistory(20)), "answer-final") {
		t.Fatal("unknown turn lost history")
	}
	send("three", "independent next task")
	env.waitFor("next turn can send normally", 3*time.Second, func() bool { return env.sentContains("ok") && !env.activeSession(key).Busy() })
}

func testCUJUnknownRichQueued(t *testing.T) {
	env := newCUJEnv(t)
	p := &receiptLostRichPreview{newReceiptLostPreview()}
	env.plat = &p.stubPlatformEngine
	env.engine.platforms = []Platform{p}
	env.engine.SetDisplayConfig(DisplayCfg{CardMode: "rich", ToolMessages: true})
	key := "test:queued-owner"
	send := func(id, content string) {
		env.engine.ReceiveMessage(p, &Message{SessionKey: key, Platform: "test", MessageID: id, UserID: "queued-owner", Content: content, ReplyCtx: "route"})
	}
	env.agent.setNextSessionEvents([]Event{
		{Type: EventText, Content: "answer-first"},
		{Type: EventToolUse, ToolName: "Read"},
		{Type: EventText, Content: "answer-continuation"},
		{Type: EventResult, Content: "answer-final", Done: true},
	}, 40)
	send("queued-one", "first task")
	env.waitFor("unknown first preview", 3*time.Second, func() bool { return len(p.getPreviewOpens()) == 1 })
	// Prepare the next agent turn while the first is still producing events.
	env.agent.mu.Lock()
	as := env.agent.sessions[0]
	env.agent.mu.Unlock()
	as.mu.Lock()
	as.pendingEvents = []Event{{Type: EventText, Content: "next-answer"}, {Type: EventResult, Content: "next-answer", Done: true}}
	as.mu.Unlock()
	send("queued-two", "independent queued task")
	env.waitFor("queued answer delivered", 3*time.Second, func() bool { return len(p.getPreviewOpens()) >= 2 && !env.activeSession(key).Busy() })
	if len(p.getPreviewOpens()) != 2 {
		t.Fatalf("first answer recreated: %v", p.getPreviewOpens())
	}
	updates := p.getPreviewUpdates()
	if len(updates) == 0 || !strings.Contains(updates[len(updates)-1].Content, "next-answer") {
		t.Fatalf("queued turn did not resume: %v", updates)
	}
	for _, sent := range p.getSent() {
		if strings.Contains(sent, "answer-") {
			t.Fatalf("first answer replayed: %q", sent)
		}
	}
	send("queued-three", "third independent task")
	env.waitFor("third turn delivered", 3*time.Second, func() bool { return env.sentContains("ok") && !env.activeSession(key).Busy() })
}

type finalReceiptLostPlatform struct {
	*receiptLostRichPreview
	failContent string
	split       bool
}

func (p *finalReceiptLostPlatform) Send(ctx context.Context, rc any, text string) error {
	_ = p.stubPlatformEngine.Send(ctx, rc, text)
	if strings.Contains(text, p.failContent) {
		return ErrMessageDeliveryUnconfirmed
	}
	return nil
}

func (p *finalReceiptLostPlatform) SendWithStatusFooter(ctx context.Context, rc any, text, footer string) error {
	return p.Send(ctx, rc, appendReplyFooter(text, footer))
}

func (p *finalReceiptLostPlatform) SplitMarkdownByTables(text string, _ int) []string {
	if p.split && text == "answer-final" {
		return []string{text, "overflow-unknown", "overflow-after"}
	}
	return []string{text}
}

func testCUJUnknownFinalAllowsQueued(t *testing.T) {
	for _, mode := range []string{"plain", "rich", "overflow"} {
		t.Run(mode, func(t *testing.T) {
			env := newCUJEnv(t)
			p := &finalReceiptLostPlatform{receiptLostRichPreview: &receiptLostRichPreview{newReceiptLostPreview()}, failContent: "answer-final", split: mode == "overflow"}
			if p.split {
				p.failContent = "overflow-unknown"
			}
			env.plat = &p.stubPlatformEngine
			env.engine.platforms = []Platform{p}
			env.engine.streamPreview.Enabled = false
			if mode != "plain" {
				env.engine.SetDisplayConfig(DisplayCfg{CardMode: "rich"})
			}
			key := "test:final-owner"
			send := func(id, content string) {
				env.engine.ReceiveMessage(p, &Message{SessionKey: key, Platform: "test", MessageID: id, UserID: "final-owner", Content: content, ReplyCtx: "route"})
			}
			env.agent.setNextSessionEvents([]Event{{Type: EventResult, Content: "answer-final", Done: true}}, 100)
			send("final-one", "first task")
			env.waitFor("first task running", 3*time.Second, func() bool { env.agent.mu.Lock(); defer env.agent.mu.Unlock(); return len(env.agent.sessions) == 1 })
			send("final-two", "queued independent task")
			env.waitFor("queued task survives unknown final send", 3*time.Second, func() bool { return env.sentContains("ok") && !env.activeSession(key).Busy() })
			count := 0
			for _, sent := range p.getSent() {
				if strings.Contains(sent, p.failContent) {
					count++
				}
				if strings.Contains(sent, "overflow-after") {
					t.Errorf("continued overflow after unknown: %q", sent)
				}
			}
			if count != 1 {
				t.Fatalf("unknown final message replayed: %v", p.getSent())
			}
			send("final-three", "third independent task")
			env.waitFor("third task completes", 3*time.Second, func() bool { return !env.activeSession(key).Busy() && env.activeSession(key).HistoryLen() >= 6 })
		})
	}
}

type deliveryHookTransport struct{ calls atomic.Int32 }

func (h *deliveryHookTransport) RoundTrip(*http.Request) (*http.Response, error) {
	h.calls.Add(1)
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
}

func TestMessageDeliveryUnconfirmedFinalHook(t *testing.T) {
	for _, mode := range []string{"plain", "rich", "overflow", "success"} {
		t.Run(mode, func(t *testing.T) {
			p := &finalReceiptLostPlatform{receiptLostRichPreview: &receiptLostRichPreview{newReceiptLostPreview()}, failContent: "answer-final", split: mode == "overflow"}
			if mode == "overflow" {
				p.failContent = "overflow-unknown"
			}
			if mode == "success" {
				p.failContent = "never matches"
			}
			e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
			defer e.cancel()
			if mode == "rich" || mode == "overflow" {
				e.SetDisplayConfig(DisplayCfg{CardMode: "rich"})
			}
			async := false
			hook := &deliveryHookTransport{}
			e.hooks = NewHookManager("test", []HookConfig{{Event: string(HookEventMessageSent), Type: "http", URL: "http://hook.invalid", Async: &async}}, "", "", "")
			e.hooks.client = &http.Client{Transport: hook}
			tts := &recordingTTS{}
			e.tts = &TTSCfg{Enabled: true, TTS: tts, ttsMode: "always"}
			key := "test:final-hook"
			session := e.sessions.GetOrCreateActive(key)
			as := newControllableSession("final-hook")
			state := &interactiveState{agentSession: as, platform: p, replyCtx: "route"}
			as.events <- Event{Type: EventResult, Content: "answer-final", Done: true}
			e.processInteractiveEvents(state, session, e.sessions, key, "message", time.Now(), nil, nil, "route", nil)
			want := int32(0)
			if mode == "success" {
				want = 1
			}
			if hook.calls.Load() != want {
				t.Errorf("message.sent hooks=%d want %d", hook.calls.Load(), want)
			}
			if mode != "success" {
				time.Sleep(30 * time.Millisecond)
				if _, _, calls := tts.snapshot(); calls != 0 {
					t.Error("unknown answer triggered TTS")
				}
				if p.doneReactions != 0 {
					t.Error("unknown final got success reaction")
				}
			}
		})
	}
}
