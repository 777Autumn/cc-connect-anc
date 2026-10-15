package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestActionToolTimingSeparatesGuardAndHostWithoutPrivatePayload(t *testing.T) {
	for _, denied := range []bool{false, true} {
		e, host, _, state := actionToolFixture(t)
		connection := &connectionHostFixture{}
		if denied {
			connection.guardError = errors.New("private error must not be logged")
		}
		e.SetConnectionHost(connection)
		var logs bytes.Buffer
		previous := slog.Default()
		slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
		w := actionToolRequest(e.ActionToolHandler(), "POST", "/tool", state.actionToken, `{"command":"customer","input":{"query":"private-customer"}}`)
		slog.SetDefault(previous)
		if (w.Code == 403) != denied || host.toolCalls != map[bool]int{false: 1, true: 0}[denied] {
			t.Fatal("timing changed the authorization or dispatch")
		}
		var record map[string]any
		if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &record); err != nil {
			t.Fatalf("one timing record required: %v", err)
		}
		if record["msg"] != "action tool timing" || record["guard_allowed"] != !denied || record["host_returned"] != !denied {
			t.Fatalf("missing actual phase outcome: %v", record)
		}
		for _, key := range []string{"total_ns", "preflight_ns", "project_lock_wait_ns", "connection_guard_ns", "host_ns"} {
			if value, ok := record[key].(float64); !ok || value < 0 || value > record["total_ns"].(float64) {
				t.Fatalf("invalid phase %s", key)
			}
		}
		if len(record["message_sha256"].(string)) != 64 || record["trace_id"] == "" {
			t.Fatal("no correlation")
		}
		for _, private := range []string{state.actionToken, "private-customer", "private error", state.currentPrincipal.SessionKey} {
			if strings.Contains(logs.String(), private) {
				t.Fatal("private payload entered timing log")
			}
		}
	}
}

type timingReplyPlatform struct {
	stubPlatformEngine
	reject bool
	calls  atomic.Int32
}

func (p *timingReplyPlatform) Send(ctx context.Context, reply any, content string) error {
	p.calls.Add(1)
	if p.reject {
		return errors.New("fixture send failure")
	}
	return p.stubPlatformEngine.Send(ctx, reply, content)
}

func TestReplyTimingDistinguishesInterruptedSendAndSilentTurn(t *testing.T) {
	for _, silent := range []bool{false, true} {
		t.Run(fmt.Sprint(silent), func(t *testing.T) {
			logPath := t.TempDir() + "/timing.jsonl"
			file, err := os.Create(logPath)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = file.Close() })
			previous := slog.Default()
			defer slog.SetDefault(previous)
			slog.SetDefault(slog.New(slog.NewJSONHandler(file, nil)))
			p := &timingReplyPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}, reject: !silent}
			content := "fixture answer"
			if silent {
				content = "NO_REPLY"
			}
			a := &cujAgent{nextSessionEvents: []Event{{Type: EventResult, Content: content, Done: true}}}
			e := NewEngine("test", a, []Platform{p}, t.TempDir()+"/sessions.json", LangEnglish)
			t.Cleanup(func() { _ = e.Stop() })
			e.SetDisplayConfig(DisplayCfg{Mode: "quiet", FinalResponseOnly: true})
			e.ReceiveMessage(p, &Message{Platform: "test", SessionKey: "fixture", MessageID: "fixture-message", UserID: "fixture", Content: "fixture query"})
			env := &cujEnv{t: t, engine: e, agent: a, plat: &p.stubPlatformEngine}
			env.waitFor("final reply timing", 2*time.Second, func() bool {
				data, _ := os.ReadFile(logPath)
				return bytes.Contains(data, []byte(`"msg":"final reply timing"`)) && !e.sessions.GetOrCreateActive("fixture").Busy()
			})
			_ = e.Stop()
			slog.SetDefault(previous)
			data, _ := os.ReadFile(logPath)
			for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
				var record map[string]any
				if json.Unmarshal(line, &record) == nil && record["msg"] == "final reply timing" {
					if record["handler_returned"] != silent || record["silent"] != silent {
						t.Fatalf("incorrect send outcome: %v", record)
					}
				}
			}
			if p.calls.Load() != map[bool]int32{false: 1, true: 0}[silent] {
				t.Fatal("timing replayed or surfaced a silent reply")
			}
		})
	}
}

func TestCUJ_TIMING1_QueuedQueriesKeepRepliesAndSeparateFinalSend(t *testing.T) {
	logPath := t.TempDir() + "/timing.jsonl"
	file, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(file, nil)))
	defer slog.SetDefault(previous)
	env := newCUJEnv(t)
	env.agent.nextSessionEvents = []Event{{Type: EventResult, Content: "first answer", Done: true}}
	env.agent.nextSessionDelayMs = 120
	env.userSends("alex", "first query")
	env.waitFor("first agent started", 2*time.Second, func() bool {
		env.agent.mu.Lock()
		defer env.agent.mu.Unlock()
		return len(env.agent.sessions) == 1
	})
	env.userSends("alex", "second query")
	env.userSends("alex", "third query")
	env.waitFor("three replies and idle", 3*time.Second, func() bool {
		env.agent.mu.Lock()
		session := env.agent.sessions[0]
		env.agent.mu.Unlock()
		return len(session.getSentPrompts()) == 3 && !env.engine.sessions.GetOrCreateActive("test:alex").Busy()
	})
	if !env.sentContains("first answer") || len(env.plat.getSent()) < 3 {
		t.Fatal("queued queries lost their answers")
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	var replies, queues int
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		var record map[string]any
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatal(err)
		}
		kind := record["msg"]
		if kind != "final reply timing" && kind != "queued message timing" {
			continue
		}
		if len(record["message_sha256"].(string)) != 64 || record["elapsed_ns"].(float64) < 0 {
			t.Fatal("missing exact message duration")
		}
		if kind == "final reply timing" {
			replies++
			if record["handler_returned"] != true {
				t.Fatal("final handler did not return")
			}
		} else {
			queues++
		}
		if strings.Contains(string(line), "query") || strings.Contains(string(line), "alex") || strings.Contains(string(line), "answer") {
			t.Fatal("private conversation leaked into timing")
		}
	}
	if replies != 3 || queues != 2 {
		t.Fatalf("missing correlated send/queue spans: replies=%d queues=%d", replies, queues)
	}
}
