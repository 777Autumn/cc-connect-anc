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
func (h *Host) publishedArtifact(ctx context.Context, st *state, p core.ActionPrincipal, workID, publicationID string) (*work, *delivery, error) {
	if h.publicationAccess == nil || !validPublicationID(publicationID) {
		return nil, nil, ErrScope
	}
	grant, err := h.publicationAccess(ctx, p, workID, publicationID)
	if err != nil {
		if errors.Is(err, core.ErrFileWorkNotFound) || errors.Is(err, ErrScope) {
			return nil, nil, ErrScope
		}
		return nil, nil, errors.Join(ErrUnavailable, err)
	}
	w := st.Works[grant.WorkID]
	if w == nil || grant.Platform != p.Platform || grant.Platform != w.Principal.Platform || grant.UserID != w.Principal.UserID || grant.ChatID != w.Principal.ChatID || grant.Project != w.Principal.Project || grant.SessionKey != w.Principal.SessionKey {
		return nil, nil, ErrScope
	}
	d := w.Deliveries[grant.DeliveryID]
	if d == nil || d.Status != "accepted" || d.Version != grant.Version || d.SHA256 != grant.SHA256 || d.Name != grant.Name || d.MessageReceipt != grant.MessageReceipt || !w.Messages[d.MessageReceipt] {
		return nil, nil, ErrScope
	}
	return w, d, nil
}

func (h *Host) importCasePublication(ctx context.Context, p core.ActionPrincipal, workID, publicationID string) (map[string]any, error) {
	err := h.store.change(ctx, func(st *state) error {
		w, err := owned(st, p, workID)
		if err != nil {
			return err
		}
		if !w.Messages[p.MessageID] {
			return ErrScope
		}
		source, artifact, err := h.publishedArtifact(ctx, st, p, workID, publicationID)
		if err != nil {
			return err
		}
		for _, input := range append(append([]Input(nil), w.Inputs...), w.PendingInputs[p.MessageID]...) {
			if input.Source != nil && input.Source.PublicationID == publicationID {
				if input.Source.WorkID != source.ID || input.Source.DeliveryID != artifact.DeliveryID || input.Source.Version != artifact.Version || input.Source.MessageReceipt != artifact.MessageReceipt || input.SHA256 != artifact.SHA256 || input.Name != artifact.Name {
					return ErrScope
				}
				return nil
			}
		}
		if len(w.Inputs)+len(w.PendingInputs[p.MessageID]) >= 128 || len(w.PendingInputs[p.MessageID]) >= 4 {
			return ErrUnavailable
		}
		id := hash([]byte(w.ID + ":publication:" + publicationID))
		input := Input{ID: id, Name: artifact.Name, Path: filepath.Join(w.Root, "inputs", id+strings.ToLower(filepath.Ext(artifact.Name))), SHA256: artifact.SHA256,
			Source: &core.FileWorkSource{WorkID: source.ID, DeliveryID: artifact.DeliveryID, Version: artifact.Version, MessageReceipt: artifact.MessageReceipt, PublicationID: publicationID}}
		if w.PendingInputs == nil {
			w.PendingInputs = map[string][]Input{}
		}
		w.PendingInputs[p.MessageID] = append(w.PendingInputs[p.MessageID], input)
		// Old binaries must refuse, not drop the publication provenance.
		st.Schema = 4
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
