package files

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

func TestFinalWordCannotBeDeliveredWithOnlyCallerInspectionClaim(t *testing.T) {
	sends := 0
	f := newFixture(t, func(context.Context, json.RawMessage, core.FileAttachment, string) (string, error) {
		sends++
		return "fictional-accepted", nil
	})
	data := output(t, f, "docx")
	// B: caller knows the final path/hash, but no trusted final-content read occurred.
	raw, _ := json.Marshal(map[string]any{"work_id": f.context.WorkID, "path": "result.docx", "sha256": hash(data), "expected_version": 0})
	result, err := f.host.Tool(context.Background(), "file-deliver", raw, f.binding.Principal, f.context.WorkID)
	if err == nil || result["code"] != "file_content_read_required" || sends != 0 {
		t.Fatalf("unchecked final Word was not blocked: result=%v err=%v sends=%d", result, err, sends)
	}
	w, err := f.host.Tool(context.Background(), "work-context", []byte(`{}`), f.binding.Principal, f.context.WorkID)
	if err != nil || w["latest_version"] != float64(0) {
		t.Fatal("blocked read must not consume a delivery version", w, err)
	}
}

func TestFinalWordNonXMLHeaderCannotProduceIncompleteProof(t *testing.T) {
	for _, declaration := range []string{
		`<Override PartName="/word/header1.part" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.header+xml"/>`,
		`<Default Extension="part" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.header+xml"/>`,
		`<Override PartName="/word/header1.part" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.header+xml" xmlns:x="urn:fixture" x:ContentType="application/octet-stream" x:PartName="/word/header1.xml"/>`,
	} {
		f := newFixture(t, nil)
		parts := documentParts("docx")
		parts["[Content_Types].xml"] = strings.Replace(parts["[Content_Types].xml"], "</Types>", declaration+"</Types>", 1)
		parts["word/document.xml"] = strings.Replace(parts["word/document.xml"], "</w:body>", `<w:sectPr xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><w:headerReference w:type="default" r:id="rh"/></w:sectPr></w:body>`, 1)
		parts["word/_rels/document.xml.rels"] = `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rh" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/header" Target="header1.part"/></Relationships>`
		parts["word/header1.part"] = `<w:hdr xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:p><w:r><w:t>HEADER_PAYMENT_UNKNOWN</w:t></w:r></w:p></w:hdr>`
		data := packageBytes(t, parts)
		if err := os.WriteFile(filepath.Join(f.context.OutputDir, "final.docx"), data, 0600); err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(map[string]any{"work_id": f.context.WorkID, "path": "final.docx", "sha256": hash(data)})
		result, err := f.host.Tool(context.Background(), "file-read", raw, f.binding.Principal, f.context.WorkID)
		if err == nil || result["code"] != "file_content_read_required" {
			t.Fatal("unsupported content layout issued proof", result, err)
		}
		contextResult, err := f.host.Tool(context.Background(), "work-context", []byte(`{}`), f.binding.Principal, f.context.WorkID)
		if err != nil || len(contextResult["content_reads"].([]any)) != 0 {
			t.Fatal("incomplete read left proof", contextResult, err)
		}
	}
}

func TestOpenRejectsNullDeliveryWithoutChangingCorruptEvidence(t *testing.T) {
	f := newFixture(t, nil)
	var raw []byte
	if err := f.store.db.QueryRow("SELECT payload FROM file_state WHERE id=1").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var corrupt state
	if err := json.Unmarshal(raw, &corrupt); err != nil {
		t.Fatal(err)
	}
	corrupt.Works[f.context.WorkID].Deliveries["bad"] = nil
	raw, _ = json.Marshal(corrupt)
	if _, err := f.store.db.Exec("UPDATE file_state SET payload=? WHERE id=1", string(raw)); err != nil {
		t.Fatal(err)
	}
	opened, err := Open(f.database)
	if opened != nil {
		_ = opened.Close()
	}
	if !errors.Is(err, ErrUnavailable) {
		t.Fatal("corrupt ledger accepted", err)
	}
	var after []byte
	if err := f.store.db.QueryRow("SELECT payload FROM file_state WHERE id=1").Scan(&after); err != nil || string(after) != string(raw) {
		t.Fatal("corrupt evidence changed", err)
	}
}

func TestFinalWordReadReturnsSavedBodyTablesHeadersAndFooterBeforeSend(t *testing.T) {
	f := newFixture(t, nil)
	parts := documentParts("docx")
	parts["word/header1.xml"] = `<w:hdr xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:p><w:r><w:t>FINAL HEADER</w:t></w:r></w:p></w:hdr>`
	parts["word/footer1.xml"] = `<w:ftr xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:p><w:r><w:t>source-014</w:t></w:r></w:p></w:ftr>`
	parts["word/alternate/location.xml"] = `<w:hdr xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:p><w:r><w:t>RELOCATED HEADER</w:t></w:r></w:p></w:hdr>`
	parts["word/document.xml"] = strings.Replace(parts["word/document.xml"], "</w:body>", `<w:tbl><w:tr><w:tc><w:p><w:r><w:t>Outstanding 4500</w:t></w:r></w:p></w:tc></w:tr></w:tbl></w:body>`, 1)
	data := packageBytes(t, parts)
	if err := os.WriteFile(filepath.Join(f.context.OutputDir, "final.docx"), data, 0600); err != nil {
		t.Fatal(err)
	}
	var content []byte
	for chunk := 0; chunk < 512; chunk++ {
		raw, _ := json.Marshal(map[string]any{"work_id": f.context.WorkID, "path": "final.docx", "sha256": hash(data), "chunk": chunk})
		read, err := f.host.Tool(context.Background(), "file-read", raw, f.binding.Principal, f.context.WorkID)
		if err != nil || read["sha256"] != hash(data) || read["path"] != "final.docx" {
			t.Fatal("final content read failed", read, err)
		}
		part, _ := json.Marshal(read["content"])
		content = append(content, part...)
		if read["status"] == "content_read" {
			break
		}
	}
	for _, expected := range []string{"Fictional reception brief", "Outstanding 4500", "FINAL HEADER", "source-014", "RELOCATED HEADER"} {
		if !strings.Contains(string(content), expected) {
			t.Fatal("missing saved content", expected)
		}
	}
	result, err := deliver(t, f, "final.docx", data, 0)
	if err != nil || result["status"] != "accepted" {
		t.Fatal("read exact final file must permit delivery", result, err)
	}
}

func TestFinalContentProofCannotBeCopiedFromSourceOldBytesOrAnotherWork(t *testing.T) {
	for _, defect := range []string{"source", "renamed", "changed", "another-work", "local-json"} {
		t.Run(defect, func(t *testing.T) {
			sends := 0
			f := newFixture(t, func(context.Context, json.RawMessage, core.FileAttachment, string) (string, error) {
				sends++
				return "unexpected", nil
			})
			data := output(t, f, "docx")
			name := "result.docx"
			readName := name
			if defect == "source" || defect == "renamed" {
				readName = "source.docx"
				if err := os.WriteFile(filepath.Join(f.context.OutputDir, readName), data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			raw, _ := json.Marshal(map[string]any{"work_id": f.context.WorkID, "path": readName, "sha256": hash(data)})
			switch defect {
			case "local-json":
				if err := os.WriteFile(filepath.Join(f.context.OutputDir, "content-read.json"), raw, 0600); err != nil {
					t.Fatal(err)
				}
			case "another-work":
				other := newFixture(t, nil)
				if result, err := other.host.Tool(context.Background(), "file-read", raw, other.binding.Principal, other.context.WorkID); err == nil {
					t.Fatal("cross-work read accepted", result)
				}
			default:
				if _, err := f.host.Tool(context.Background(), "file-read", raw, f.binding.Principal, f.context.WorkID); err != nil {
					t.Fatal(err)
				}
			}
			if defect == "changed" {
				parts := documentParts("docx")
				parts["word/document.xml"] = strings.Replace(parts["word/document.xml"], "Fictional reception brief", "Modified after read", 1)
				data = packageBytes(t, parts)
				if err := os.WriteFile(filepath.Join(f.context.OutputDir, name), data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			raw, _ = json.Marshal(map[string]any{"work_id": f.context.WorkID, "path": name, "sha256": hash(data), "expected_version": 0})
			result, err := f.host.Tool(context.Background(), "file-deliver", raw, f.binding.Principal, f.context.WorkID)
			if err == nil || result["code"] != "file_content_read_required" || sends != 0 {
				t.Fatal(result, err, sends)
			}
		})
	}
}

func TestFailedOrCancelledContentReadCannotAuthorizeDelivery(t *testing.T) {
	for _, defect := range []string{"cancelled", "invalid", "over-budget"} {
		t.Run(defect, func(t *testing.T) {
			f := newFixture(t, nil)
			data := output(t, f, "docx")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if defect == "cancelled" {
				cancel()
			}
			if defect == "invalid" {
				data = []byte("not a Word document")
			}
			if defect == "over-budget" {
				parts := documentParts("docx")
				parts["word/document.xml"] = strings.Replace(parts["word/document.xml"], "Fictional reception brief", strings.Repeat("fictional text ", 50000), 1)
				data = packageBytes(t, parts)
			}
			if err := os.WriteFile(filepath.Join(f.context.OutputDir, "result.docx"), data, 0600); err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(map[string]any{"work_id": f.context.WorkID, "path": "result.docx", "sha256": hash(data)})
			if result, err := f.host.Tool(ctx, "file-read", raw, f.binding.Principal, f.context.WorkID); err == nil {
				t.Fatal("read should fail", result)
			}
			result, err := f.host.Tool(context.Background(), "work-context", []byte(`{}`), f.binding.Principal, f.context.WorkID)
			if err != nil || len(result["content_reads"].([]any)) != 0 || result["latest_version"] != float64(0) {
				t.Fatal("failed read left proof", result, err)
			}
		})
	}
}

func TestPDFContentReadRejectsFailedPartialAndOversizedParserOutput(t *testing.T) {
	for _, parser := range []string{"exit 1", `printf '{"format":"pdf_text_layer","parts":[]}'`,
		`printf '{"format":"pdf_text_layer","parts":[{"name":"page-2","text":"partial"}]}'`,
		`head -c 600000 /dev/zero`,
		`printf '{"format":"pdf_text_layer","parts":[{"name":"page-1","text":"ok"}]} trailing'`} {
		t.Run(hash([]byte(parser))[:8], func(t *testing.T) {
			f := newFixture(t, nil)
			validator := filepath.Join(filepath.Dir(f.database), "read-validator")
			if err := os.WriteFile(validator, []byte("#!/bin/sh\n"+parser+"\n"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := f.host.SetDocumentFormats(validator); err != nil {
				t.Fatal(err)
			}
			data := []byte("%PDF-fictional")
			if err := os.WriteFile(filepath.Join(f.context.OutputDir, "final.pdf"), data, 0600); err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(map[string]any{"work_id": f.context.WorkID, "path": "final.pdf", "sha256": hash(data)})
			if result, err := f.host.Tool(context.Background(), "file-read", raw, f.binding.Principal, f.context.WorkID); err == nil {
				t.Fatal("partial parser result accepted", result)
			}
			result, err := f.host.Tool(context.Background(), "work-context", []byte(`{}`), f.binding.Principal, f.context.WorkID)
			if err != nil || len(result["content_reads"].([]any)) != 0 {
				t.Fatal("failed parser left proof", result, err)
			}
		})
	}
}

func TestAcceptedAndUnknownDeliveryRemainSingleAttemptAfterReadEvidenceChanges(t *testing.T) {
	for _, uncertain := range []bool{false, true} {
		t.Run(fmt.Sprint(uncertain), func(t *testing.T) {
			sends := 0
			f := newFixture(t, func(context.Context, json.RawMessage, core.FileAttachment, string) (string, error) {
				sends++
				if uncertain {
					return "", errors.New("fictional transport uncertainty")
				}
				return "fictional-accepted", nil
			})
			data := output(t, f, "docx")
			first, err := deliver(t, f, "result.docx", data, 0)
			if err != nil || first["content_sha256"] == "" {
				t.Fatal(first, err)
			}
			if err := os.WriteFile(filepath.Join(f.context.OutputDir, "result.docx"), []byte("changed after first attempt"), 0600); err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(map[string]any{"work_id": f.context.WorkID, "path": "result.docx", "sha256": hash(data), "expected_version": 0})
			reopened, err := Open(f.database)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = reopened.Close() }()
			f.host.store = reopened
			again, err := f.host.Tool(context.Background(), "file-deliver", raw, f.binding.Principal, f.context.WorkID)
			if err != nil || again["delivery_id"] != first["delivery_id"] || again["status"] != first["status"] || sends != 1 {
				t.Fatal("original attempt replayed or lost", again, err, sends)
			}
		})
	}
}
