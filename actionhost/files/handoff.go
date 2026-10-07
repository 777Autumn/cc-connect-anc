package files

import (
	"context"
	"encoding/json"
	"io"

	"github.com/chenhg5/cc-connect/core"
)

func (h *Host) checkHandoffSnapshots(ctx context.Context, p core.ActionPrincipal, workID string, raw json.RawMessage) (map[string]any, error) {
	var request struct {
		Files []core.CasePublicationFile `json:"files"`
	}
	if strictInput(raw, &request) != nil || len(request.Files) < 1 || len(request.Files) > 4 {
		return fileRefusal(ErrInvalid)
	}
	err := h.store.change(ctx, func(st *state) error {
		w, err := owned(st, p, workID)
		if err != nil {
			return err
		}
		seen := map[string]bool{}
		for _, file := range request.Files {
			d := w.Deliveries[file.DeliveryID]
			if seen[file.DeliveryID] || d == nil || d.Status != "accepted" || d.Version != file.Version || d.SHA256 != file.SHA256 || d.Name != file.Name || d.MessageReceipt != file.MessageReceipt || !w.Messages[d.MessageReceipt] {
				return ErrScope
			}
			seen[file.DeliveryID] = true
			f, err := h.snapshots.Open(d.Snapshot)
			if err != nil {
				return err
			}
			data, err := io.ReadAll(io.LimitReader(f, MaxFileBytes+1))
			_ = f.Close()
			if err != nil || len(data) > MaxFileBytes || hash(data) != d.SHA256 {
				return ErrInvalid
			}
			if err := h.validateFile(ctx, d.Name, data); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fileRefusal(err)
	}
	return map[string]any{"status": "verified"}, nil
}
