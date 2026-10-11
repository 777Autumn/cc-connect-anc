package crmfollowup

import (
	"context"
	"errors"
)

// HandoffCall is reachable only from the host's notice/callback paths.
func (a *Adapter) HandoffCall(ctx context.Context, command string, input map[string]any) (map[string]any, error) {
	switch command {
	case "host-handoff-events", "host-handoff-notify-claim", "host-handoff-notify-ack", "host-handoff-start":
		return a.call(ctx, command, input, "")
	default:
		return nil, errors.New("unsupported handoff host call")
	}
}
