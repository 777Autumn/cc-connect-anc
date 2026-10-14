package files

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/chenhg5/cc-connect/core"
	"github.com/google/uuid"
)

func (h *Host) authorizeBindingSource(ctx context.Context, b Binding) (string, projectGrant, projectGrant, error) {
	var sourceGrant, bindingGrant projectGrant
	boundID := ""
	var err error
	if b.SourceReceipt != "" {
		if !b.Group {
			return "", projectGrant{}, projectGrant{}, ErrScope
		}
		err = h.store.change(ctx, func(st *state) error {
			if _, err := sharedWork(st, b.Principal, b.SourceReceipt); err != nil {
				return err
			}
			for _, w := range st.Works {
				if scope(w.Principal) == scope(b.Principal) && w.SessionID == b.SessionID {
					boundID = w.ID
				}
			}
			return nil
		})
		if err != nil {
			return "", projectGrant{}, projectGrant{}, err
		}
		if boundID == "" {
			boundID = uuid.NewString()
		}
		sourceGrant, err = h.authorizeProject(ctx, b.Principal, "", b.SourceReceipt, false)
		if err != nil {
			return "", projectGrant{}, projectGrant{}, err
		}
		bindingGrant, err = h.authorizeProject(ctx, b.Principal, boundID, b.SourceReceipt, false)
		if err != nil {
			return "", projectGrant{}, projectGrant{}, err
		}
	}
	return boundID, sourceGrant, bindingGrant, nil
}

type inputAuthorization struct {
	pending      []Input
	publications map[string]core.CasePublishedSource
	projects     map[string]projectGrant
}

func (h *Host) authorizePendingInputs(ctx context.Context, p core.ActionPrincipal, workID string) (inputAuthorization, WorkContext, error) {
	var result WorkContext
	var pending []Input
	crossChat := map[string]bool{}
	err := h.store.change(ctx, func(st *state) error {
		w, err := owned(st, p, workID)
		if err != nil {
			return err
		}
		if !w.Messages[p.MessageID] {
			return ErrScope
		}
		pending = w.PendingInputs[p.MessageID]
		for _, input := range pending {
			if input.Source != nil {
				source := st.Works[input.Source.WorkID]
				if source == nil {
					return ErrScope
				}
				crossChat[input.ID] = source.Principal.ChatID != p.ChatID
			}
		}
		result = workContext(w)
		return nil
	})
	if err != nil || len(pending) == 0 {
		return inputAuthorization{}, result, err
	}
	grants := map[string]core.CasePublishedSource{}
	projectGrants := map[string]projectGrant{}
	for _, input := range pending {
		if input.Source != nil && input.Source.PublicationID != "" {
			grant, err := h.authorizePublication(ctx, p, workID, input.Source.PublicationID, publicationPurpose(input.Source.Purpose))
			if err != nil {
				return inputAuthorization{}, WorkContext{}, err
			}
			grants[input.ID] = grant
		} else if input.Source != nil && crossChat[input.ID] {
			grant, err := h.authorizeProject(ctx, p, workID, input.Source.MessageReceipt, true)
			if err != nil {
				return inputAuthorization{}, WorkContext{}, err
			}
			projectGrants[input.ID] = grant
		}
	}
	return inputAuthorization{pending: pending, publications: grants, projects: projectGrants}, result, nil
}

func (h *Host) copyInputSnapshot(ctx context.Context, inputs *os.Root, input Input, snapshot string) error {
	name := filepath.Base(input.Path)
	f, err := h.snapshots.Open(snapshot)
	if err != nil {
		return err
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxFileBytes+1))
	_ = f.Close()
	if err != nil || len(data) > MaxFileBytes || hash(data) != input.SHA256 {
		return ErrInvalid
	}
	if input.Source != nil {
		if err := h.validateFile(ctx, input.Name, data); err != nil {
			return err
		}
	}
	if info, statErr := inputs.Lstat(name); statErr == nil {
		// A crash may occur after the durable copy but before the ledger
		// commit. Reconcile identical host-owned files without rewriting.
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0440 || checkOwner(info, os.Geteuid(), true) != nil {
			return ErrScope
		}
		f, err := inputs.Open(name)
		if err != nil {
			return err
		}
		existing, err := io.ReadAll(io.LimitReader(f, MaxFileBytes+1))
		_ = f.Close()
		if err != nil || hash(existing) != input.SHA256 {
			return ErrInvalid
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	} else if err := writeNew(inputs, name, data, 0440); err != nil {
		return err
	}
	return nil
}
