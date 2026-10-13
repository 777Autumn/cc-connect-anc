package crmfollowup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

func TestBanquetContextJourneyWithRealCRMHost(t *testing.T) {
	root := os.Getenv("MYANC_SPIKE_CRM_ROOT")
	if root == "" {
		t.Skip("set MYANC_SPIKE_CRM_ROOT for the paired context contract")
	}
	pythonName := "python3"
	if runtime.GOOS == "windows" {
		pythonName = "python"
	}
	python, err := exec.LookPath(pythonName)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, user, chat, basis string
		stale, handoff          bool
	}{
		{name: "owner-current", user: "sender-1", chat: "chat-1", basis: "current", handoff: true},
		{name: "recipient-stale", user: "employee", chat: "employee-chat", basis: "stale", stale: true, handoff: true},
		{name: "shared-no-material-metadata", user: "sender-1", chat: "shared-group", stale: true},
		{name: "nonparty-no-material-metadata", user: "other", chat: "other-private", stale: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &Adapter{hostSecret: "offline-banquet-context-contract-000000001", toolsEnabled: true,
				run: func(ctx context.Context, _, command string, input []byte, env []string) ([]byte, error) {
					if command != "host-tool" {
						t.Fatalf("unexpected host command %s", command)
					}
					args := []string{"-B", "testdata/banquet_context_fixture.py", "--root", root}
					if tc.stale {
						args = append(args, "--stale")
					}
					cmd := exec.CommandContext(ctx, python, args...)
					cmd.Env, cmd.Stdin = env, bytes.NewReader(input)
					out, runErr := cmd.Output()
					if failure, ok := runErr.(*exec.ExitError); ok {
						return nil, fmt.Errorf("paired context fixture: %w: %s", runErr, failure.Stderr)
					}
					return out, runErr
				}}
			principal := core.ActionPrincipal{Platform: "feishu", UserID: tc.user, ChatID: tc.chat,
				Project: "claude-local", SessionKey: "fixture-context", MessageID: "fixture-message"}
			value, card, err := a.Tool(context.Background(), "banquet-context", json.RawMessage(`{"banquet_id":"recSales1"}`),
				principal, "offline-context-session-token-000000001", core.LangChinese)
			if err != nil || card != nil || value["materials_read"] != false {
				t.Fatalf("read-only context: %v %v %v", value, card, err)
			}
			if history, ok := value["history"].([]any); !ok || len(history) != 10 || value["history_has_more"] != true {
				t.Fatal("history window lost", value)
			}
			if tc.handoff {
				basis, ok := value["publication_basis"].(map[string]any)
				if !ok || basis["status"] != tc.basis || value["status"] != "found" {
					t.Fatal("publication basis lost across paired host boundary", value)
				}
				handoff := value["handoff"].(map[string]any)
				if _, found := handoff["files"]; found {
					t.Fatal("aggregate exposed file import instructions", value)
				}
			} else {
				for _, key := range []string{"handoff", "notifications", "publication_basis", "is_recipient"} {
					if _, found := value[key]; found {
						t.Fatal("aggregate leaked handoff metadata", key, value)
					}
				}
				if value["status"] != "partial" {
					t.Fatal("missing private source must be explicit", value)
				}
			}
		})
	}
}

func TestHandoffRevisionJourneyWithRealCRMHost(t *testing.T) {
	root := os.Getenv("MYANC_SPIKE_CRM_ROOT")
	if root == "" {
		t.Skip("set MYANC_SPIKE_CRM_ROOT for the paired revision contract")
	}
	pythonName := "python3"
	if runtime.GOOS == "windows" {
		pythonName = "python"
	}
	python, err := exec.LookPath(pythonName)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, user, purpose string
		stale, authorized   bool
	}{
		{name: "current-recipient", user: "employee", purpose: "current", authorized: true},
		{name: "stale-owner-reference", user: "sender-1", purpose: "revision", stale: true, authorized: true},
		{name: "stale-recipient-reference-denied", user: "employee", purpose: "revision", stale: true},
		{name: "stale-current-denied", user: "sender-1", purpose: "current", stale: true},
		{name: "forged-purpose-denied", user: "sender-1", purpose: "approve", stale: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &Adapter{hostSecret: "offline-banquet-context-contract-000000001",
				run: func(ctx context.Context, _, command string, input []byte, env []string) ([]byte, error) {
					if command != "host-handoff-access" {
						t.Fatalf("unexpected host command %s", command)
					}
					args := []string{"-B", "testdata/banquet_context_fixture.py", "--root", root, "--command", command}
					if tc.stale {
						args = append(args, "--stale")
					}
					cmd := exec.CommandContext(ctx, python, args...)
					cmd.Env, cmd.Stdin = env, bytes.NewReader(input)
					return cmd.Output()
				}}
			principal := core.ActionPrincipal{Platform: "feishu", UserID: tc.user, ChatID: "fixture-private",
				Project: "claude-local", SessionKey: "fixture-revision", MessageID: "fixture-message"}
			value, err := a.HandoffAccess(context.Background(), principal, "fixture-new-work", "bh_chg_paired_publication_0", tc.purpose)
			if err != nil && tc.authorized {
				t.Fatal(err)
			}
			if (value["status"] == "authorized") != tc.authorized {
				t.Fatal("unexpected host authorization", value)
			}
			if tc.authorized {
				source, ok := value["source"].(map[string]any)
				if !ok || source["purpose"] != tc.purpose || source["reference_only"] != (tc.purpose == "revision") || source["publication_basis_id"] != "chg_paired_publication" {
					t.Fatal("host source authorization markers lost", value)
				}
			} else {
				if err == nil && value["status"] != "blocked" && value["status"] != "failed" {
					t.Fatal("missing host refusal", value)
				}
				if _, found := value["source"]; found {
					t.Fatal("refusal leaked usable source", value)
				}
			}
		})
	}
}
