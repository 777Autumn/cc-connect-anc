package crmfollowup

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"testing"
)

func TestConnectionStartupJourneyWithRealCRMHost(t *testing.T) {
	root := os.Getenv("MYANC_SPIKE_CRM_ROOT")
	if root == "" {
		t.Skip("set MYANC_SPIKE_CRM_ROOT for the offline paired-source startup contract")
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
		name, mode string
		route      ConnectionRoute
		want       bool
	}{
		{name: "legacy", mode: "legacy", route: ConnectionRoute{AppID: "bot-app"}, want: true},
		{name: "personal-private", mode: "personal", route: ConnectionRoute{Project: "personal-bot", AppID: "personal-app", TenantKey: "fixture-tenant", MemberID: "bot-manager", UserID: "personal-open-id", ChatID: "personal-chat", Audience: "private"}, want: true},
		{name: "personal-shared", mode: "personal", route: ConnectionRoute{Project: "manager-group", AppID: "bot-app", TenantKey: "fixture-tenant", MemberID: "bot-manager", UserID: "bot-manager", ChatID: "shared-group", Audience: "shared"}, want: true},
		{name: "group-is-not-wire-audience", mode: "personal", route: ConnectionRoute{Project: "manager-group", AppID: "bot-app", TenantKey: "fixture-tenant", MemberID: "bot-manager", UserID: "bot-manager", ChatID: "shared-group", Audience: "group"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &Adapter{hostSecret: "offline-connection-contract-secret-000000001", run: func(ctx context.Context, _, command string, input []byte, env []string) ([]byte, error) {
				if command != "host-connection-config" {
					t.Fatalf("unexpected command %s", command)
				}
				cmd := exec.CommandContext(ctx, python, "-B", "testdata/connection_fixture.py", "--root", root, "--mode", tc.mode)
				cmd.Env = env
				cmd.Stdin = bytes.NewReader(input)
				out, err := cmd.Output()
				if failure, ok := err.(*exec.ExitError); ok {
					return nil, fmt.Errorf("real CRM fixture: %w: %s", err, failure.Stderr)
				}
				return out, err
			}}
			enabled, err := a.ConfigureConnections(context.Background(), tc.route)
			if enabled != tc.want || (err == nil) != tc.want {
				t.Fatalf("real host startup: enabled=%t err=%v", enabled, err)
			}
			if enabled && (len(a.connectionUsers) != 1 || a.connectionUsers[0] != "bot-manager") {
				t.Fatal("real host member maintenance identities lost", a.connectionUsers)
			}
		})
	}
}
