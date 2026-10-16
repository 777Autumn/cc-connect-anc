package files

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

func TestFinalContentMustProvideAllBoundedChunksBeforeDelivery(t *testing.T) {
	f := newFixture(t, nil)
	// Production enables the existing static Office-field validation profile.
	validator := filepath.Join(filepath.Dir(f.database), "unused-pdf-validator")
	if err := os.WriteFile(validator, []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := f.host.SetDocumentFormats(validator); err != nil {
		t.Fatal(err)
	}
	parts := documentParts("docx")
	parts["word/document.xml"] = strings.Replace(parts["word/document.xml"], "Fictional reception brief", strings.Repeat("Fictional table condition 4500 remains unknown. ", 1200), 1)
	data := packageBytes(t, parts)
	// The private real failure is replayed locally; no chat or original enters Git.
	if original := os.Getenv("MYANC_CONTENT_READ_FIXTURE"); original != "" {
		var err error
		data, err = os.ReadFile(original)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(f.context.OutputDir, "final.docx"), data, 0600); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"work_id": f.context.WorkID, "path": "final.docx", "sha256": hash(data)})
	read, err := f.host.Tool(context.Background(), "file-read", raw, f.binding.Principal, f.context.WorkID)
	encoded, _ := json.Marshal(read)
	if err != nil || read["status"] != "content_read_partial" || len(encoded) > 8192 {
		t.Fatalf("first segment must be bounded and incomplete: status=%v bytes=%d err=%v", read["status"], len(encoded), err)
	}
	raw, _ = json.Marshal(map[string]any{"work_id": f.context.WorkID, "path": "final.docx", "sha256": hash(data), "expected_version": 0})
	result, err := f.host.Tool(context.Background(), "file-deliver", raw, f.binding.Principal, f.context.WorkID)
	if err == nil || result["code"] != "file_content_read_required" {
		t.Fatal("partial output authorized delivery", result, err)
	}
	total := int(read["chunks_total"].(float64))
	readChunk := func(index int) (map[string]any, error) {
		payload, _ := json.Marshal(map[string]any{"work_id": f.context.WorkID, "path": "final.docx", "sha256": hash(data), "chunk": index})
		return f.host.Tool(context.Background(), "file-read", payload, f.binding.Principal, f.context.WorkID)
	}
	if _, err := readChunk(2); err == nil {
		t.Fatal("skipping a segment was accepted")
	}
	duplicate, err := readChunk(0)
	if err != nil || duplicate["chunks_provided"] != float64(1) {
		t.Fatal("repeated first segment advanced progress", duplicate, err)
	}
	reopened, err := Open(f.database)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	f.host.store = reopened
	provided := map[string]string{}
	for index := 0; index < total; index++ {
		segment, err := readChunk(index)
		encoded, _ := json.Marshal(segment)
		if err != nil || len(encoded) > 8192 || segment["sha256"] != hash(data) || segment["content_sha256"] != read["content_sha256"] {
			t.Fatalf("segment %d was not bounded and bound: bytes=%d err=%v", index, len(encoded), err)
		}
		var body struct {
			Content fileContent `json:"content"`
			Offset  int         `json:"part_offset"`
		}
		if err := json.Unmarshal(encoded, &body); err != nil || len(body.Content.Parts) != 1 {
			t.Fatal("segment content missing", err)
		}
		part := body.Content.Parts[0]
		if body.Offset != len(provided[part.Name]) {
			t.Fatal("content gap or overlap", part.Name, body.Offset)
		}
		provided[part.Name] += part.Text
		if index < total-1 {
			if segment["status"] != "content_read_partial" {
				t.Fatal("premature completion", index)
			}
			if _, err := f.host.Tool(context.Background(), "file-deliver", raw, f.binding.Principal, f.context.WorkID); err == nil {
				t.Fatal("incomplete content allowed a send", index)
			}
		} else if segment["status"] != "content_read" {
			t.Fatal("last segment did not complete")
		}
	}
	z, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	for name, text := range provided {
		part, err := z.Open(name)
		if err != nil {
			t.Fatal(err)
		}
		original, err := io.ReadAll(part)
		_ = part.Close()
		if err != nil || string(original) != text {
			t.Fatal("actual final part changed or was omitted", name, err)
		}
	}
	if provided["word/document.xml"] == "" {
		t.Fatal("final document body missing")
	}
	result, err = f.host.Tool(context.Background(), "file-deliver", raw, f.binding.Principal, f.context.WorkID)
	if err != nil || result["status"] != "accepted" {
		t.Fatal("complete final content could not be delivered", result, err)
	}
	// A revision at the same path must restart at zero, even after completed reads.
	parts["word/document.xml"] = strings.Replace(parts["word/document.xml"], "4500", "4800", 1)
	data = packageBytes(t, parts)
	if err := os.WriteFile(filepath.Join(f.context.OutputDir, "final.docx"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readChunk(1); err == nil {
		t.Fatal("changed final file reused prior progress")
	}
	first, err := readChunk(0)
	if err != nil || first["chunks_provided"] != float64(1) || first["status"] != "content_read_partial" {
		t.Fatal("changed file did not restart", first, err)
	}
	raw, _ = json.Marshal(map[string]any{"work_id": f.context.WorkID, "path": "final.docx", "sha256": hash(data), "expected_version": 1})
	if result, err := f.host.Tool(context.Background(), "file-deliver", raw, f.binding.Principal, f.context.WorkID); err == nil || result["code"] != "file_content_read_required" {
		t.Fatal("changed file was sent without complete new content", result, err)
	}
}

func TestLegacySingleResponseProofCannotAuthorizeNewDelivery(t *testing.T) {
	f := newFixture(t, nil)
	data := output(t, f, "docx")
	if err := f.store.change(context.Background(), func(st *state) error {
		st.Schema = 6
		st.Works[f.context.WorkID].ContentReads = map[string]core.FileContentRead{
			"result.docx": {Path: "result.docx", SHA256: hash(data), ContentSHA256: hash([]byte("legacy supplied content"))},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(f.database)
	if err != nil {
		t.Fatal("legacy ledger must remain readable", err)
	}
	defer func() { _ = reopened.Close() }()
	f.host.store = reopened
	raw, _ := json.Marshal(map[string]any{"work_id": f.context.WorkID, "path": "result.docx", "sha256": hash(data), "expected_version": 0})
	result, err := f.host.Tool(context.Background(), "file-deliver", raw, f.binding.Principal, f.context.WorkID)
	if err == nil || result["code"] != "file_content_read_required" {
		t.Fatal("legacy single response authorized a new delivery", result, err)
	}
	result, err = deliver(t, f, "result.docx", data, 0)
	if err != nil || result["status"] != "accepted" {
		t.Fatal("complete reread did not recover legacy work", result, err)
	}
}

func TestContentChunksPreserveUTF8EscapesAndEmptyPDFPages(t *testing.T) {
	content := fileContent{Format: "pdf_text_layer", Parts: []contentPart{
		{Name: "page-1", Text: strings.Repeat("中<>&\"\\\n", 10000)}, {Name: "page-2", Text: ""},
	}}
	chunks, err := contentChunks(content)
	if err != nil {
		t.Fatal(err)
	}
	texts := map[string]string{}
	for _, chunk := range chunks {
		body, err := json.Marshal(chunk.Content)
		part := chunk.Content.Parts[0]
		if err != nil || len(body) > 6144 || chunk.Offset != len(texts[part.Name]) {
			t.Fatal("unbounded or discontinuous content", err)
		}
		texts[part.Name] += part.Text
	}
	for _, part := range content.Parts {
		if got, ok := texts[part.Name]; !ok || got != part.Text {
			t.Fatal("part lost or changed", part.Name)
		}
	}
}
