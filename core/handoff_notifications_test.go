package core

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type notificationPlatform struct {
	filePlatformStub
	principal        ActionPrincipal
	sends, edits     int
	sendErr, editErr error
	lastCard         *Card
	callback         TrustedCardActionHandler
}

type handoffPendingHost struct {
	actionHostStub
	pending bool
	err     error
	seen    []ActionPrincipal
}

func (h *handoffPendingHost) PendingActions(_ context.Context, p ActionPrincipal) (bool, error) {
	h.seen = append(h.seen, p)
	return h.pending, h.err
}

func TestHandoffStartKeepsPendingApprovalsAndUnreadableMaterials(t *testing.T) {
	for _, mode := range []string{"pending-primary", "pending-nested", "host-unavailable", "case-awaiting", "case-executing", "crm-busy", "revoked", "missing-material"} {
		t.Run(mode, func(t *testing.T) {
			e, p, _ := notificationFixture(t)
			a := &fileSessionJourneyAgent{}
			e.agent = a
			e.sessions = NewSessionManager(filepath.Join(t.TempDir(), "sessions.json"))
			prior := e.sessions.GetOrCreateActive(p.principal.SessionKey)
			h := &handoffStartHost{busy: mode == "crm-busy", refuse: mode == "revoked"}
			e.handoffNotifications.host = h
			files := &handoffJourneyFiles{broken: mode == "missing-material"}
			root := t.TempDir()
			if err := e.SetFileWorkHost(files, func(id string) (string, int, error) { return filepath.Join(root, id), 0, nil }); err != nil {
				t.Fatal(err)
			}
			checker := &handoffPendingHost{pending: strings.HasPrefix(mode, "pending-")}
			if mode == "host-unavailable" {
				checker.err = errors.New("ledger unreadable")
			}
			e.actionHost = checker
			if mode == "pending-nested" {
				e.actionHost = CombineActionHosts(&actionHostStub{}, CombineActionHosts(&actionHostStub{}, checker, nil), nil)
			}
			if strings.HasPrefix(mode, "case-") {
				q := &CaseQuestion{Status: strings.TrimPrefix(mode, "case-"), Expires: time.Now().Add(time.Hour)}
				if err := e.sessions.saveCaseQuestion(prior, q); err != nil {
					t.Fatal(err)
				}
			}
			e.initPlatformCapabilities(p)
			t.Cleanup(func() { _ = e.Stop() })
			action := TrustedCardAction{Kind: handoffStartKind, ApprovalID: "event", Decision: "start", Principal: p.principal}
			action.Principal.MessageID = "notice-message"
			p.callback(action)
			key := MsgHandoffBusy
			if mode == "host-unavailable" || mode == "revoked" || mode == "missing-material" {
				key = MsgHandoffUnavailable
			}
			if !strings.Contains(strings.Join(p.getSent(), "\n"), e.i18n.T(key)) {
				t.Fatalf("missing refusal: %v", p.getSent())
			}
			if h.claims != 0 || len(files.bindings) != 0 || e.sessions.GetOrCreateActive(p.principal.SessionKey).ID != prior.ID {
				t.Fatal("refused handoff created work or replaced current session")
			}
			a.mu.Lock()
			starts := len(a.resumeIDs)
			a.mu.Unlock()
			if starts != 0 {
				t.Fatal("refused handoff started a model")
			}
			if len(checker.seen) != 1 || checker.seen[0].UserID != p.principal.UserID || checker.seen[0].ChatID != p.principal.ChatID || checker.seen[0].Project != e.name {
				t.Fatal("pending check did not receive physical principal")
			}
		})
	}
}

func TestHandoffStartIgnoresCompletedExpiredAndOtherSessionQuestions(t *testing.T) {
	for _, mode := range []string{"no-pending", "done", "expired", "other-session"} {
		t.Run(mode, func(t *testing.T) {
			e, p, _ := notificationFixture(t)
			e.sessions = NewSessionManager(filepath.Join(t.TempDir(), "sessions.json"))
			e.handoffNotifications.host = &handoffStartHost{}
			e.actionHost = &handoffPendingHost{}
			if mode != "no-pending" {
				key := p.principal.SessionKey
				if mode == "other-session" {
					key = "fixture:other:other"
				}
				q := &CaseQuestion{Status: "awaiting", Expires: time.Now().Add(time.Hour)}
				if mode == "done" {
					q.Status = "done"
				}
				if mode == "expired" {
					q.Expires = time.Now().Add(-time.Hour)
				}
				if err := e.sessions.saveCaseQuestion(e.sessions.GetOrCreateActive(key), q); err != nil {
					t.Fatal(err)
				}
			}
			if err := e.SetFileWorkHost(&handoffJourneyFiles{}, func(string) (string, int, error) { return t.TempDir(), 0, nil }); err != nil {
				t.Fatal(err)
			}
			msg := &Message{Platform: p.Name(), SessionKey: p.principal.SessionKey, UserID: p.principal.UserID, ChannelID: p.principal.ChatID, MessageID: "notice-message", handoffStart: &handoffStartRequest{eventID: "event"}}
			if got := e.prepareHandoffStart(msg); got != "" {
				t.Fatalf("unrelated/completed approval blocked start: %q", got)
			}
		})
	}
}

type handoffStartHost struct {
	notificationHost
	started bool
	busy    bool
	refuse  bool
	claims  int
}

func (h *handoffStartHost) HandoffCall(ctx context.Context, command string, input map[string]any) (map[string]any, error) {
	if command != "host-handoff-start" {
		return h.notificationHost.HandoffCall(ctx, command, input)
	}
	if h.refuse {
		return nil, errors.New("revoked")
	}
	if h.busy {
		return map[string]any{"status": "busy"}, nil
	}
	if h.started {
		return map[string]any{"status": "replayed"}, nil
	}
	result := map[string]any{"status": "ready", "banquet_id": "recFixture", "version": 1, "file_check": BanquetFileCheck{Published: true, WorkID: "author-work", Files: []CasePublicationFile{{Name: "handoff.docx"}}}}
	if work, _ := input["work_id"].(string); work != "" {
		if input["handoff_files_verified"] != true || input["session_id"] == "" {
			return nil, errors.New("missing host check")
		}
		h.started = true
		h.claims++
		result["status"] = "started"
	}
	return result, nil
}

type handoffJourneyFiles struct {
	fileHostStub
	broken bool
}

func (h *handoffJourneyFiles) Bind(ctx context.Context, b FileWorkBinding) (FileWorkContext, error) {
	work, err := h.fileHostStub.Bind(ctx, b)
	work.WorkID = "work-" + b.SessionID
	return work, err
}
func (h *handoffJourneyFiles) Tool(ctx context.Context, command string, input json.RawMessage, p ActionPrincipal, work string) (map[string]any, error) {
	if command == "host-handoff-check" {
		if h.broken {
			return nil, errors.New("missing snapshot")
		}
		return map[string]any{"status": "verified"}, nil
	}
	return h.fileHostStub.Tool(ctx, command, input, p, work)
}

func (p *notificationPlatform) SetTrustedCardActionHandler(handler TrustedCardActionHandler) {
	p.callback = handler
}

func (p *notificationPlatform) HandoffReplyContext(principal ActionPrincipal) (any, error) {
	return principal.MessageID, nil
}

func (p *notificationPlatform) HandoffPrincipal() ActionPrincipal           { return p.principal }
func (p *notificationPlatform) ReconstructReplyCtx(key string) (any, error) { return key, nil }
func (p *notificationPlatform) SendHandoffCard(_ context.Context, principal ActionPrincipal, card *Card, id string) (string, error) {
	p.sends++
	p.lastCard = card
	if principal.UserID != p.principal.UserID || id == "" {
		return "", errors.New("wrong target")
	}
	return "notice-message", p.sendErr
}
func (p *notificationPlatform) RefreshCardMessage(_ context.Context, _, _ string, card *Card) error {
	p.edits++
	p.lastCard = card
	return p.editErr
}

type notificationHost struct {
	calls   []string
	claimed bool
	ack     string
	ackErr  bool
	event   map[string]any
}

func (h *notificationHost) HandoffCall(_ context.Context, command string, input map[string]any) (map[string]any, error) {
	h.calls = append(h.calls, command)
	switch command {
	case "host-handoff-events":
		return map[string]any{"status": "ok", "events": []any{h.event}}, nil
	case "host-handoff-notify-claim":
		if h.claimed {
			return map[string]any{"status": "already_claimed"}, nil
		}
		h.claimed = true
		return map[string]any{"status": "claimed", "event": h.event, "dispatch_id": "dispatch-fixture"}, nil
	case "host-handoff-notify-ack":
		h.ack, _ = input["status"].(string)
		if h.ackErr {
			return nil, errors.New("disk")
		}
		return map[string]any{"status": "recorded"}, nil
	}
	return nil, errors.New("unexpected command")
}
func notificationFixture(t *testing.T) (*Engine, *notificationPlatform, *notificationHost) {
	t.Helper()
	p := &notificationPlatform{filePlatformStub: filePlatformStub{stubPlatformEngine: stubPlatformEngine{n: "fixture"}}, principal: ActionPrincipal{Platform: "fixture", UserID: "user", ChatID: "chat", SessionKey: "fixture:chat:user"}}
	e := NewEngine("private", &fileAgentStub{}, []Platform{p}, "", LangChinese)
	t.Cleanup(e.cancel)
	h := &notificationHost{event: map[string]any{"event_id": "change-fixture", "kind": "submit", "banquet_id": "recFixture", "version": float64(1), "target": map[string]any{"platform": "fixture", "project": "private", "user_id": "user", "chat_id": "chat", "member_id": "member", "source_app_id": "app", "source_tenant_key": "tenant"}}}
	if err := e.SetHandoffNotifications(p, h, HandoffNotificationRoute{AppID: "app", TenantKey: "tenant", MemberID: "member", UserID: "user", ChatID: "chat"}); err != nil {
		t.Fatal(err)
	}
	return e, p, h
}
func TestHandoffNotificationBindsReceiptBeforeButtonsAndNeverResends(t *testing.T) {
	for _, mode := range []string{"success", "send-unknown", "ack-unknown", "edit-failed", "wrong-target"} {
		t.Run(mode, func(t *testing.T) {
			e, p, h := notificationFixture(t)
			if mode == "send-unknown" {
				p.sendErr = errors.New("network")
			}
			if mode == "ack-unknown" {
				h.ackErr = true
			}
			if mode == "edit-failed" {
				p.editErr = errors.New("network")
			}
			if mode == "wrong-target" {
				h.event["target"].(map[string]any)["user_id"] = "other"
			}
			e.pollHandoffNotifications()
			e.pollHandoffNotifications()
			if mode == "wrong-target" {
				if p.sends != 0 {
					t.Fatal("wrong recipient sent")
				}
				return
			}
			if p.sends != 1 {
				t.Fatalf("repeated send: %d", p.sends)
			}
			if mode == "send-unknown" && (h.ack != "unknown" || p.edits != 0) {
				t.Fatal("unknown receipt became actionable")
			}
			if mode == "ack-unknown" && p.edits != 0 {
				t.Fatal("unbound card became actionable")
			}
			if mode == "success" && (h.ack != "sent" || p.edits != 1) {
				t.Fatal("successful card not bound")
			}
		})
	}
}
