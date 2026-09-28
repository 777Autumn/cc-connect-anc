package core

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type failedHistoryAgent struct{ stubAgent }

func (*failedHistoryAgent) GetSessionHistory(context.Context, string, int) ([]HistoryEntry, error) {
	return nil, errors.New("synthetic private directory is unreadable")
}

func TestHistoryReadFailureIsNotEmpty(t *testing.T) {
	e := newTestEngine()
	e.agent = &failedHistoryAgent{}
	p := &stubPlatformEngine{n: "test"}
	key := "test:private:history"
	e.sessions.GetOrCreateActive(key).SetAgentSessionID("known-session", "stub")
	e.cmdHistory(p, &Message{SessionKey: key}, nil)
	text := strings.Join(p.getSent(), "\n")
	if text == "" || strings.Contains(text, e.i18n.T(MsgHistoryEmpty)) {
		t.Fatalf("failed history shown as empty: %s", text)
	}
	card := e.renderHistoryCard(key).RenderText()
	if strings.Contains(card, e.i18n.T(MsgHistoryEmpty)) || !strings.Contains(card, text) {
		t.Fatalf("card does not report same read failure: %s / %s", card, text)
	}
}
