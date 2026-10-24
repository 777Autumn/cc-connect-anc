package files

import (
	"context"
	"errors"
	"github.com/chenhg5/cc-connect/core"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestVideoOriginalBindingAndForeignScope(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("ordinary POSIX validator contract")
	}
	f := newFixture(t, nil)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(dir, "validator")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\ncat >/dev/null\nprintf 'VIDEO_OK_V1\\n'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := f.host.SetVideoFormat(helper); err != nil {
		t.Fatal(err)
	}
	// This verifies binding after a validator receipt, not actual video decoding.
	data := []byte("synthetic validated video original")
	b := f.binding
	b.Principal.MessageID = "video-original"
	b.Inputs = []core.FileAttachment{{FileName: "original.mp4", Data: data, RequireSave: true}}
	w, err := f.host.Bind(context.Background(), b)
	if err != nil || len(w.Inputs) != 1 {
		t.Fatal(w, err)
	}
	stored, err := os.ReadFile(w.Inputs[0].Path)
	if err != nil || hash(stored) != hash(data) || w.Inputs[0].SHA256 != hash(data) {
		t.Fatal("original changed", err)
	}
	ref, err := f.host.FindByMessage(context.Background(), b.Principal, b.Principal.MessageID)
	if err != nil || ref.WorkID != w.WorkID || ref.SessionID != b.SessionID {
		t.Fatal("wrong work", err)
	}
	other := b.Principal
	other.UserID = "other-user"
	if _, err := f.host.FindByMessage(context.Background(), other, b.Principal.MessageID); !errors.Is(err, ErrScope) {
		t.Fatal("foreign scope", err)
	}
}

func TestVideoDefaultOffAndSizeBoundary(t *testing.T) {
	h := &Host{videoValidator: "not-executed"}
	for _, data := range [][]byte{nil, make([]byte, MaxFileBytes+1)} {
		if !errors.Is(h.validateFile(context.Background(), "video.mp4", data), ErrInvalid) {
			t.Fatal("video budget disabled")
		}
	}
	if (&Host{}).validateFile(context.Background(), "video.mp4", []byte("synthetic")) == nil {
		t.Fatal("video enabled by default")
	}
	if h.SetVideoFormat("/missing/fixed-validator") == nil {
		t.Fatal("unprotected validator accepted")
	}
}

func TestVideoFixedValidatorProtocolAndTimeout(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("ordinary POSIX validator contract")
	}
	for _, tc := range []struct {
		name, body string
		want       error
	}{
		{"pass", "cat >/dev/null; test -z \"$PYTHONPATH\" || exit 2; printf 'VIDEO_OK_V1\\n'", nil},
		{"invalid", "exit 1", ErrInvalid}, {"unavailable", "exit 2", ErrUnavailable},
		{"wrong_protocol", "printf PDF_OK_V1", ErrUnavailable},
		{"oversized_reply", "printf '%0100d' 1", ErrUnavailable},
		{"timeout", "sleep 30 & wait", ErrUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Chmod(dir, 0700); err != nil {
				t.Fatal(err)
			}
			helper := filepath.Join(dir, "validator")
			if err := os.WriteFile(helper, []byte("#!/bin/sh\n"+tc.body+"\n"), 0700); err != nil {
				t.Fatal(err)
			}
			h := &Host{}
			if err := h.SetVideoFormat(helper); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if tc.name == "timeout" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 100*time.Millisecond)
				defer cancel()
			}
			if err := h.validateFile(ctx, "video.mp4", []byte("synthetic stdin protocol fixture")); !errors.Is(err, tc.want) {
				t.Fatalf("got %v want %v", err, tc.want)
			}
		})
	}
}
