package codex

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

func TestCodexRunAs_RequiresExplicitIdentityPaths(t *testing.T) {
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	opts := map[string]any{"cmd": bin, "work_dir": work, "codex_home": filepath.Join(work, "home"),
		"run_as_user": "_fixture", "run_as_env": []any{"HTTPS_PROXY"}, "backend": "app_server", "app_server_url": "stdio"}
	for _, key := range []string{"cmd", "work_dir", "codex_home"} {
		t.Run(key, func(t *testing.T) {
			value := opts[key]
			opts[key] = "relative"
			defer func() { opts[key] = value }()
			if _, err := New(opts); err == nil {
				t.Fatal("relative isolated runtime path accepted")
			}
		})
	}
	a, err := New(opts)
	if runtime.GOOS == "windows" {
		if err == nil {
			t.Fatal("Windows silently ignored run_as_user")
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	isolated := a.(*Agent)
	if isolated.GetRunAsUser() != "_fixture" || !slices.Contains(isolated.GetRunAsEnv(), "HTTPS_PROXY") {
		t.Fatal("identity options lost")
	}
	opts["app_server_url"] = "ws://127.0.0.1:3845"
	if _, err := New(opts); err == nil {
		t.Fatal("isolated network listener accepted")
	}
}

func TestCodexRunAs_DoesNotReadSupervisorCatalogOrSkills(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())
	a := &Agent{workDir: t.TempDir(), codexHome: t.TempDir(), model: "selected-model", activeIdx: -1,
		spawnOpts: core.SpawnOptions{RunAsUser: "_fixture"}}
	if got := a.AvailableModels(context.Background()); len(got) != 1 || got[0].Name != "selected-model" {
		t.Fatalf("isolated model choices: %v", got)
	}
	for _, dir := range a.SkillDirs() {
		if !strings.HasPrefix(dir, a.workDir+string(filepath.Separator)) && !strings.HasPrefix(dir, a.codexHome+string(filepath.Separator)) {
			t.Fatalf("supervisor skill path leaked: %s", dir)
		}
	}
}

func TestCodexRunAs_WorkspaceOptionsKeepPinnedCLIAndConfig(t *testing.T) {
	a := &Agent{cmd: "/fixed/codex", cliExtraArgs: []string{"--fixture"}, configEnv: []string{"HTTPS_PROXY=http://127.0.0.1:17890"},
		codexHome: "/employee/.codex", spawnOpts: core.SpawnOptions{RunAsUser: "_fixture", EnvAllowlist: []string{"HTTPS_PROXY"}}}
	opts := a.WorkspaceAgentOptions()
	if !slices.Equal(opts["cmd"].([]string), []string{"/fixed/codex", "--fixture"}) || opts["codex_home"] != a.codexHome {
		t.Fatal("workspace lost pinned CLI or auth location")
	}
	opts["cmd"].([]string)[0] = "modified"
	opts["env"].(map[string]string)["HTTPS_PROXY"] = "modified"
	env := a.GetRunAsEnv()
	env[0] = "modified"
	if a.cmd != "/fixed/codex" || a.configEnv[0] != "HTTPS_PROXY=http://127.0.0.1:17890" || a.GetRunAsEnv()[0] != "HTTPS_PROXY" {
		t.Fatal("workspace options alias parent state")
	}
}

func TestCodexRunAs_DoesNotWriteProviderCredentials(t *testing.T) {
	home := t.TempDir()
	a := &Agent{codexHome: home, activeIdx: 0, spawnOpts: core.SpawnOptions{RunAsUser: "_fixture"},
		providers: []core.ProviderConfig{{Name: "fixture", BaseURL: "https://example.invalid", APIKey: "synthetic-key"}}}
	if _, err := a.StartSession(context.Background(), ""); err == nil {
		t.Fatal("supervisor provider materialization accepted under isolation")
	}
	files, err := os.ReadDir(home)
	if err != nil || len(files) != 0 {
		t.Fatal("provider config or authentication written by supervisor")
	}
}
