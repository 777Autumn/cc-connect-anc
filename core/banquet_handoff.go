package core

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
)

type BanquetFileCheck struct {
	Principal ActionPrincipal       `json:"principal"`
	WorkID    string                `json:"work_id"`
	Files     []CasePublicationFile `json:"files"`
	Published bool                  `json:"published"`
}

type banquetContextKey struct{}
type banquetCheckKey struct{}
type banquetVerifiedKey struct{}

func BanquetFilesVerified(ctx context.Context) (bool, bool) {
	verified, present := ctx.Value(banquetVerifiedKey{}).(bool)
	return verified, present
}

type BanquetHandoffContext struct {
	WorkID string                `json:"work_id"`
	Files  []CasePublicationFile `json:"files"`
}

func TrustedBanquetContext(ctx context.Context) (BanquetHandoffContext, bool) {
	value, ok := ctx.Value(banquetContextKey{}).(BanquetHandoffContext)
	return value, ok
}

// Only the engine supplies this checker; models cannot approve file provenance.
func CheckBanquetFiles(ctx context.Context, check BanquetFileCheck) error {
	verify, ok := ctx.Value(banquetCheckKey{}).(func(context.Context, BanquetFileCheck) error)
	if !ok {
		return errors.New("handoff file checker unavailable")
	}
	return verify(ctx, check)
}

func (e *Engine) banquetCheckContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, banquetCheckKey{}, func(ctx context.Context, check BanquetFileCheck) error {
		if !check.Published {
			if err := e.guardConnectionWork(ctx, "case-publish", check.Principal, "", check.WorkID); err != nil {
				return err
			}
		}
		e.actionMu.RLock()
		host := e.fileWorkHost
		e.actionMu.RUnlock()
		if host == nil {
			return errors.New("handoff file host unavailable")
		}
		raw, err := json.Marshal(map[string]any{"files": check.Files})
		if err != nil {
			return err
		}
		result, err := host.Tool(ctx, "host-handoff-check", raw, check.Principal, check.WorkID)
		if err != nil {
			return err
		}
		if result["status"] != "verified" {
			return errors.New("handoff snapshots unavailable")
		}
		return nil
	})
}

func (e *Engine) banquetSubmissionContext(ctx context.Context, principal ActionPrincipal, workID string, input json.RawMessage) (context.Context, error) {
	var request struct {
		DeliveryIDs []string `json:"delivery_ids"`
	}
	if json.Unmarshal(input, &request) != nil {
		return ctx, errors.New("invalid material selection")
	}
	selection, _ := json.Marshal(map[string]any{"case_name": "banquet-material-selection", "expected_revision": 0, "delivery_ids": request.DeliveryIDs})
	files, err := e.selectedCaseFiles(ctx, principal, workID, selection)
	if err != nil {
		return ctx, err
	}
	ctx = e.banquetCheckContext(ctx)
	if err := CheckBanquetFiles(ctx, BanquetFileCheck{Principal: principal, WorkID: workID, Files: files}); err != nil {
		return ctx, err
	}
	return context.WithValue(ctx, banquetContextKey{}, BanquetHandoffContext{WorkID: workID, Files: files}), nil
}

// Reuse the existing file importer and immutable snapshots. Its authorization
// comes from the current recipient and Feishu version, never a mock project.
func (e *Engine) authorizeHandoffPublication(ctx context.Context, p ActionPrincipal, workID, publicationID, purpose string) (CasePublishedSource, error) {
	e.actionMu.RLock()
	host := actionHostForCommand(e.actionHost, "handoff")
	e.actionMu.RUnlock()
	tools, ok := host.(interface {
		HandoffAccess(context.Context, ActionPrincipal, string, string, string) (map[string]any, error)
	})
	if !ok || workID == "" || !strings.HasPrefix(publicationID, "bh_") {
		return CasePublishedSource{}, ErrFileWorkNotFound
	}
	result, err := tools.HandoffAccess(ctx, p, workID, publicationID, purpose)
	if err != nil {
		return CasePublishedSource{}, err
	}
	if result["status"] != "authorized" || result["publication_id"] != publicationID {
		return CasePublishedSource{}, ErrFileWorkNotFound
	}
	raw, err := json.Marshal(result["source"])
	var source CasePublishedSource
	if err != nil || json.Unmarshal(raw, &source) != nil {
		return source, ErrFileWorkNotFound
	}
	if source.Purpose != purpose || source.ReferenceOnly != (purpose == "revision") || source.PublicationBasisID == "" {
		return CasePublishedSource{}, ErrFileWorkNotFound
	}
	return source, nil
}

func (e *Engine) importHandoffFiles(ctx context.Context, token string, p ActionPrincipal, result map[string]any) error {
	handoff, _ := result["handoff"].(map[string]any)
	files, _ := handoff["files"].([]any)
	if len(files) < 1 || len(files) > 4 {
		return ErrFileWorkNotFound
	}
	artifacts := make([]map[string]any, 0, len(files))
	for _, raw := range files {
		file, _ := raw.(map[string]any)
		id, _ := file["publication_id"].(string)
		if !strings.HasPrefix(id, "bh_") {
			return ErrFileWorkNotFound
		}
		artifacts = append(artifacts, map[string]any{"kind": "published_file", "publication_id": id, "name": file["name"], "sha256": file["sha256"],
			"purpose": file["purpose"], "reference_only": file["reference_only"], "publication_basis_id": file["publication_basis_id"]})
	}
	imported := map[string]any{"case": map[string]any{"artifacts": artifacts}}
	e.importCaseArtifacts(ctx, token, p, imported)
	status, _ := imported["file_import"].(map[string]any)
	if status["status"] != "available" || status["imported"] != len(files) {
		return ErrFileWorkNotFound
	}
	result["materials_imported"] = true
	result["work_context"] = imported["work_context"]
	return nil
}
