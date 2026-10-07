package core

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type banquetFilesStub struct {
	fileHostStub
	broken bool
}

func (h *banquetFilesStub) Tool(ctx context.Context, command string, input json.RawMessage, p ActionPrincipal, work string) (map[string]any, error) {
	if command == "host-handoff-check" {
		if h.broken {
			return nil, errors.New("snapshot unavailable")
		}
		return map[string]any{"status": "verified"}, nil
	}
	return h.fileHostStub.Tool(ctx, command, input, p, work)
}

func TestBanquetSubmissionFreezesHostFilesAndRechecksBytesAndConnection(t *testing.T) {
	e, _, _, state := actionToolFixture(t)
	defer e.cancel()
	work := publicationWork()
	work.Artifacts[0].Name = "handoff.docx"
	files := &banquetFilesStub{fileHostStub: fileHostStub{toolResult: publicationResult(work)}}
	e.fileWorkHost = files
	connection := &connectionHostFixture{}
	e.SetConnectionHost(connection)
	input := json.RawMessage(`{"banquet_id":"recFixture","recipient_ref":"owner_fixture","delivery_ids":["selected"],"missing":"none"}`)
	ctx, err := e.banquetSubmissionContext(context.Background(), state.currentPrincipal, work.WorkID, input)
	if err != nil {
		t.Fatal(err)
	}
	selected, ok := TrustedBanquetContext(ctx)
	if !ok || selected.WorkID != work.WorkID || len(selected.Files) != 1 || selected.Files[0].SHA256 != work.Artifacts[0].SHA256 {
		t.Fatal("host did not freeze accepted bytes", selected)
	}
	check := BanquetFileCheck{Principal: state.currentPrincipal, WorkID: work.WorkID, Files: selected.Files}
	files.broken = true
	if CheckBanquetFiles(ctx, check) == nil {
		t.Fatal("missing snapshot accepted at confirmation")
	}
	files.broken = false
	connection.guardError = errors.New("expired or private source")
	if CheckBanquetFiles(ctx, check) == nil {
		t.Fatal("source connection/privacy guard bypassed")
	}
	// A previously published version keeps its file grant; recipient authority
	// is independently checked by the CRM host, without author's conversation.
	check.Published = true
	if err := CheckBanquetFiles(ctx, check); err != nil {
		t.Fatal("published snapshot depends on author session", err)
	}
}

func TestBanquetHandoffImportRequiresActualMatchingInputAndKeepsWorkIsolated(t *testing.T) {
	e, _, _, state := actionToolFixture(t)
	defer e.cancel()
	state.fileWorkID = "recipient-work"
	file := map[string]any{"publication_id": "bh_chg_fixture_0", "name": "handoff.docx", "sha256": strings.Repeat("a", 64)}
	for _, mode := range []string{"empty", "other-work", "wrong-hash", "accepted"} {
		work := FileWorkContext{Enabled: true, WorkID: state.fileWorkID}
		if mode != "empty" {
			work.Inputs = []FileWorkInput{{Name: "handoff.docx", SHA256: strings.Repeat("a", 64), Source: &FileWorkSource{PublicationID: "bh_chg_fixture_0"}}}
		}
		if mode == "other-work" {
			work.WorkID = "author-work"
		}
		if mode == "wrong-hash" {
			work.Inputs[0].SHA256 = strings.Repeat("b", 64)
		}
		e.fileWorkHost = &fileHostStub{toolResult: publicationResult(work)}
		result := map[string]any{"handoff": map[string]any{"files": []any{file}}}
		err := e.importHandoffFiles(context.Background(), state.actionToken, state.currentPrincipal, result)
		if (err == nil) != (mode == "accepted") {
			t.Fatal(mode, err, result)
		}
		if err == nil && result["materials_imported"] != true {
			t.Fatal("accepted version not exposed")
		}
	}
}
