package core

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHandoffStartPreservesActiveFileRecoveryAfterReload(t *testing.T) {
	for _, tc := range []struct {
		name, status        string
		historical, blocked bool
	}{
		{name: "active-queued", status: "queued", blocked: true},
		{name: "active-started", status: "started", blocked: true},
		{name: "active-completed", status: "completed"},
		{name: "active-stopped", status: "stopped"},
		{name: "historical-queued", status: "queued", historical: true},
		{name: "historical-started", status: "started", historical: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, p, _ := notificationFixture(t)
			agent := &fileSessionJourneyAgent{}
			e.agent = agent
			path := filepath.Join(t.TempDir(), "sessions.json")
			e.sessions = NewSessionManager(path)
			host := &handoffStartHost{}
			e.handoffNotifications.host = host
			files := &handoffJourneyFiles{}
			root := t.TempDir()
			if err := e.SetFileWorkHost(files, func(id string) (string, int, error) { return filepath.Join(root, id), 0, nil }); err != nil {
				t.Fatal(err)
			}
			e.initPlatformCapabilities(p)
			t.Cleanup(func() { _ = e.Stop() })
			key := p.principal.SessionKey
			prior := e.sessions.GetOrCreateActive(key)
			turn := fixtureFileTurn("unfinished-before-restart")
			turn.Status, turn.Principal.Project = tc.status, e.name
			if err := e.sessions.addFileTurn(prior, turn, 2); err != nil {
				t.Fatal(err)
			}
			if tc.historical {
				e.sessions.NewSession(key, "current independent task")
			}
			activeID, sessionCount := e.sessions.ActiveSessionID(key), len(e.sessions.ListSessions(key))
			e.sessions = NewSessionManager(path)
			if active := e.sessions.FindByID(activeID); active == nil || active.Busy() || e.sessions.ActiveSessionID(key) != activeID {
				t.Fatal("reload did not restore the inactive-process session fixture")
			}
			if got := e.sessions.FindByID(prior.ID).fileTurns(); len(got) != 1 || got[0].Status != tc.status {
				t.Fatal("persisted work receipt changed", got)
			}
			action := TrustedCardAction{Kind: handoffStartKind, ApprovalID: "event", Decision: "start", Principal: p.principal}
			action.Principal.MessageID = "notice-message"
			result := p.callback(action)
			if tc.blocked {
				agent.mu.Lock()
				modelStarts := len(agent.resumeIDs)
				agent.mu.Unlock()
				if !strings.Contains(strings.Join(p.getSent(), "\n"), e.i18n.T(MsgHandoffBusy)) || host.claims != 0 || len(files.bindings) != 0 || modelStarts != 0 {
					t.Fatalf("unfinished active work allowed handoff: replies=%v claims=%d bindings=%d modelStarts=%d", p.getSent(), host.claims, len(files.bindings), modelStarts)
				}
				if e.sessions.ActiveSessionID(key) != activeID || len(e.sessions.ListSessions(key)) != sessionCount {
					t.Fatal("handoff displaced the persisted recovery entry")
				}
				if restored := NewSessionManager(path); restored.ActiveSessionID(key) != activeID || !restored.FindByID(activeID).hasUnfinishedFileTurns() {
					t.Fatal("handoff changed the recovery snapshot")
				}
				return
			}
			if result.Toast != e.i18n.T(MsgHandoffStarted) || host.claims != 1 || len(files.bindings) != 1 {
				t.Fatalf("terminal or historical work blocked new handoff: %v claims=%d", result, host.claims)
			}
			env := &cujEnv{t: t}
			env.waitFor("new handoff model reply", 3*time.Second, func() bool {
				agent.mu.Lock()
				starts := len(agent.resumeIDs)
				agent.mu.Unlock()
				return starts == 1 && len(p.getSent()) > 0 && !e.sessions.GetOrCreateActive(key).Busy()
			})
			if e.sessions.ActiveSessionID(key) == activeID || len(e.sessions.ListSessions(key)) != sessionCount+1 {
				t.Fatal("allowed handoff did not get one new task")
			}
			if got := e.sessions.FindByID(prior.ID).fileTurns(); len(got) != 1 || got[0].Status != tc.status {
				t.Fatal("historical task was altered", got)
			}
		})
	}
}
