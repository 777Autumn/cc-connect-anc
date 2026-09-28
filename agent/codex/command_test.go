package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// Run this test binary as the configured CLI: no installed Codex or account is used.
func TestConfiguredCLIHelper(t *testing.T) {
	if !slices.Contains(os.Args, "anc-fake-codex") {
		return
	}
	if !slices.Contains(os.Args, "app-server") && !slices.Contains(os.Args, "exec") {
		os.Exit(2)
	}
	cwd, err := os.Getwd()
	actual, statErr := os.Stat(cwd)
	expected, expectedErr := os.Stat(os.Getenv("ANC_EXPECT_CWD"))
	if err != nil || statErr != nil || expectedErr != nil || !os.SameFile(actual, expected) || os.Getenv("CODEX_HOME") != os.Getenv("ANC_EXPECT_HOME") {
		os.Exit(3)
	}
	if name := os.Getenv("ANC_EXPECT_UID"); name != "" {
		identity, err := user.Current()
		if err != nil || identity.Username != name || identity.Uid == "0" {
			os.Exit(6)
		}
		for _, path := range strings.Split(os.Getenv("ANC_FORBIDDEN"), string(os.PathListSeparator)) {
			if _, err := os.ReadFile(path); !errors.Is(err, os.ErrPermission) {
				os.Exit(7)
			}
		}
	}
	if os.Getenv("CC_FILE_TRANSPORT") == "native" {
		if os.Getenv("CC_FILE_ACTION_TOKEN") != "synthetic-file-capability" || os.Getenv("CC_FILE_WORK_ROOT") != os.Getenv("ANC_EXPECT_CWD") ||
			strings.Contains(strings.Join(os.Args, " "), "synthetic-file-capability") {
			os.Exit(8)
		}
	} else if os.Getenv("ANC_EXPECT_UID") != "" {
		if os.Getenv("MYANC_CRM_ACTION_TOKEN") != "synthetic-stage-capability" || os.Getenv("MYANC_CRM_STAGE_INPUT") != os.Getenv("ANC_EXPECT_CWD")+"/stage.json" {
			os.Exit(9)
		}
	}
	scanner := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	if slices.Contains(os.Args, "exec") {
		for _, event := range []map[string]any{
			{"type": "thread.started", "thread_id": "synthetic-thread"}, {"type": "turn.completed"},
		} {
			if err := encoder.Encode(event); err != nil {
				os.Exit(5)
			}
		}
		os.Exit(0)
	}
	for scanner.Scan() {
		var req struct {
			ID     any            `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if json.Unmarshal(scanner.Bytes(), &req) != nil {
			os.Exit(4)
		}
		if req.ID == nil {
			continue
		}
		var result any = map[string]any{}
		switch req.Method {
		case "config/read":
			result = map[string]any{"config": map[string]any{"model": "synthetic-model", "model_reasoning_effort": "medium"}}
		case "thread/start", "thread/resume":
			if profile := os.Getenv("ANC_EXPECT_PROFILE"); profile != "" && (req.Params["permissions"] != profile || req.Params["sandbox"] != nil) {
				os.Exit(10)
			}
			result = map[string]any{"thread": map[string]any{"id": "synthetic-thread"}, "cwd": cwd, "model": "synthetic-model", "reasoningEffort": "medium"}
		}
		if encoder.Encode(map[string]any{"id": req.ID, "result": result}) != nil {
			os.Exit(5)
		}
	}
	os.Exit(0)
}

func TestConfiguredCLI_AppServerAndRuntimeConfig(t *testing.T) {
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// An empty PATH proves that neither path falls back to a different installed CLI.
	t.Setenv("PATH", t.TempDir())
	for _, backend := range []string{"app_server", "exec"} {
		t.Run(backend, func(t *testing.T) {
			workDir := t.TempDir()
			home := filepath.Join(workDir, "codex-home")
			opts := map[string]any{"cmd": []string{bin, "-test.run=^TestConfiguredCLIHelper$", "--", "anc-fake-codex"},
				"backend": backend, "app_server_url": "stdio", "work_dir": workDir, "codex_home": home,
				"env": map[string]string{"ANC_EXPECT_CWD": workDir, "ANC_EXPECT_HOME": home}}
			if backend == "app_server" {
				opts["mode"], opts["permission_profile"] = "full-auto", "anc-fixture"
				opts["env"].(map[string]string)["ANC_EXPECT_PROFILE"] = "anc-fixture"
			}
			a, err := New(opts)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			s, err := a.StartSession(ctx, "")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := s.Close(); err != nil {
					t.Errorf("close session: %v", err)
				}
			})
			runtime := s.(interface {
				GetModel() string
				GetReasoningEffort() string
			})
			if runtime.GetModel() != "synthetic-model" || runtime.GetReasoningEffort() != "medium" {
				t.Fatalf("configured CLI was not used: model=%q effort=%q", runtime.GetModel(), runtime.GetReasoningEffort())
			}
			if backend == "app_server" {
				opts = a.(*Agent).WorkspaceAgentOptions()
				opts["work_dir"] = workDir
				a, err = New(opts)
				if err != nil {
					t.Fatal(err)
				}
				resumed, err := a.StartSession(ctx, "synthetic-thread")
				if err != nil {
					t.Fatal(err)
				}
				if err := resumed.Close(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
