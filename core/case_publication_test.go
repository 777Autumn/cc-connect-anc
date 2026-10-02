package core

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func publicationWork() FileWorkContext {
	return FileWorkContext{Enabled: true, WorkID: "fixture-work", Artifacts: []FileWorkArtifact{{DeliveryID: "selected", Name: "budget.xlsx", SHA256: strings.Repeat("a", 64), Version: 2, Status: "accepted", MessageReceipt: "private-receipt"}}}
}

func publicationResult(work FileWorkContext) map[string]any {
	raw, _ := json.Marshal(work)
	var result map[string]any
	_ = json.Unmarshal(raw, &result)
	return result
}

const publicationInput = `{"case_name":"Fixture case","expected_revision":1,"delivery_ids":["selected"]}`

func TestCasePublicationFreezesOnlyAcceptedFilesAndRechecksBeforeConfirmation(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(map[bool]string{false: "unchanged", true: "changed-after-question"}[changed], func(t *testing.T) {
			e, h, p, state, s := caseQuestionFixture(t)
			files := &fileHostStub{toolResult: publicationResult(publicationWork())}
			e.fileWorkHost = files
			body := `{"command":"case-publish","input":` + publicationInput + `}`
			w := actionToolRequest(e.ActionToolHandler(), "POST", "/tool", state.actionToken, body)
			if w.Code != 200 || !strings.Contains(w.Body.String(), "awaiting_confirmation") {
				t.Fatal(w.Code, w.Body.String())
			}
			q := s.caseQuestion()
			saved := NewSessionManager(e.sessions.storePath).FindByID(s.ID).caseQuestion()
			if len(q.Context.PublicationFiles) != 1 || q.Context.PublicationFiles[0].MessageReceipt != "private-receipt" || !reflect.DeepEqual(q.Context.PublicationFiles, saved.Context.PublicationFiles) {
				t.Fatal("selection not frozen", q)
			}
			h.toolResult = map[string]any{"status": "recorded"}
			if changed {
				work := publicationWork()
				work.Artifacts[0].Status = "unknown"
				files.toolResult = publicationResult(work)
			}
			m := answerFixture(q, "askq:0:1", true)
			e.handleCaseAnswer(p, m, m.Content)
			if changed {
				if h.toolCalls != 1 || s.caseQuestion().Status != "needs_review" {
					t.Fatal("changed file published", h.toolCalls)
				}
			} else {
				if h.toolCalls != 2 || s.caseQuestion().Status != "done" {
					t.Fatal("confirmed selection not published")
				}
				e.handleCaseAnswer(p, answerFixture(q, "askq:0:1", true), "askq:0:1")
				if h.toolCalls != 2 {
					t.Fatal("duplicate published twice")
				}
			}
		})
	}
}

func TestCasePublicationRejectsForgedOrUnacceptedSelection(t *testing.T) {
	e, h, _, state, _ := caseQuestionFixture(t)
	e.fileWorkHost = &fileHostStub{toolResult: publicationResult(publicationWork())}
	for _, input := range []string{
		`{"case_name":"Fixture case","expected_revision":1,"delivery_ids":[]}`,
		`{"case_name":"Fixture case","expected_revision":1,"delivery_ids":["selected","selected"]}`,
		`{"case_name":"Fixture case","expected_revision":1,"delivery_ids":["other-work"]}`,
		`{"case_name":"Fixture case","expected_revision":1,"delivery_ids":["selected"],"publication_files":[]}`,
		`{"case_name":"Fixture case","expected_revision":1,"delivery_ids":["selected"],"group_realm":"forged"}`,
		`{"case_name":"Fixture case","expected_revision":1,"delivery_ids":[],"delivery_ids":["selected"]}`,
	} {
		w := actionToolRequest(e.ActionToolHandler(), "POST", "/tool", state.actionToken, `{"command":"case-publish","input":`+input+`}`)
		if w.Code != 403 || h.toolCalls != 0 {
			t.Fatal("forged selection reached CRM", w.Code, w.Body.String())
		}
	}
	for _, status := range []string{"submitted", "unknown", "failed"} {
		work := publicationWork()
		work.Artifacts[0].Status = status
		e.fileWorkHost = &fileHostStub{toolResult: publicationResult(work)}
		if _, err := e.selectedCaseFiles(context.Background(), state.currentPrincipal, "fixture-work", json.RawMessage(publicationInput)); err == nil {
			t.Fatal(status)
		}
	}
}

func TestCasePublicationRecoveryQueriesFrozenResultWithoutResolvingFiles(t *testing.T) {
	e, h, _, state, _ := caseQuestionFixture(t)
	q := &CaseQuestion{Command: "case-publish", Input: json.RawMessage(publicationInput), Principal: state.currentPrincipal, Context: CaseContext{WorkID: "fixture-work", PublicationFiles: []CasePublicationFile{{DeliveryID: "selected"}}}}
	h.toolResult = map[string]any{"status": "recorded"}
	e.fileWorkHost = nil
	if result, err := e.callCaseQuestion(context.Background(), q, true); err != nil || result["status"] != "recorded" || h.toolCommand != "case-result" {
		t.Fatal(result, err)
	}
}

func TestCaseFileImportRequiresActualSelectedInputAndCannotClaimEmptySuccess(t *testing.T) {
	e, _, _, state := actionToolFixture(t)
	defer e.cancel()
	state.fileWorkID = "fixture-work"
	files := &fileHostStub{toolResult: publicationResult(FileWorkContext{Enabled: true, WorkID: state.fileWorkID})}
	e.fileWorkHost = files
	artifact := map[string]any{"kind": "published_file", "publication_id": "pub_fixture", "name": "budget.xlsx", "sha256": strings.Repeat("a", 64)}
	for _, mode := range []string{"empty", "missing-input", "present"} {
		result := map[string]any{"case": map[string]any{"artifacts": []any{artifact}}}
		if mode == "empty" {
			result["case"] = map[string]any{"artifacts": []any{}}
		}
		if mode == "present" {
			work := FileWorkContext{Enabled: true, WorkID: state.fileWorkID, Inputs: []FileWorkInput{{Name: "budget.xlsx", SHA256: strings.Repeat("a", 64), Source: &FileWorkSource{PublicationID: "pub_fixture"}}}}
			files.toolResult = publicationResult(work)
		}
		e.importCaseArtifacts(context.Background(), state.actionToken, state.currentPrincipal, result)
		status := result["file_import"].(map[string]any)["status"]
		want := map[string]string{"empty": "none", "missing-input": "unavailable", "present": "available"}[mode]
		if status != want {
			t.Fatal(mode, result)
		}
		if mode == "present" && result["file_import"].(map[string]any)["imported"] != 1 {
			t.Fatal(result)
		}
	}
}
