//go:build !windows

package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func TestCodexRunAs_FiltersSupervisorEnvironment(t *testing.T) {
	t.Setenv("ANC_HOST_SECRET", "synthetic-host-secret")
	opts := core.SpawnOptions{RunAsUser: "_fixture", EnvAllowlist: []string{"CODEX_HOME", "HTTPS_PROXY"}}
	cmd := buildCodexCommand(context.Background(), opts, "/fixed/codex", []string{"app-server"}, "/work/with spaces",
		[]string{"CODEX_HOME=/employee/.codex", "HTTPS_PROXY=http://127.0.0.1:17890"})
	if filepath.Base(cmd.Path) != "sudo" || !slices.Contains(cmd.Args, "_fixture") || !slices.Contains(cmd.Args, "/fixed/codex") {
		t.Fatalf("missing identity wrapper: %v", cmd.Args)
	}
	for _, key := range []string{"HOME=", "PATH=", "ANC_HOST_SECRET="} {
		for _, entry := range cmd.Env {
			if strings.HasPrefix(entry, key) {
				t.Fatalf("host environment leaked: %s", key)
			}
		}
	}
	if !slices.Contains(cmd.Env, "CODEX_HOME=/employee/.codex") || !slices.Contains(cmd.Env, core.RunAsChdirEnv+"=/work/with spaces") {
		t.Fatal("isolated auth/workspace environment lost")
	}
}

// The CI fixture creates identities, then runs this test as the ordinary gateway.
func TestCodexRunAsOrdinaryIdentities(t *testing.T) {
	path := os.Getenv("ANC_CODEX_IDENTITY_FIXTURE")
	if path == "" {
		t.Skip("requires disposable CI identity fixture")
	}
	var f struct {
		Model, Other, Work, Home, Forbidden string
		GatewayUID, ModelUID                int
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() == 0 || os.Geteuid() != f.GatewayUID || f.GatewayUID == f.ModelUID {
		t.Fatal("fixture must run with distinct ordinary identities")
	}
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, backend := range []string{"app_server", "exec"} {
		t.Run(backend, func(t *testing.T) {
			opts := map[string]any{"cmd": []string{bin, "-test.run=^TestConfiguredCLIHelper$", "--", "anc-fake-codex"},
				"work_dir": f.Work, "codex_home": f.Home, "run_as_user": f.Model, "backend": backend, "app_server_url": "stdio",
				"run_as_env": []string{"ANC_EXPECT_CWD", "ANC_EXPECT_HOME", "ANC_EXPECT_UID", "ANC_FORBIDDEN"},
				"env":        map[string]string{"ANC_EXPECT_CWD": f.Work, "ANC_EXPECT_HOME": f.Home, "ANC_EXPECT_UID": f.Model, "ANC_FORBIDDEN": f.Forbidden}}
			a, err := New(opts)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			s, err := a.StartSession(ctx, "")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := s.Close(); err != nil {
					t.Error(err)
				}
			})
			if s.(interface{ GetModel() string }).GetModel() != "synthetic-model" {
				t.Fatal("isolated CLI failed")
			}
		})
	}
	// The gateway cannot launch an unassigned employee; no supervisor fallback.
	cmd := buildCodexCommand(context.Background(), core.SpawnOptions{RunAsUser: f.Other}, "/usr/bin/true", nil, f.Work, nil)
	if err := startCodexCommand(cmd, context.Background(), core.SpawnOptions{RunAsUser: f.Other}); err == nil {
		_ = cmd.Wait()
		t.Fatal("unassigned identity accepted")
	}
}
