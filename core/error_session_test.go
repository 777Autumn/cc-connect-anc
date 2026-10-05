package core

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

type cleanupOnErrorPlatform struct {
	previewCapturePlatform
	beforeError func()
}

func (p *cleanupOnErrorPlatform) UpdateMessage(ctx context.Context, target any, content string) error {
	if payload, ok := ParseProgressCardPayload(content); ok && payload.State == ProgressCardStateFailed {
		p.beforeError()
	}
	return p.previewCapturePlatform.UpdateMessage(ctx, target, content)
}

func TestErrorSessionIDSurvivesConcurrentCleanup(t *testing.T) {
	store := filepath.Join(t.TempDir(), "sessions.json")
	p := &cleanupOnErrorPlatform{}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, store, LangEnglish)
	key := "test:owner"
	session := e.sessions.GetOrCreateActive(key)
	live := newControllableSession("late-id")
	reply := progressHintReplyCtx{style: progressStyleCard, payload: true}
	state := &interactiveState{agentSession: live, agent: &controllableAgent{}, platform: p, replyCtx: reply}
	e.interactiveStates[key] = state
	p.beforeError = func() {
		// Finalizing the error card interleaves actual cleanup after the
		// error is selected, before native ID persistence.
		done := make(chan struct{})
		go func() {
			e.cleanupInteractiveState(key, state)
			close(done)
		}()
		<-done
		e.sessions.NewSession(key, "replacement").SetAgentSessionID("replacement-id", "controllable")
		e.sessions.Save()
	}
	live.events <- Event{Type: EventToolUse, ToolName: "Read", ToolInput: "fictional.txt"}
	live.events <- Event{Type: EventError, Error: errors.New("failed after ID allocation")}
	e.processInteractiveEvents(state, session, e.sessions, key, "error-message", time.Now(), nil, nil, reply)
	reloaded := NewSessionManager(store)
	if reloaded.sessions[session.ID].GetAgentSessionID() != "late-id" || reloaded.GetOrCreateActive(key).GetAgentSessionID() != "replacement-id" {
		t.Fatal("cleanup lost the failed turn ID or changed the replacement task")
	}
}

func TestErrorSessionIDPersistsForRestartWithoutRevivingExpiredWork(t *testing.T) {
	for _, tc := range []struct {
		name, oldID, reportedID, pendingName string
		expired, switched                    bool
	}{
		{name: "late first ID", reportedID: "late-id"},
		{name: "named first ID", reportedID: "late-id", pendingName: "Project Alpha"},
		{name: "forked ID", oldID: "old-id", reportedID: "late-id"},
		{name: "empty ID", oldID: "old-id"},
		{name: "same ID", oldID: "same-id", reportedID: "same-id"},
		{name: "continue sentinel", oldID: "old-id", reportedID: ContinueSession},
		{name: "expired work", oldID: "old-id", reportedID: "late-id", expired: true},
		{name: "late error after new task", oldID: "old-id", reportedID: "late-id", switched: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := filepath.Join(t.TempDir(), "sessions.json")
			p := &stubPlatformEngine{n: "test"}
			e := NewEngine("test", &stubAgent{}, []Platform{p}, store, LangEnglish)
			key := "test:owner"
			session := e.sessions.GetOrCreateActive(key)
			if tc.pendingName != "" {
				session.SetName(tc.pendingName)
			}
			session.SetAgentSessionID(tc.oldID, "controllable")
			if tc.expired {
				if err := e.sessions.expireConnection(session); err != nil {
					t.Fatal(err)
				}
			}
			other := e.sessions.GetOrCreateActive("test:other")
			other.SetAgentSessionID("other-id", "controllable")
			if tc.switched {
				e.sessions.NewSession(key, "new-task").SetAgentSessionID("new-task-id", "controllable")
			}
			e.sessions.Save()
			live := newControllableSession(tc.reportedID)
			state := &interactiveState{agentSession: live, agent: &controllableAgent{}, platform: p, replyCtx: "ctx"}
			live.events <- Event{Type: EventError, Error: errors.New("synthetic failed turn")}
			e.processInteractiveEvents(state, session, e.sessions, key, "error-message", time.Now(), nil, nil, "ctx")
			want := tc.reportedID
			if want == "" || want == ContinueSession {
				want = tc.oldID
			}
			reloaded := NewSessionManager(store)
			saved := reloaded.sessions[session.ID]
			if tc.pendingName != "" {
				matched := e.matchSession([]AgentSessionInfo{{ID: want}}, reloaded, tc.pendingName)
				if reloaded.GetSessionName(want) != tc.pendingName || matched == nil || matched.ID != want {
					t.Fatal("failed first turn lost its custom name for list/switch")
				}
			}
			if saved.GetAgentSessionID() != want || saved.AgentType != "controllable" || saved.connectionExpired() != tc.expired {
				t.Fatalf("persisted ID=%q expired=%v", saved.GetAgentSessionID(), saved.connectionExpired())
			}
			if tc.oldID != "" && tc.oldID != want && !slices.Contains(saved.PastAgentSessionIDs, tc.oldID) {
				t.Fatal("old native provenance was lost")
			}
			if reloaded.GetOrCreateActive("test:other").GetAgentSessionID() != "other-id" {
				t.Fatal("other account changed")
			}
			if tc.switched && reloaded.GetOrCreateActive(key).GetAgentSessionID() != "new-task-id" {
				t.Fatal("late error replaced the new task")
			}
			if tc.expired {
				if err := e.guardConnectionSession(reloaded, saved, ActionPrincipal{}, "work"); err == nil {
					t.Fatal("failed turn revived expired context")
				}
				if _, err := reloaded.SwitchToAgentSession(key, want, "controllable", ""); err == nil {
					t.Fatal("expired native ID was reusable")
				}
			} else if !tc.switched {
				var resumed string
				a := &controllableAgent{startSessionFn: func(_ context.Context, id string) (AgentSession, error) {
					resumed = id
					return newControllableSession(id), nil
				}}
				restarted := NewEngine("test", a, []Platform{p}, store, LangEnglish)
				restarted.getOrCreateInteractiveStateWith(key, p, "ctx", restarted.sessions.GetOrCreateActive(key), restarted.sessions, nil, "")
				if resumed != want {
					t.Fatalf("restart resumed %q instead of %q", resumed, want)
				}
			}
			if len(p.getSent()) != 1 {
				t.Fatal("error presentation changed")
			}
		})
	}
}
