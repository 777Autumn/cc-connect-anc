//go:build linux || darwin

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/chenhg5/cc-connect/actionhost/files"
	"github.com/chenhg5/cc-connect/core"
)

func recoveryDigest(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

// The privileged parent only provisions a disposable fixture and read handles.
// Both invocations of the real command function run as the ordinary ledger UID.
func TestRecoverDeviceChangeAcrossUIDReadOnlyFiles(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires isolated privileged fixture setup; CI runs this explicitly")
	}
	const gateway, model = 65534, 65533
	rootTmp, err := filepath.EvalSymlinks("/tmp")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", rootTmp)
	base := t.TempDir()
	if err := os.Chmod(filepath.Dir(base), 0711); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"state", "snapshots", "work", "work/inputs", "work/outputs"} {
		if err := os.Mkdir(filepath.Join(base, p), 0700); err != nil {
			t.Fatal(err)
		}
	}
	ledger, snapshots := filepath.Join(base, "state/files.sqlite"), filepath.Join(base, "snapshots")
	if err := files.Initialize(ledger); err != nil {
		t.Fatal(err)
	}
	store, err := files.Open(ledger)
	if err != nil {
		t.Fatal(err)
	}
	host, err := files.New(store, snapshots, func(context.Context, json.RawMessage, core.FileAttachment, string) (string, error) {
		t.Fatal("unexpected send")
		return "", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	binding := files.Binding{Principal: core.ActionPrincipal{Platform: "fixture", Project: "fixture", UserID: "user", ChatID: "chat", SessionKey: "session", MessageID: "original"}, SessionID: "native", Route: json.RawMessage(`{"audience":"original"}`), WorkRoot: filepath.Join(base, "work"), OwnerUID: 0}
	work, err := host.Bind(context.Background(), binding)
	if err != nil {
		t.Fatal(err)
	}
	if err := host.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(work.OutputDir, "private.docx")
	content := []byte("synthetic model-owned private draft")
	if err := os.WriteFile(output, content, 0600); err != nil {
		t.Fatal(err)
	}
	if err := filepath.WalkDir(base, func(p string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		return os.Chown(p, gateway, gateway)
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(work.OutputDir, model, gateway); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(work.OutputDir, 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(output, model, gateway); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+ledger+"?mode=rw")
	if err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if err = db.QueryRow("SELECT payload FROM file_state WHERE id=1").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var state struct {
		Works map[string]map[string]json.RawMessage `json:"works"`
	}
	if err = json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	w := state.Works[work.WorkID]
	replace := func(key string, value any) {
		encoded, _ := json.Marshal(value)
		old := append([]byte(`"`+key+`":`), w[key]...)
		next := append([]byte(`"`+key+`":`), encoded...)
		if bytes.Count(raw, old) != 1 {
			t.Fatal("ambiguous fixture field", key)
		}
		raw = bytes.Replace(raw, old, next, 1)
		w[key] = encoded
	}
	replace("OwnerUID", model)
	entries := map[string]files.RecoveryEntry{}
	if err = filepath.WalkDir(work.WorkRoot, func(p string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		i, e := os.Lstat(p)
		if e != nil {
			return e
		}
		s := i.Sys().(*syscall.Stat_t)
		rel, e := filepath.Rel(work.WorkRoot, p)
		if e != nil {
			return e
		}
		rel = filepath.ToSlash(rel)
		entry := files.RecoveryEntry{Identity: fmt.Sprintf("%d:%d:%d:%d", s.Dev, s.Ino, s.Uid, s.Gid), Mode: uint32(s.Mode) & 07777, Directory: i.IsDir()}
		if !i.IsDir() {
			data, e := os.ReadFile(p)
			if e != nil {
				return e
			}
			entry.SHA256 = recoveryDigest(data)
		}
		entries[rel] = entry
		for name, field := range map[string]string{".": "RootIdentity", "inputs": "InputIdentity", "outputs": "OutputIdentity"} {
			if rel == name {
				replace(field, fmt.Sprintf("%d:%d:%d:%d", s.Dev+3, s.Ino, s.Uid, s.Gid))
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("UPDATE file_state SET payload=? WHERE id=1", string(raw)); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	request := files.DeviceRecoveryRequest{StateSHA256: recoveryDigest(raw), Works: []files.DeviceRecoveryWork{{Project: "fixture", WorkSHA256: recoveryDigest([]byte(work.WorkID)), Entries: entries}}}
	manifest, _ := json.Marshal(request)
	configPath := filepath.Join(base, "config.toml")
	config := []byte(fmt.Sprintf("[[projects]]\nname='fixture'\n[projects.file_work]\nenabled=true\nruntime='native'\nledger='%s'\nsnapshots='%s'\n", ledger, snapshots))
	if err = os.WriteFile(configPath, config, 0644); err != nil {
		t.Fatal(err)
	}
	// Copy the test executable out of the build system's private directory.
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	binary, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	childPath := filepath.Join(base, "command.test")
	if err = os.WriteFile(childPath, binary, 0755); err != nil {
		t.Fatal(err)
	}
	readPayload := func() []byte {
		d, e := sql.Open("sqlite", "file:"+ledger+"?mode=ro")
		if e != nil {
			t.Fatal(e)
		}
		defer func() {
			if err := d.Close(); err != nil {
				t.Error(err)
			}
		}()
		var b []byte
		if e = d.QueryRow("SELECT payload FROM file_state WHERE id=1").Scan(&b); e != nil {
			t.Fatal(e)
		}
		return b
	}
	for _, mode := range []string{"no_handles", "read_handles"} {
		cmd := exec.Command(childPath, "-test.run=^TestRecoveryCommandIdentityChild$", "-test.v")
		cmd.Env = append(os.Environ(), "ANC_RECOVERY_CHILD="+mode, "ANC_RECOVERY_CONFIG="+configPath, "ANC_RECOVERY_CONFIG_SHA="+recoveryDigest(config), "ANC_RECOVERY_PRIVATE="+output)
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: gateway, Gid: gateway, Groups: []uint32{gateway}}}
		cmd.Dir = base
		cmd.Stdin = bytes.NewReader(manifest)
		f, e := os.Open(output)
		if e != nil {
			t.Fatal(e)
		}
		cmd.ExtraFiles = []*os.File{f}
		result, e := cmd.CombinedOutput()
		_ = f.Close()
		if e != nil {
			t.Fatalf("%s: %v\n%s", mode, e, result)
		}
		if mode == "no_handles" && !bytes.Equal(raw, readPayload()) {
			t.Fatal("rejected recovery changed ledger")
		}
	}
	after := readPayload()
	for name, field := range map[string]string{".": "RootIdentity", "inputs": "InputIdentity", "outputs": "OutputIdentity"} {
		replace(field, entries[name].Identity)
	}
	if !bytes.Equal(raw, after) {
		t.Fatal("recovery changed more than the three identity fields")
	}
	i, err := os.Lstat(output)
	if err != nil || i.Mode().Perm() != 0600 || i.Sys().(*syscall.Stat_t).Uid != model {
		t.Fatal("private file permissions changed", err)
	}
	i, err = os.Lstat(ledger)
	if err != nil || i.Sys().(*syscall.Stat_t).Uid != gateway {
		t.Fatal("ledger owner changed", err)
	}
}

func TestRecoveryCommandIdentityChild(t *testing.T) {
	mode := os.Getenv("ANC_RECOVERY_CHILD")
	if mode == "" {
		t.Skip("subprocess only")
	}
	if os.Geteuid() != 65534 {
		t.Fatal("gateway identity required")
	}
	if f, err := os.Open(os.Getenv("ANC_RECOVERY_PRIVATE")); err == nil {
		if err := f.Close(); err != nil {
			t.Error(err)
		}
		t.Fatal("fixture did not deny ordinary path read")
	}
	args := []string{os.Getenv("ANC_RECOVERY_CONFIG"), os.Getenv("ANC_RECOVERY_CONFIG_SHA")}
	if mode == "read_handles" {
		args = append(args, "--inherited-output-fds="+strconv.Itoa(3))
	}
	result, err := recoverFileDevices(context.Background(), args, os.Stdin)
	if mode == "no_handles" {
		if !errors.Is(err, files.ErrUnavailable) {
			t.Fatal("missing permission failure", err)
		}
		return
	}
	if err != nil || result.BeforeSHA256 == result.AfterSHA256 || len(result.Works) != 1 || strings.TrimSpace(result.AfterSHA256) == "" {
		t.Fatal("original work recovery failed", err, result)
	}
	var closed syscall.Stat_t
	if err := syscall.Fstat(3, &closed); !errors.Is(err, syscall.EBADF) {
		t.Fatal("command retained its inherited descriptor", err)
	}
}
