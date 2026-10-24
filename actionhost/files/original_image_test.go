package files

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

func TestOriginalImageWorkBindingPreservesBytesAndScope(t *testing.T) {
	f := newFixture(t, nil)
	f.host.documentValidator = "fixed-host-validator"
	data := fictionalImage(t, "png")
	if path := os.Getenv("ANC_TEST_ORIGINAL_IMAGE"); path != "" {
		var err error
		data, err = os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
	}
	b := f.binding
	b.Principal.MessageID = "original-image"
	b.Inputs = []core.FileAttachment{{FileName: "original.png", Data: data, RequireSave: true}}
	w, err := f.host.Bind(context.Background(), b)
	if err != nil || len(w.Inputs) != 1 {
		t.Fatal(w, err)
	}
	stored, err := os.ReadFile(w.Inputs[0].Path)
	if err != nil || hash(stored) != hash(data) || w.Inputs[0].SHA256 != hash(data) {
		t.Fatal("original changed", err)
	}
	ref, err := f.host.FindByMessage(context.Background(), b.Principal, "original-image")
	if err != nil || ref.WorkID != w.WorkID || ref.SessionID != b.SessionID {
		t.Fatal("wrong work", err)
	}
	other := b.Principal
	other.UserID = "other-user"
	if _, err := f.host.FindByMessage(context.Background(), other, "original-image"); !errors.Is(err, ErrScope) {
		t.Fatal("foreign identity accessed original", err)
	}
}
