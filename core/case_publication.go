package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"
)

// CasePublicationFile is resolved from the host's accepted delivery journal.
// Models select IDs only; these fields are frozen with the existing question.
type CasePublicationFile struct {
	DeliveryID     string `json:"delivery_id"`
	Name           string `json:"name"`
	SHA256         string `json:"sha256"`
	Version        int    `json:"version"`
	MessageReceipt string `json:"message_receipt"`
}

// CasePublishedSource travels only between authenticated hosts. It is never a
// model-supplied GroupRealm, filesystem path, or newly sent message receipt.
type CasePublishedSource struct {
	Platform       string `json:"platform"`
	UserID         string `json:"user_id"`
	ChatID         string `json:"chat_id"`
	Project        string `json:"project"`
	SessionKey     string `json:"session_key"`
	WorkID         string `json:"work_id"`
	DeliveryID     string `json:"delivery_id"`
	Name           string `json:"name"`
	SHA256         string `json:"sha256"`
	Version        int    `json:"version"`
	MessageReceipt string `json:"message_receipt"`
}

func (e *Engine) selectedCaseFiles(ctx context.Context, p ActionPrincipal, workID string, input json.RawMessage) ([]CasePublicationFile, error) {
	if err := e.guardConnectionWork(ctx, "case-publish", p, "", workID); err != nil {
		return nil, errConnectionWorkUnavailable
	}
	var request struct {
		CaseName         string   `json:"case_name"`
		ExpectedRevision *int     `json:"expected_revision"`
		DeliveryIDs      []string `json:"delivery_ids"`
	}
	// Reject duplicates as well as unknown fields, including forged context.
	d := json.NewDecoder(bytes.NewReader(input))
	start, err := d.Token()
	if err != nil || start != json.Delim('{') {
		return nil, errors.New("invalid publication selection")
	}
	seen := map[string]bool{}
	for d.More() {
		key, err := d.Token()
		name, ok := key.(string)
		if err != nil || !ok || seen[name] {
			return nil, errors.New("invalid publication selection")
		}
		seen[name] = true
		var value json.RawMessage
		if d.Decode(&value) != nil {
			return nil, errors.New("invalid publication selection")
		}
	}
	d = json.NewDecoder(bytes.NewReader(input))
	d.DisallowUnknownFields()
	if len(input) > 4096 || d.Decode(&request) != nil || d.Decode(new(any)) != io.EOF || request.CaseName == "" || request.ExpectedRevision == nil || *request.ExpectedRevision < 0 || len(request.DeliveryIDs) < 1 || len(request.DeliveryIDs) > 4 {
		return nil, errors.New("invalid publication selection")
	}
	e.actionMu.RLock()
	host := e.fileWorkHost
	e.actionMu.RUnlock()
	if host == nil || workID == "" {
		return nil, errors.New("publication file host unavailable")
	}
	result, err := host.Tool(ctx, "work-context", json.RawMessage(`{}`), p, workID)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(result)
	var work FileWorkContext
	if err != nil || json.Unmarshal(raw, &work) != nil || !work.Enabled || work.WorkID != workID {
		return nil, errors.New("publication work unavailable")
	}
	files := make([]CasePublicationFile, 0, len(request.DeliveryIDs))
	seenIDs := map[string]bool{}
	for _, id := range request.DeliveryIDs {
		if id == "" || seenIDs[id] {
			return nil, errors.New("invalid publication selection")
		}
		seenIDs[id] = true
		found := false
		for _, artifact := range work.Artifacts {
			if artifact.DeliveryID != id {
				continue
			}
			if found || artifact.Status != "accepted" || artifact.MessageReceipt == "" || artifact.Name == "" || len(artifact.SHA256) != 64 || artifact.Version < 1 {
				return nil, errors.New("publication file unavailable")
			}
			found = true
			files = append(files, CasePublicationFile{DeliveryID: id, Name: artifact.Name, SHA256: artifact.SHA256, Version: artifact.Version, MessageReceipt: artifact.MessageReceipt})
		}
		if !found {
			return nil, errors.New("publication file unavailable")
		}
	}
	return files, nil
}

func (e *Engine) casePublicationContext(ctx context.Context, p ActionPrincipal, input json.RawMessage) (context.Context, error) {
	binding, ok := TrustedCaseContext(ctx)
	if !ok || !binding.OfferConfirmation {
		return ctx, errors.New("publication confirmation unavailable")
	}
	files, err := e.selectedCaseFiles(ctx, p, binding.WorkID, input)
	if err != nil {
		return ctx, err
	}
	binding.PublicationFiles = files
	return context.WithValue(ctx, caseContextKey{}, binding), nil
}

// AuthorizeCasePublication rechecks current project membership and the target
// work binding. Legacy group receipts keep their separate authorization path.
func (e *Engine) AuthorizeCasePublication(ctx context.Context, p ActionPrincipal, workID, publicationID string) (CasePublishedSource, error) {
	e.actionMu.RLock()
	host := actionHostForCommand(e.actionHost, "case-read")
	enabled := e.sharedCasesEnabled
	e.actionMu.RUnlock()
	tools, ok := host.(ActionToolHost)
	if !enabled || !ok || workID == "" || publicationID == "" {
		return CasePublishedSource{}, ErrFileWorkNotFound
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ctx = context.WithValue(ctx, caseContextKey{}, CaseContext{WorkID: workID})
	input, _ := json.Marshal(map[string]any{"publication_id": publicationID, "require_binding": true})
	result, card, err := tools.Tool(ctx, "case-access", input, p, "host-project-file-access-not-a-model-token", e.i18n.CurrentLang())
	if err != nil {
		return CasePublishedSource{}, err
	}
	if card != nil || result["status"] != "authorized" || result["publication_id"] != publicationID {
		return CasePublishedSource{}, ErrFileWorkNotFound
	}
	raw, err := json.Marshal(result["source"])
	var source CasePublishedSource
	if err != nil || json.Unmarshal(raw, &source) != nil {
		return source, ErrFileWorkNotFound
	}
	return source, nil
}
