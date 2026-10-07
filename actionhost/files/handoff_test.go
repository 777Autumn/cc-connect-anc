package files

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

func TestHandoffSnapshotsVerifyExactAcceptedVersionAndRejectOtherActor(t *testing.T) {
	f, target, _, source, _, _ := publicationFixture(t)
	selected := core.CasePublicationFile{DeliveryID: source.DeliveryID, Name: source.Name, SHA256: source.SHA256, Version: source.Version, MessageReceipt: source.MessageReceipt}
	check := func(p core.ActionPrincipal, file core.CasePublicationFile) bool {
		raw, _ := json.Marshal(map[string]any{"files": []core.CasePublicationFile{file}})
		result, err := f.host.Tool(context.Background(), "host-handoff-check", raw, p, source.WorkID)
		return err == nil && result["status"] == "verified"
	}
	if !check(f.binding.Principal, selected) {
		t.Fatal("accepted snapshot rejected")
	}
	if check(target.Principal, selected) {
		t.Fatal("another actor read a private snapshot")
	}
	selected.Version++
	if check(f.binding.Principal, selected) {
		t.Fatal("unaccepted version authorized")
	}
}
