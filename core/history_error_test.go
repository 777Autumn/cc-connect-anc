package core

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

type failedHistoryAgent struct{ stubAgent }

func (*failedHistoryAgent) GetSessionHistory(context.Context, string, int) ([]HistoryEntry, error) {
	return nil, errors.New("synthetic private directory is unreadable")
}

type resultHistoryAgent struct {
	stubAgent
	entries []HistoryEntry
	err     error
	calls   int
	id      string
	limit   int
}

func (a *resultHistoryAgent) GetSessionHistory(_ context.Context, id string, limit int) ([]HistoryEntry, error) {
	a.calls++
	a.id, a.limit = id, limit
	return a.entries, a.err
}

func TestHistoryEntries_NativeHistoryPrecedesLocalContinuation(t *testing.T) {
	readErr := errors.New("synthetic native history read failure")
	for _, tc := range []struct {
		name     string
		id       string
		provider bool
		native   []HistoryEntry
		err      error
	}{
		{"native includes the original conversation", "known-thread", true, []HistoryEntry{{Role: "user", Content: "original native request"}}, nil},
		{"empty successful native history falls back", "known-thread", true, nil, nil},
		{"native failure is not local success", "known-thread", true, nil, readErr},
		{"no native ID uses local history", "", true, nil, nil},
		{"no history capability uses local history", "known-thread", false, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEngine()
			s := &Session{}
			s.SetAgentSessionID(tc.id, "stub")
			s.AddHistory("user", "older local entry")
			s.AddHistory("assistant", "new local continuation")
			hp := &resultHistoryAgent{entries: tc.native, err: tc.err}
			var agent Agent = &stubAgent{}
			if tc.provider {
				agent = hp
			}
			got, err := e.historyEntries(agent, s, 1)
			if err != tc.err {
				t.Fatalf("history error = %v, want the original %v", err, tc.err)
			}
			want := s.GetHistory(1)
			if len(tc.native) > 0 || tc.err != nil {
				want = tc.native
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("history = %+v, want %+v", got, want)
			}
			if tc.provider && tc.id != "" {
				if hp.calls != 1 || hp.id != tc.id || hp.limit != 1 {
					t.Fatalf("provider call was bypassed or changed: calls=%d id=%q limit=%d", hp.calls, hp.id, hp.limit)
				}
			} else if hp.calls != 0 {
				t.Fatal("history provider called without a native binding")
			}
		})
	}
}

func TestHistoryReadFailureIsNotEmpty(t *testing.T) {
	e := newTestEngine()
	e.agent = &failedHistoryAgent{}
	p := &stubPlatformEngine{n: "test"}
	key := "test:private:history"
	s := e.sessions.GetOrCreateActive(key)
	s.SetAgentSessionID("known-session", "stub")
	s.AddHistory("assistant", "recent local content must not hide a failed native read")
	e.cmdHistory(p, &Message{SessionKey: key}, nil)
	text := strings.Join(p.getSent(), "\n")
	if !strings.Contains(text, e.i18n.T(MsgHistoryReadFailed)) || strings.Contains(text, e.i18n.T(MsgHistoryEmpty)) || strings.Contains(text, "recent local content") || strings.Contains(text, "private directory") {
		t.Fatalf("failed native history hidden by empty/local success or private error text: %s", text)
	}
	card := e.renderHistoryCard(key).RenderText()
	if strings.Contains(card, e.i18n.T(MsgHistoryEmpty)) || !strings.Contains(card, text) {
		t.Fatalf("card does not report same read failure: %s / %s", card, text)
	}
}
