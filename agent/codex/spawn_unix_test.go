//go:build !windows

package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

type syntheticSudoCheck struct{}

func (syntheticSudoCheck) Run(_ context.Context, args ...string) ([]byte, error) {
	if slices.Contains(args, "sudo") {
		return nil, errors.New("synthetic model cannot escalate")
	}
	return nil, nil
}

func TestCodexRunAs_FiltersSupervisorEnvironment(t *testing.T) {
	core.ResetVerifyCache()
	t.Cleanup(core.ResetVerifyCache)
	if err := core.VerifyRunAsUserCheap(context.Background(), syntheticSudoCheck{}, "_fixture"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANC_HOST_SECRET", "synthetic-host-secret")
	opts := core.SpawnOptions{RunAsUser: "_fixture", EnvAllowlist: []string{"CODEX_HOME", "HTTPS_PROXY", "MYANC_CRM_HOST_SECRET"}}
	cmd, err := buildCodexCommand(context.Background(), opts, "/fixed/codex", []string{"app-server"}, "/work/with spaces",
		[]string{"CODEX_HOME=/employee/.codex", "HTTPS_PROXY=http://127.0.0.1:17890", "MYANC_CRM_HOST_SECRET=synthetic-must-not-inherit",
			"MYANC_CRM_ACTION_TOKEN=synthetic-stage-capability", "MYANC_CRM_STAGE_INPUT=/work/stage.json"})
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(cmd.Path) != "sudo" || !slices.Contains(cmd.Args, "_fixture") || !slices.Contains(cmd.Args, "/fixed/codex") {
		t.Fatalf("missing identity wrapper: %v", cmd.Args)
	}
	if cmd.Dir != "" {
		t.Fatal("supervisor must not enter the employee's private workspace before sudo")
	}
	for _, key := range []string{"HOME=", "PATH=", "ANC_HOST_SECRET=", "MYANC_CRM_HOST_SECRET="} {
		for _, entry := range cmd.Env {
			if strings.HasPrefix(entry, key) {
				t.Fatalf("host environment leaked: %s", key)
			}
		}
	}
	if !slices.Contains(cmd.Env, "CODEX_HOME=/employee/.codex") || !slices.Contains(cmd.Env, core.RunAsChdirEnv+"=/work/with spaces") {
		t.Fatal("isolated auth/workspace environment lost")
	}
	if !slices.Contains(cmd.Env, "MYANC_CRM_ACTION_TOKEN=synthetic-stage-capability") || !slices.Contains(cmd.Env, "MYANC_CRM_STAGE_INPUT=/work/stage.json") {
		t.Fatal("host-scoped stage environment lost")
	}
	for _, arg := range cmd.Args {
		if strings.Contains(arg, "synthetic-stage-capability") || (strings.HasPrefix(arg, "--preserve-env=") && strings.Contains(arg, "MYANC_CRM_ACTION_TOKEN")) {
			t.Fatal("stage capability would enter sudo audit arguments")
		}
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
	t.Run("private_session_access", func(t *testing.T) {
		if _, err := os.ReadDir(f.Home); !errors.Is(err, os.ErrPermission) {
			t.Fatal("gateway must not read model home", err)
		}
		// The native fixture runs as the model; the supervisor sees only the
		// scoped protocol result, never credentials or transcript paths.
		agent, err := New(map[string]any{
			"cmd":      []string{bin, "-test.run=^TestCodexSessionCLIHelper$", "--", "anc-session-cli"},
			"work_dir": f.Work, "codex_home": f.Home, "run_as_user": f.Model, "backend": "app_server", "app_server_url": "stdio",
			"run_as_env": []string{"ANC_SESSION_CWD", "ANC_SESSION_UID", "ANC_SESSION_DELETE"},
			"env":        map[string]string{"ANC_SESSION_CWD": f.Work, "ANC_SESSION_UID": f.Model, "ANC_SESSION_DELETE": filepath.Join(f.Home, "deleted")},
		})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		sessions, err := agent.ListSessions(ctx)
		if err != nil || len(sessions) != 1 || sessions[0].ID != "own" {
			t.Fatalf("ordinary native list: %+v / %v", sessions, err)
		}
		history, err := agent.(core.HistoryProvider).GetSessionHistory(ctx, "own", 1)
		if err != nil || len(history) != 1 || history[0].Content != "second" {
			t.Fatalf("ordinary history: %+v / %v", history, err)
		}
		if err := agent.(core.SessionDeleter).DeleteSession(ctx, "own"); err != nil {
			t.Fatal(err)
		}
		if _, err := os.ReadFile(filepath.Join(f.Home, "synthetic-session-access")); !errors.Is(err, os.ErrPermission) {
			t.Fatal("model session storage exposed to gateway", err)
		}
	})
	for _, variant := range []string{"app_server", "exec", "file_app_server"} {
		t.Run(variant, func(t *testing.T) {
			backend := variant
			if variant == "file_app_server" {
				backend = "app_server"
			}
			opts := map[string]any{"cmd": []string{bin, "-test.run=^TestConfiguredCLIHelper$", "--", "anc-fake-codex"},
				"work_dir": f.Work, "codex_home": f.Home, "run_as_user": f.Model, "backend": backend, "app_server_url": "stdio", "mode": "full-auto", "file_work_native": true,
				"run_as_env": []string{"ANC_EXPECT_CWD", "ANC_EXPECT_HOME", "ANC_EXPECT_UID", "ANC_FORBIDDEN"},
				"env":        map[string]string{"ANC_EXPECT_CWD": f.Work, "ANC_EXPECT_HOME": f.Home, "ANC_EXPECT_UID": f.Model, "ANC_FORBIDDEN": f.Forbidden}}
			if variant == "file_app_server" {
				opts["permission_profile"] = "anc-work"
			}
			a, err := New(opts)
			if err != nil {
				t.Fatal(err)
			}
			if variant == "file_app_server" {
				a, err = a.(core.FileWorkAgent).ForFileWork(f.Work)
				if err != nil {
					t.Fatal(err)
				}
				if runtime.GOOS == "darwin" && !strings.Contains(strings.Join(a.(*Agent).cliExtraArgs, " "), fmt.Sprintf("/OSL_PIPE_%d_", f.ModelUID)) {
					t.Fatal("Office permission must use the model UID, not the ordinary gateway UID")
				}
				a.(*Agent).SetSessionEnv([]string{"CC_FILE_ACTION_TOKEN=synthetic-file-capability"})
			} else {
				a.(*Agent).SetSessionEnv([]string{"MYANC_CRM_ACTION_TOKEN=synthetic-stage-capability", "MYANC_CRM_STAGE_INPUT=" + f.Work + "/stage.json"})
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
			if backend == "exec" {
				if err := s.Send("synthetic request", "fixture", nil, nil); err != nil {
					t.Fatal(err)
				}
				for {
					select {
					case event, ok := <-s.Events():
						if !ok || event.Type == core.EventError {
							t.Fatalf("isolated exec failed: %+v", event)
						}
						if event.Type == core.EventResult && event.Done {
							return
						}
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					}
				}
			}
		})
	}
	// The gateway cannot launch an unassigned employee; no supervisor fallback.
	if _, err := buildCodexCommand(context.Background(), core.SpawnOptions{RunAsUser: f.Other}, "/usr/bin/true", nil, f.Work, nil); err == nil {
		t.Fatal("unassigned identity accepted")
	}
}

func TestCodexRunAs_RejectedIdentityDoesNotLeakPipes(t *testing.T) {
	// No sudo binary: every identity check must fail, without allocating pipes.
	t.Setenv("PATH", t.TempDir())
	core.ResetVerifyCache()
	t.Cleanup(core.ResetVerifyCache)
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	count := func(t *testing.T) int {
		dir, err := os.Open("/dev/fd")
		if err != nil {
			t.Fatal(err)
		}
		entries, err := dir.Readdirnames(-1)
		closeErr := dir.Close()
		if err != nil || closeErr != nil {
			t.Fatalf("count descriptors: %v / %v", err, closeErr)
		}
		return len(entries)
	}
	for _, backend := range []string{"app_server", "exec"} {
		t.Run(backend, func(t *testing.T) {
			before := count(t)
			for i := 0; i < 16; i++ {
				a := &Agent{cmd: bin, backend: backend, appServerURL: "stdio://", workDir: t.TempDir(), activeIdx: -1,
					spawnOpts: core.SpawnOptions{RunAsUser: "_denied"}}
				s, err := a.StartSession(context.Background(), "")
				if backend == "app_server" {
					if err == nil {
						_ = s.Close()
						t.Fatal("identity failure ignored")
					}
					continue
				}
				if err != nil {
					t.Fatal(err)
				}
				if err := s.Send("synthetic", "fixture", nil, nil); err == nil {
					t.Fatal("exec identity failure ignored")
				}
				if s.(interface{ GetModel() string }).GetModel() != "" {
					t.Fatal("config inspection identity failure ignored")
				}
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if after := count(t); after > before+2 {
				t.Fatalf("rejected launches leaked descriptors: %d -> %d", before, after)
			}
		})
	}
}
