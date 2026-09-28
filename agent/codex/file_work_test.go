package codex

import (
	"context"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"

	"github.com/chenhg5/cc-connect/core"
)

func TestCodexFileWorkPreservesRuntimeWithoutOldSession(t *testing.T) {
	base := t.TempDir()
	a := &Agent{cmd: filepath.Join(base, "codex"), workDir: filepath.Join(base, "project"), codexHome: filepath.Join(base, "auth"),
		fileWorkNative: true, mode: "full-auto", backend: "app_server", appServerURL: "stdio://", activeIdx: -1,
		model: "selected-model", reasoningEffort: "medium", configEnv: []string{"HTTPS_PROXY=http://127.0.0.1:17890"},
		spawnOpts: core.SpawnOptions{RunAsUser: "_fixture"}, sessionEnv: []string{"CC_FILE_ACTION_TOKEN=old-work"}}
	root := filepath.Join(base, "current.work with spaces")
	clone, err := a.ForFileWork(root)
	if runtime.GOOS == "windows" {
		if err == nil {
			t.Fatal("Windows native file runtime accepted")
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	child := clone.(*Agent)
	if child.cmd != a.cmd || child.codexHome != a.codexHome || child.workDir != a.workDir || child.model != a.model || child.reasoningEffort != "medium" ||
		child.spawnOpts.RunAsUser != "_fixture" || len(child.sessionEnv) != 0 || child.fileWorkRoot != root {
		t.Fatal("employee runtime lost or previous capability copied")
	}
	if !slices.Contains(child.configEnv, "CC_FILE_WORK_ROOT="+root) || !slices.Contains(child.spawnOpts.SudoEnvKeep, "CC_FILE_ACTION_TOKEN") ||
		!strings.Contains(strings.Join(child.cliExtraArgs, " "), "sandbox_workspace_write.writable_roots=") {
		t.Fatal("work root or protected capability handoff lost")
	}
	if _, err := child.StartSession(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "capability missing") {
		t.Fatalf("file session launched without current host capability: %v", err)
	}
	if len(a.cliExtraArgs) != 0 || len(a.spawnOpts.EnvAllowlist) != 0 || len(a.configEnv) != 1 {
		t.Fatal("parent configuration mutated")
	}
	a.permissionProfile = "anc-work"
	if runtime.GOOS == "darwin" {
		current, err := user.Current()
		if err != nil {
			t.Fatal(err)
		}
		a.spawnOpts.RunAsUser = current.Username
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	profileClone, err := a.ForFileWork(root)
	if err != nil {
		t.Fatal(err)
	}
	profileChild := profileClone.(*Agent)
	args := strings.Join(profileChild.cliExtraArgs, " ")
	if profileChild.permissionProfile != "anc-work" || !strings.Contains(args, "permissions={") || strings.Contains(args, "sandbox_workspace_write") {
		t.Fatalf("named profile overwritten by legacy file policy: %s", args)
	}
	var policy struct {
		Permissions map[string]struct {
			Roots   map[string]bool `toml:"workspace_roots"`
			Network struct {
				Sockets map[string]string `toml:"unix_sockets"`
			} `toml:"network"`
		}
	}
	if _, err := toml.Decode(profileChild.cliExtraArgs[len(profileChild.cliExtraArgs)-1], &policy); err != nil || !policy.Permissions["anc-work"].Roots[root] {
		t.Fatalf("invalid dynamic profile/root TOML: %v", err)
	}
	if runtime.GOOS == "darwin" {
		socket, err := macOfficeSocket(root, a.spawnOpts.RunAsUser)
		if err != nil || len(policy.Permissions["anc-work"].Network.Sockets) != 1 || policy.Permissions["anc-work"].Network.Sockets[socket] != "allow" {
			t.Fatalf("Office permission must select only the trusted work socket: %v", err)
		}
	}
	for _, mode := range []string{"suggest", "yolo"} {
		a.mode = mode
		if _, err := a.ForFileWork(root); err == nil {
			t.Fatalf("unsupported file mode accepted: %s", mode)
		}
	}
	a.mode = "full-auto"
	a.fileWorkNative = false
	if _, err := a.ForFileWork(root); err == nil {
		t.Fatal("disabled native runtime silently launched")
	}
}

func TestOfficeSocketMatchesCRMUnicodeURIAndIdentity(t *testing.T) {
	profile := "/private/var/fictional work/婚宴 %/outputs/.office-profile"
	want := "/private/tmp/OSL_PIPE_501_SingleOfficeIPC_786cfa4b1e5213eb148ce155790654d"
	if got := officeSocketName(profile, 501); got != want {
		t.Fatalf("Office socket naming protocol mismatch: %s", got)
	}
	if officeSocketName(profile, 502) == want || officeSocketName(profile+"-other", 501) == want {
		t.Fatal("socket must be work and UID specific")
	}
}

func TestOfficeSocketResolvesNativeIdentityAndCanonicalRoot(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS identity and path resolution")
	}
	current, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	alias := filepath.Join(t.TempDir(), "work alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	a, err := macOfficeSocket(root, current.Username)
	if err != nil {
		t.Fatal(err)
	}
	b, err := macOfficeSocket(alias, current.Username)
	if err != nil || a != b {
		t.Fatalf("root aliases must select one socket: %v", err)
	}
	for _, username := range []string{"root", "-u", "", "_anc_nonexistent_fixture_12345"} {
		if _, err := macOfficeSocket(root, username); err == nil {
			t.Fatalf("unknown/privileged identity accepted: %q", username)
		}
	}
	if _, err := macOfficeSocket(root+"/missing", current.Username); err == nil {
		t.Fatal("nonexistent work accepted")
	}
}
