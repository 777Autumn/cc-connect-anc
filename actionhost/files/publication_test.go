package files

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

func publicationFixture(t *testing.T) (fixture, Binding, WorkContext, core.CasePublishedSource, []byte, *int) {
	t.Helper()
	sends := 0
	f := newFixture(t, func(context.Context, json.RawMessage, core.FileAttachment, string) (string, error) {
		sends++
		return fmt.Sprintf("private-receipt-%d", sends), nil
	})
	data := output(t, f, "xlsx")
	result, err := deliver(t, f, "result.xlsx", data, 0)
	if err != nil {
		t.Fatal(err)
	}
	p := f.binding.Principal
	grant := core.CasePublishedSource{Platform: p.Platform, UserID: p.UserID, ChatID: p.ChatID, Project: p.Project, SessionKey: p.SessionKey, WorkID: f.context.WorkID, DeliveryID: result["delivery_id"].(string), Name: "result.xlsx", SHA256: hash(data), Version: 1, MessageReceipt: "private-receipt-1"}
	target := f.binding
	target.SessionID = "other-private-work"
	target.Principal.UserID = "staff-b"
	target.Principal.ChatID = "private-b"
	target.Principal.SessionKey = "session-b"
	target.Principal.MessageID = "request-b"
	base := t.TempDir()
	if err = os.Chmod(base, 0700); err != nil {
		t.Fatal(err)
	}
	target.WorkRoot, err = PrepareWork(base, target.SessionID, os.Geteuid())
	if err != nil {
		t.Fatal(err)
	}
	work, err := f.host.Bind(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	return f, target, work, grant, data, &sends
}

func TestPublishedPrivateFileImportsSnapshotWithoutInventingGroupOrResending(t *testing.T) {
	f, target, work, grant, data, sends := publicationFixture(t)
	ctx := context.Background()
	input := json.RawMessage(`{"publication_id":"pub_selected"}`)
	if _, err := f.host.Tool(ctx, "host-case-import", input, target.Principal, work.WorkID); err == nil {
		t.Fatal("publication without authorized CRM accepted")
	}
	f.host.SetPublicationAccess(func(_ context.Context, p core.ActionPrincipal, id, pub string) (core.CasePublishedSource, error) {
		if p != target.Principal || id != work.WorkID || pub != "pub_selected" {
			return core.CasePublishedSource{}, ErrScope
		}
		return grant, nil
	})
	// The mutable output is not the selected, saved artifact.
	if err := os.WriteFile(filepath.Join(f.context.OutputDir, "result.xlsx"), []byte("changed output"), 0600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		result, err := f.host.Tool(ctx, "host-case-import", input, target.Principal, work.WorkID)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(result)
		var current WorkContext
		_ = json.Unmarshal(raw, &current)
		if len(current.Inputs) != 1 || current.Inputs[0].Source.PublicationID != "pub_selected" {
			t.Fatal(current)
		}
		actual, err := os.ReadFile(current.Inputs[0].Path)
		if err != nil || !bytes.Equal(actual, data) {
			t.Fatal("wrong imported bytes", err)
		}
		if strings.Contains(string(raw), grant.MessageReceipt) || strings.Contains(string(raw), grant.WorkID) {
			t.Fatal("private provenance exposed", string(raw))
		}
	}
	for _, request := range []string{`{"publication_id":"pub_unselected"}`, `{"receipt":"private-receipt-1"}`, `{"publication_id":"pub_selected","receipt":"private-receipt-1"}`, `{"publication_id":"pub_selected","group_realm":"forged"}`} {
		if _, err := f.host.Tool(ctx, "host-case-import", json.RawMessage(request), target.Principal, work.WorkID); err == nil {
			t.Fatal("unauthorized reference", request)
		}
	}
	if *sends != 1 {
		t.Fatal("publication/import resent a file", *sends)
	}
	if err := f.store.change(ctx, func(st *state) error {
		source, destination := st.Works[grant.WorkID], st.Works[work.WorkID]
		if source.GroupRealm != "" || destination.GroupRealm != "" || destination.ProjectImportRealm != "" || st.Schema != 4 || !validState(*st) {
			t.Fatal("invented realm or invalid provenance")
		}
		provenance := destination.Inputs[0].Source
		if provenance.WorkID != grant.WorkID || provenance.DeliveryID != grant.DeliveryID || provenance.MessageReceipt != grant.MessageReceipt {
			t.Fatal("private provenance not retained")
		}
		st.Schema = 3
		if validState(*st) {
			t.Fatal("old schema silently erases publication provenance")
		}
		st.Schema = 4
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if ref, err := f.host.FindByMessage(ctx, target.Principal, grant.MessageReceipt); err == nil {
		t.Fatal("private source receipt became recipient message", ref)
	}
	reopened, err := Open(f.database)
	if err != nil {
		t.Fatal("persisted publication unreadable", err)
	}
	_ = reopened.Close()
}

func TestPublicationImportRechecksSourceAndMembershipBeforeBytes(t *testing.T) {
	f, target, work, grant, _, _ := publicationFixture(t)
	ctx := context.Background()
	input := json.RawMessage(`{"publication_id":"pub_selected"}`)
	for _, field := range []string{"work", "actor", "chat", "session", "project", "delivery", "version", "hash", "name", "receipt"} {
		bad := grant
		switch field {
		case "work":
			bad.WorkID = "other"
		case "actor":
			bad.UserID = "other"
		case "chat":
			bad.ChatID = "other"
		case "session":
			bad.SessionKey = "other"
		case "project":
			bad.Project = "other"
		case "delivery":
			bad.DeliveryID = "other"
		case "version":
			bad.Version++
		case "hash":
			bad.SHA256 = strings.Repeat("b", 64)
		case "name":
			bad.Name = "other.xlsx"
		case "receipt":
			bad.MessageReceipt = "other"
		}
		f.host.SetPublicationAccess(func(context.Context, core.ActionPrincipal, string, string) (core.CasePublishedSource, error) {
			return bad, nil
		})
		if _, err := f.host.Tool(ctx, "host-case-import", input, target.Principal, work.WorkID); err == nil {
			t.Fatal("source mismatch accepted", field)
		}
	}
	calls := 0
	f.host.SetPublicationAccess(func(context.Context, core.ActionPrincipal, string, string) (core.CasePublishedSource, error) {
		calls++
		if calls > 1 {
			return core.CasePublishedSource{}, ErrScope
		}
		return grant, nil
	})
	if _, err := f.host.Tool(ctx, "host-case-import", input, target.Principal, work.WorkID); err == nil || calls != 2 {
		t.Fatal("membership not rechecked before copying", err, calls)
	}
	entries, err := os.ReadDir(filepath.Join(target.WorkRoot, "inputs"))
	if err != nil || len(entries) != 0 {
		t.Fatal("bytes published after authorization revoked", err)
	}
	if _, err := f.host.ActivateInputs(ctx, target.Principal, work.WorkID); err == nil {
		t.Fatal("recovered pending import skipped authorization")
	}
}
