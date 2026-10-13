package files

import (
	"context"
	"errors"
	"path/filepath"
	"strings"

	"github.com/chenhg5/cc-connect/core"
)

func validPublicationID(id string) bool {
	return id != "" && len(id) <= 200 && !strings.ContainsAny(id, " \t\r\n\x00/\\")
}

// This separate branch deliberately cannot authorize a private receipt via a
// fabricated GroupRealm. The CRM resolves the opaque publication for this
// current member/work; the local ledger then checks every source field.
func (h *Host) publishedArtifact(ctx context.Context, st *state, p core.ActionPrincipal, workID, publicationID, purpose string) (*work, *delivery, core.CasePublishedSource, error) {
	if h.publicationAccess == nil || !validPublicationID(publicationID) || (purpose != "current" && purpose != "revision") || (purpose == "revision" && !strings.HasPrefix(publicationID, "bh_")) {
		return nil, nil, core.CasePublishedSource{}, ErrScope
	}
	grant, err := h.publicationAccess(ctx, p, workID, publicationID, purpose)
	if err != nil {
		if errors.Is(err, core.ErrFileWorkNotFound) || errors.Is(err, ErrScope) {
			return nil, nil, grant, ErrScope
		}
		return nil, nil, grant, errors.Join(ErrUnavailable, err)
	}
	if strings.HasPrefix(publicationID, "bh_") && (grant.Purpose != purpose || !validPublicationID(grant.PublicationBasisID)) {
		return nil, nil, grant, ErrScope
	}
	if grant.ReferenceOnly != (purpose == "revision") {
		return nil, nil, grant, ErrScope
	}
	w := st.Works[grant.WorkID]
	if w == nil || grant.Platform != p.Platform || grant.Platform != w.Principal.Platform || grant.UserID != w.Principal.UserID || grant.ChatID != w.Principal.ChatID || grant.Project != w.Principal.Project || grant.SessionKey != w.Principal.SessionKey {
		return nil, nil, grant, ErrScope
	}
	d := w.Deliveries[grant.DeliveryID]
	if d == nil || d.Status != "accepted" || d.Version != grant.Version || d.SHA256 != grant.SHA256 || d.Name != grant.Name || d.MessageReceipt != grant.MessageReceipt || !w.Messages[d.MessageReceipt] {
		return nil, nil, grant, ErrScope
	}
	grant.Purpose = purpose
	return w, d, grant, nil
}

func publicationPurpose(purpose string) string {
	if purpose == "" {
		return "current"
	}
	return purpose
}

func samePublicationBasis(source *core.FileWorkSource, grant core.CasePublishedSource) bool {
	return publicationPurpose(source.Purpose) == grant.Purpose && source.ReferenceOnly == grant.ReferenceOnly && (source.PublicationBasisID == "" || source.PublicationBasisID == grant.PublicationBasisID)
}

func (h *Host) importCasePublication(ctx context.Context, p core.ActionPrincipal, workID, publicationID, purpose string) (map[string]any, error) {
	err := h.store.change(ctx, func(st *state) error {
		w, err := owned(st, p, workID)
		if err != nil {
			return err
		}
		if !w.Messages[p.MessageID] {
			return ErrScope
		}
		source, artifact, grant, err := h.publishedArtifact(ctx, st, p, workID, publicationID, purpose)
		if err != nil {
			return err
		}
		// Another accepted turn owns its pending import until activation/recovery.
		for message, pending := range w.PendingInputs {
			if message != p.MessageID {
				for _, input := range pending {
					if input.Source != nil && input.Source.PublicationID == publicationID {
						return ErrScope
					}
				}
			}
		}
		for _, input := range append(append([]Input(nil), w.Inputs...), w.PendingInputs[p.MessageID]...) {
			if input.Source != nil && input.Source.PublicationID == publicationID {
				if input.Source.WorkID != source.ID || input.Source.DeliveryID != artifact.DeliveryID || input.Source.Version != artifact.Version || input.Source.MessageReceipt != artifact.MessageReceipt || input.SHA256 != artifact.SHA256 || input.Name != artifact.Name || !samePublicationBasis(input.Source, grant) {
					return ErrScope
				}
				input.Source.Purpose, input.Source.ReferenceOnly, input.Source.PublicationBasisID = grant.Purpose, grant.ReferenceOnly, grant.PublicationBasisID
				st.Schema = max(st.Schema, 5)
				return nil
			}
		}
		if len(w.Inputs)+len(w.PendingInputs[p.MessageID]) >= 128 || len(w.PendingInputs[p.MessageID]) >= 4 {
			return ErrUnavailable
		}
		id := hash([]byte(w.ID + ":publication:" + publicationID))
		input := Input{ID: id, Name: artifact.Name, Path: filepath.Join(w.Root, "inputs", id+strings.ToLower(filepath.Ext(artifact.Name))), SHA256: artifact.SHA256,
			Source: &core.FileWorkSource{WorkID: source.ID, DeliveryID: artifact.DeliveryID, Version: artifact.Version, MessageReceipt: artifact.MessageReceipt, PublicationID: publicationID, Purpose: grant.Purpose, ReferenceOnly: grant.ReferenceOnly, PublicationBasisID: grant.PublicationBasisID}}
		if w.PendingInputs == nil {
			w.PendingInputs = map[string][]Input{}
		}
		w.PendingInputs[p.MessageID] = append(w.PendingInputs[p.MessageID], input)
		// Old binaries must refuse, not drop the publication provenance.
		st.Schema = max(st.Schema, 5)
		return nil
	})
	if err != nil {
		return fileRefusal(err)
	}
	work, err := h.ActivateInputs(ctx, p, workID)
	if err != nil {
		return fileRefusal(err)
	}
	return resultMap(work), nil
}
