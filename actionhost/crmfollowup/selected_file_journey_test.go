package crmfollowup

// Real Engine, action token, CRM CLI/SQLite, file ledger and snapshot copy.
// Only model output and the external file-send transport are scripted.
import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/actionhost/files"
	"github.com/chenhg5/cc-connect/core"
)

func TestCUJ_CASEIPC_SelectedPrivateFilePublicationAndRealImport(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX ownership/no-follow boundary; run this journey on Linux")
	}
	crm := os.Getenv("MYANC_SPIKE_CRM_ROOT")
	if crm == "" {
		t.Skip("set MYANC_SPIKE_CRM_ROOT for the paired offline journey")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	account, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	protected := func() string {
		dir := t.TempDir()
		if err := os.Chmod(dir, 0700); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	crmState, workBase, snapshots := protected(), protected(), protected()
	database := filepath.Join(protected(), "files.sqlite")
	if err := files.Initialize(database); err != nil {
		t.Fatal(err)
	}
	store, err := files.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	var sends atomic.Int32
	host, err := files.New(store, snapshots, func(_ context.Context, _ json.RawMessage, attachment core.FileAttachment, _ string) (string, error) {
		if len(attachment.Data) == 0 {
			t.Error("empty delivery")
		}
		return fmt.Sprintf("private-file-receipt-%d", sends.Add(1)), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.Close() })
	engines := map[string]*core.Engine{}
	host.SetPublicationAccess(func(ctx context.Context, p core.ActionPrincipal, workID, publicationID string) (core.CasePublishedSource, error) {
		return engines[p.UserID].AuthorizeCasePublication(ctx, p, workID, publicationID)
	})
	type actor struct {
		turn      func(string, string, map[string]any) map[string]any
		principal core.ActionPrincipal
		platform  *caseJourneyPlatform
	}
	newActor := func(id, chat string) *actor {
		a := &Adapter{hostSecret: "offline-gateway-host-secret-00000000000001", toolsEnabled: true}
		if err := a.SetWorkDir(t.TempDir(), account.Username); err != nil {
			t.Fatal(err)
		}
		a.run = func(ctx context.Context, _ string, command string, input []byte, env []string) ([]byte, error) {
			cmd := exec.CommandContext(ctx, python, "-B", filepath.Join(crm, "spikes", "host_tool_fixture.py"), command)
			cmd.Env = append(env, "MYANC_SPIKE_DIR="+crmState, "MYANC_SPIKE_SEED=project-context")
			cmd.Stdin = bytes.NewReader(input)
			output, err := cmd.Output()
			if exit, ok := err.(*exec.ExitError); ok {
				return nil, fmt.Errorf("offline helper: %s", exit.Stderr)
			}
			return output, err
		}
		platform := &caseJourneyPlatform{toolJourneyPlatform: toolJourneyPlatform{cards: map[string]*core.Card{}}}
		agent := &caseJourneyAgent{toolJourneyAgent: toolJourneyAgent{client: &http.Client{Timeout: 10 * time.Second}, steps: make(chan toolJourneyStep, 1), results: make(chan toolJourneyReply, 1), events: make(chan core.Event, 16)}}
		engine := core.NewEngine("test", agent, []core.Platform{platform}, filepath.Join(t.TempDir(), "sessions.json"), core.LangEnglish)
		engine.SetActionHost(a)
		engine.SetSharedCasesEnabled(true)
		engine.SetCaseConfirmationsEnabled(true)
		if err := engine.SetFileWorkHost(host, func(id string) (string, int, error) {
			root, err := files.PrepareWork(workBase, id, os.Geteuid())
			return root, os.Geteuid(), err
		}); err != nil {
			t.Fatal(err)
		}
		engines[id] = engine
		server := httptest.NewServer(engine.ActionToolHandler())
		agent.url = server.URL
		t.Cleanup(server.Close)
		if err := engine.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = engine.Stop() })
		result := &actor{platform: platform}
		n := 0
		result.turn = func(content, command string, input map[string]any) map[string]any {
			t.Helper()
			n++
			key := "mock:" + chat + ":" + id
			message := fmt.Sprintf("%s-%s-%d", id, chat, n)
			result.principal = core.ActionPrincipal{Platform: "mock", UserID: id, ChatID: chat, Project: "test", SessionKey: key, MessageID: message}
			agent.steps <- toolJourneyStep{number: n, command: command, input: input, plain: content}
			engine.ReceiveMessage(platform, &core.Message{Platform: "mock", SessionKey: key, UserID: id, ChannelID: chat, MessageID: message, Content: content, ReplyCtx: "fixture", ControlledFileWork: true, FileWorkPrivate: true, UserMessageTimeMs: time.Now().UnixMilli() + 1})
			var reply toolJourneyReply
			select {
			case reply = <-agent.results:
			case <-time.After(15 * time.Second):
				t.Fatal("scripted turn timed out", platform.transcript())
			}
			if reply.err != nil {
				t.Fatal(reply.err)
			}
			deadline := time.Now().Add(5 * time.Second)
			for {
				busy := false
				for _, session := range engine.GetSessions().ListSessions(key) {
					busy = busy || session.Busy()
				}
				if !busy {
					return reply.data
				}
				if time.Now().After(deadline) {
					t.Fatal("turn did not finish")
				}
				time.Sleep(5 * time.Millisecond)
			}
		}
		return result
	}
	decodeWork := func(value any) core.FileWorkContext {
		t.Helper()
		raw, err := json.Marshal(value)
		var work core.FileWorkContext
		if err != nil || json.Unmarshal(raw, &work) != nil || !work.Enabled || work.WorkID == "" {
			t.Fatalf("invalid real work context: %v", value)
		}
		return work
	}
	expect := func(result map[string]any, status string) map[string]any {
		t.Helper()
		if result["status"] != status {
			t.Fatalf("want %s, got %v", status, result)
		}
		return result
	}
	owner, partner := newActor("sender-1", "private-1"), newActor("sender-2", "private-2")
	read := map[string]any{"case_name": "周许婚宴"}
	create := map[string]any{"case_name": "周许婚宴", "expected_revision": 0, "changes": []any{map[string]any{"field": "桌数", "value": "22桌", "state": "proposed", "quote": "周许婚宴桌数改为22桌"}}}
	expect(owner.turn("请同步给执行：周许婚宴桌数改为22桌", "case-update", create), "recorded")
	expect(partner.turn("查看周许婚宴", "case-read", read), "not_found")
	invite := map[string]any{"case_name": "周许婚宴", "expected_revision": 1, "member_name": "Fixture sender-2", "responsibility": "运营执行"}
	expect(owner.turn("周许婚宴，麻烦叫Fixture sender-2来一起做运营吧", "case-member-add", invite), "awaiting_confirmation")
	expect(owner.turn("嗯", "case-read", read), "found")

	work := decodeWork(owner.turn("准备两个文件，其中一个只供我自己看", "work-context", map[string]any{}))
	selected := selectedJourneyDocument(t, "Fictional shared banquet brief")
	private := selectedJourneyDocument(t, "Fictional private negotiation notes")
	for i, item := range []struct {
		name string
		data []byte
	}{{"shared.docx", selected}, {"private.docx", private}} {
		if err := os.WriteFile(filepath.Join(work.OutputDir, item.name), item.data, 0600); err != nil {
			t.Fatal(err)
		}
		expect(owner.turn("把这份文件发给我", "file-deliver", map[string]any{"work_id": work.WorkID, "path": item.name, "sha256": fmt.Sprintf("%x", sha256.Sum256(item.data)), "expected_version": i}), "accepted")
	}
	work = decodeWork(owner.turn("只把共同方案放进项目", "work-context", map[string]any{}))
	var selectedFile, privateFile core.FileWorkArtifact
	for _, artifact := range work.Artifacts {
		switch artifact.Name {
		case "shared.docx":
			selectedFile = artifact
		case "private.docx":
			privateFile = artifact
		}
	}
	if selectedFile.Status != "accepted" || privateFile.Status != "accepted" || sends.Load() != 2 {
		t.Fatal("two private deliveries were not accepted", work, sends.Load())
	}
	publication := map[string]any{"case_name": "周许婚宴", "expected_revision": 2, "delivery_ids": []string{selectedFile.DeliveryID}}
	expect(owner.turn("周许婚宴，只共享共同方案，私密谈判说明不共享", "case-publish", publication), "awaiting_confirmation")
	_, question := owner.platform.lastCard()
	if question == nil || !strings.Contains(question.RenderText(), "shared.docx") || !strings.Contains(question.RenderText(), "项目成员") || strings.Contains(question.RenderText(), "private.docx") {
		t.Fatal("visible confirmation did not name only the selected file and audience", question)
	}
	beforeConfirmation := expect(partner.turn("查看周许婚宴的共同方案", "case-read", read), "found")
	if len(beforeConfirmation["case"].(map[string]any)["artifacts"].([]any)) != 0 || beforeConfirmation["file_import"].(map[string]any)["imported"] != float64(0) || sends.Load() != 2 {
		t.Fatal("file shared before owner confirmation", beforeConfirmation)
	}
	expect(owner.turn("嗯", "case-read", read), "found")
	// The accepted snapshot, rather than a later model edit, must be copied.
	if err := os.WriteFile(filepath.Join(work.OutputDir, "shared.docx"), []byte("later model edit"), 0600); err != nil {
		t.Fatal(err)
	}
	imported := expect(partner.turn("继续周许婚宴，读取共同方案", "case-read", read), "found")
	if summary, ok := imported["file_import"].(map[string]any); !ok || summary["status"] != "available" || summary["imported"] != float64(1) {
		t.Fatal("a zero-file import is not a successful handoff", imported)
	}
	record := imported["case"].(map[string]any)
	artifacts := record["artifacts"].([]any)
	if len(artifacts) != 1 || record["revision"] != float64(3) {
		t.Fatal("publication selection or revision changed", record)
	}
	artifact := artifacts[0].(map[string]any)
	publicationID, _ := artifact["publication_id"].(string)
	if artifact["kind"] != "published_file" || publicationID == "" || artifact["sha256"] != selectedFile.SHA256 || artifact["name"] != selectedFile.Name || artifact["file_version"] != float64(selectedFile.Version) || artifact["case_revision"] != float64(3) {
		t.Fatal("public artifact is not the selected version", artifact)
	}
	partnerWork := decodeWork(imported["work_context"])
	if partnerWork.WorkID == work.WorkID || len(partnerWork.Inputs) != 1 {
		t.Fatal("import did not use the second actor's own work", partnerWork)
	}
	input := partnerWork.Inputs[0]
	if filepath.Dir(input.Path) != filepath.Join(filepath.Dir(partnerWork.OutputDir), "inputs") || input.SHA256 != selectedFile.SHA256 || input.Source == nil || input.Source.PublicationID != publicationID {
		t.Fatal("import path or provenance mismatch", input)
	}
	actual, err := os.ReadFile(input.Path)
	if err != nil || !bytes.Equal(actual, selected) || fmt.Sprintf("%x", sha256.Sum256(actual)) != selectedFile.SHA256 {
		t.Fatal("second actor did not receive the exact accepted snapshot", err)
	}
	raw, _ := json.Marshal(imported)
	for _, secret := range []string{work.WorkID, selectedFile.DeliveryID, selectedFile.MessageReceipt, privateFile.MessageReceipt, privateFile.SHA256, "private.docx"} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("private source metadata leaked to the other actor", secret)
		}
	}
	// Knowing an unselected private receipt cannot grant access through the
	// legacy import branch or by presenting it as a publication identifier.
	for _, request := range []map[string]string{{"receipt": privateFile.MessageReceipt}, {"publication_id": privateFile.MessageReceipt}} {
		raw, _ := json.Marshal(request)
		refused, err := host.Tool(context.Background(), "host-case-import", raw, partner.principal, partnerWork.WorkID)
		if err == nil && refused["status"] != "blocked" {
			t.Fatal("unselected private artifact imported", refused)
		}
	}
	expect(owner.turn("嗯", "case-read", read), "found")
	repeated := expect(partner.turn("再看一下共同方案", "case-read", read), "found")
	again := decodeWork(repeated["work_context"])
	if repeated["case"].(map[string]any)["revision"] != float64(3) || len(repeated["case"].(map[string]any)["artifacts"].([]any)) != 1 || len(again.Inputs) != 1 || again.Inputs[0].Path != input.Path || sends.Load() != 2 {
		t.Fatal("repeated confirmation/read duplicated publication, input or send", repeated, sends.Load())
	}
	// Inspect persisted host provenance through a read-only connection. Model
	// redaction must not erase the source needed for queued/restarted imports.
	ledger, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: filepath.ToSlash(database), RawQuery: "mode=ro"}).String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ledger.Close() }()
	var payload []byte
	if err := ledger.QueryRow("SELECT payload FROM file_state WHERE id=1").Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var persisted struct {
		Schema int
		Works  map[string]struct {
			GroupRealm, ProjectImportRealm string
			Inputs                         []core.FileWorkInput
			Deliveries                     map[string]core.FileWorkArtifact
		}
	}
	if err := json.Unmarshal(payload, &persisted); err != nil {
		t.Fatal(err)
	}
	source, sourcePresent := persisted.Works[work.WorkID]
	destination, destinationPresent := persisted.Works[partnerWork.WorkID]
	if !sourcePresent || !destinationPresent || persisted.Schema != 4 || source.GroupRealm != "" || source.ProjectImportRealm != "" || destination.GroupRealm != "" || destination.ProjectImportRealm != "" || len(destination.Inputs) != 1 {
		t.Fatal("publication changed the private work realms or lost its persisted input")
	}
	for _, original := range []core.FileWorkArtifact{selectedFile, privateFile} {
		if source.Deliveries[original.DeliveryID] != original {
			t.Fatal("publication changed an original private delivery")
		}
	}
	provenance := destination.Inputs[0].Source
	if provenance == nil || provenance.PublicationID != publicationID || provenance.WorkID != work.WorkID || provenance.DeliveryID != selectedFile.DeliveryID || provenance.MessageReceipt != selectedFile.MessageReceipt || provenance.Version != selectedFile.Version {
		t.Fatal("private source provenance was not durably retained")
	}
	if _, err := os.Stat(filepath.Join(crmState, "formal-never-opened.sqlite3")); !os.IsNotExist(err) {
		t.Fatal("formal ledger opened")
	}
}

func selectedJourneyDocument(t *testing.T, text string) []byte {
	t.Helper()
	var result bytes.Buffer
	archive := zip.NewWriter(&result)
	for _, part := range []struct{ name, text string }{
		{"[Content_Types].xml", `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/></Types>`},
		{"_rels/.rels", `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="r1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/></Relationships>`},
		{"word/document.xml", `<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body><w:p><w:r><w:t>` + text + `</w:t></w:r></w:p></w:body></w:document>`},
	} {
		file, err := archive.Create(part.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write([]byte(part.text)); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	return result.Bytes()
}
