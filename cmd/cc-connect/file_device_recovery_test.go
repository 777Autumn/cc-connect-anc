//go:build linux || darwin

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chenhg5/cc-connect/actionhost/files"
)

func TestRecoverFileDevicesGuardsConfigurationAndRunningInstance(t *testing.T) {
	for _, test := range []string{"hash", "symlink", "writable", "disabled", "foreign_project", "running", "duplicate_project", "oversized"} {
		t.Run(test, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			raw := []byte("[[projects]]\nname='fixture'\n[projects.file_work]\nenabled=true\nruntime='native'\nledger='/nonexistent/files.db'\nsnapshots='/nonexistent/snapshots'\n")
			if test == "disabled" {
				raw = bytes.Replace(raw, []byte("enabled=true"), []byte("enabled=false"), 1)
			}
			if test == "duplicate_project" {
				raw = append(raw, raw...)
			}
			if test == "oversized" {
				raw = bytes.Repeat([]byte("#"), 1<<20+1)
			}
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256(raw)
			expected := hex.EncodeToString(digest[:])
			if test == "hash" {
				expected = strings.Repeat("0", 64)
			}
			if test == "symlink" {
				link := path + "-link"
				if err := os.Symlink(path, link); err != nil {
					t.Fatal(err)
				}
				path = link
			}
			if test == "writable" {
				if err := os.Chmod(path, 0666); err != nil {
					t.Fatal(err)
				}
			}
			project := "fixture"
			if test == "foreign_project" {
				project = "foreign"
			}
			request := files.DeviceRecoveryRequest{StateSHA256: strings.Repeat("a", 64), Works: []files.DeviceRecoveryWork{{Project: project, WorkSHA256: strings.Repeat("b", 64), Entries: map[string]files.RecoveryEntry{}}}}
			manifest, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			if test == "running" {
				lock, err := AcquireInstanceLock(path)
				if err != nil {
					t.Fatal(err)
				}
				defer lock.Release()
			}
			_, err = recoverFileDevices(context.Background(), []string{path, expected}, bytes.NewReader(manifest))
			want := files.ErrInvalid
			switch test {
			case "disabled", "foreign_project", "duplicate_project":
				want = files.ErrScope
			case "running":
				want = files.ErrUnavailable
			}
			if !errors.Is(err, want) {
				t.Fatal("guard did not reject before ledger access", err, want)
			}
		})
	}
}
