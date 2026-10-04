package crmfollowup

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

func TestPersonalConnectionAdapterBindsHostPrincipalAndSubject(t *testing.T) {
	var received map[string]any
	a := &Adapter{hostSecret: "fixture-host-secret", run: func(_ context.Context, _, command string, input []byte, _ []string) ([]byte, error) {
		if command != "host-connection-guard" {
			t.Fatalf("unexpected command %s", command)
		}
		if err := json.Unmarshal(input, &received); err != nil {
			t.Fatal(err)
		}
		return []byte(`{"status":"allowed"}`), nil
	}}
	p := core.ActionPrincipal{UserID: "fixture-owner", Project: "private", ChatID: "private"}
	if err := a.ConnectionGuard(context.Background(), "knowledge_read", p, "session", "work", "management"); err != nil {
		t.Fatal(err)
	}
	if received["knowledge_subject"] != "management" || received["work_id"] != "work" || received["principal"].(map[string]any)["user_id"] != p.UserID {
		t.Fatal("host binding lost")
	}
	if a.ConnectionOperation("重新连接") != "reconnect" || a.ConnectionOperation("连接我的飞书") != "connect" {
		t.Fatal("enrollment operations conflated")
	}
}

func TestPersonalConnectionMaintenanceDeliversBeforeAnotherAccountFails(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	delivered := false
	a := &Adapter{connectionUsers: []string{"owner", "staff"}, run: func(_ context.Context, _, command string, input []byte, _ []string) ([]byte, error) {
		var request map[string]string
		if err := json.Unmarshal(input, &request); err != nil {
			t.Fatal(err)
		}
		if command != "host-connection-tick" {
			t.Fatal("unexpected maintenance command")
		}
		if request["user_id"] == "owner" {
			return []byte(`{"changes":[{"user_id":"owner","status":"connected","version":4}]}`), nil
		}
		if !delivered {
			t.Fatal("account failure could erase an earlier invalidation")
		}
		cancel()
		return nil, errors.New("fixture offline")
	}}
	a.RunConnections(ctx, func(user string) {
		if user != "owner" {
			t.Fatal("wrong account invalidated")
		}
		delivered = true
	})
	if !delivered {
		t.Fatal("invalidation lost")
	}
}

func TestPersonalConnectionGuardDistinguishesTemporaryFromExpired(t *testing.T) {
	for _, code := range []string{"connection_temporarily_unavailable", "connection_session_expired", "personal_work_audience_denied"} {
		t.Run(code, func(t *testing.T) {
			a := &Adapter{run: func(context.Context, string, string, []byte, []string) ([]byte, error) {
				return json.Marshal(map[string]string{"status": "blocked", "code": code})
			}}
			err := a.ConnectionGuard(context.Background(), "work-resume", core.ActionPrincipal{}, "", "work", "")
			if err == nil || errors.Is(err, core.ErrConnectionTemporary) != (code == "connection_temporarily_unavailable") {
				t.Fatalf("incorrect guard classification: %s: %v", code, err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a := &Adapter{run: func(context.Context, string, string, []byte, []string) ([]byte, error) {
		return nil, ctx.Err()
	}}
	if !errors.Is(a.ConnectionGuard(ctx, "work-resume", core.ActionPrincipal{}, "", "work", ""), core.ErrConnectionTemporary) {
		t.Fatal("host call cancellation expired the connection")
	}
}
