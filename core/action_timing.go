package core

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"
)

type actionToolTimingKey struct{}

var actionToolTimingSequence atomic.Uint64

// ActionToolTimingID correlates host timing only; it grants no session access.
func ActionToolTimingID(ctx context.Context) string {
	id, _ := ctx.Value(actionToolTimingKey{}).(string)
	return id
}

func timingMessageHash(id string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(id)))
}

func logQueuedMessageTiming(queued queuedMessage) {
	if !queued.enqueuedAt.IsZero() {
		slog.Info("queued message timing", "message_sha256", timingMessageHash(queued.messageID),
			"elapsed_ns", time.Since(queued.enqueuedAt).Nanoseconds())
	}
}
