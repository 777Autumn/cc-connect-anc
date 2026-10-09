package core

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// TaskProgressPlatform must make at most one message API attempt per call.
// Preview APIs with implicit retries cannot guarantee a single status message
// when the remote service accepts a create but loses its response.
type TaskProgressPlatform interface {
	StartTaskProgress(context.Context, any, string) (any, error)
	UpdateTaskProgress(context.Context, any, string) error
}

// taskProgress owns one turn's fixed reply route. Only host-selected message
// keys cross this boundary; agent text and tool payloads are never accepted.
type taskProgress struct {
	mu    sync.Mutex
	phase MsgKey
	ended bool
	stop  <-chan struct{}
	wake  chan struct{}
	done  chan struct{}
}

func newTaskProgress(ctx context.Context, p Platform, replyCtx any, lang Language, enabled bool, stop <-chan struct{}, start time.Time, delay, interval time.Duration) *taskProgress {
	sender, supported := p.(TaskProgressPlatform)
	if !enabled || !supported {
		return nil
	}
	tp := &taskProgress{phase: MsgTaskProcessing, stop: stop, wake: make(chan struct{}, 1), done: make(chan struct{})}
	go tp.run(ctx, sender, replyCtx, NewI18n(lang), start, delay, interval)
	return tp
}

func (tp *taskProgress) set(phase MsgKey, end bool) {
	if tp == nil {
		return
	}
	tp.mu.Lock()
	if !tp.ended {
		tp.phase, tp.ended = phase, end
	}
	tp.mu.Unlock()
	select {
	case tp.wake <- struct{}{}:
	default:
	}
}

func (tp *taskProgress) finish(phase MsgKey) { tp.set(phase, true) }

// watchStop attaches the native session once startup has returned.
func (tp *taskProgress) watchStop(stop <-chan struct{}) {
	if tp == nil {
		return
	}
	tp.mu.Lock()
	tp.stop = stop
	tp.mu.Unlock()
	select {
	case tp.wake <- struct{}{}:
	default:
	}
}

func (tp *taskProgress) snapshot() (MsgKey, bool, <-chan struct{}) {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	return tp.phase, tp.ended, tp.stop
}

func (tp *taskProgress) run(ctx context.Context, sender TaskProgressPlatform, replyCtx any, tr *I18n, start time.Time, delay, interval time.Duration) {
	defer close(tp.done)
	timer := time.NewTimer(time.Until(start.Add(delay)))
	defer timer.Stop()
	var handle any
	var sent MsgKey
	var lastSent time.Time
	ready := false
	stopped := false
	for {
		_, _, stop := tp.snapshot()
		if stopped {
			stop = nil
		}
		select {
		case <-ctx.Done():
			tp.finish(MsgTaskInterrupted)
		case <-stop:
			stopped, stop = true, nil
		case <-tp.wake:
		case <-timer.C:
			ready = true
		}
		// Startup may attach a stopped session while the initial timer fires.
		phase, ended, currentStop := tp.snapshot()
		if !stopped {
			stop = currentStop
		}
		select {
		case <-stop:
			stopped, stop = true, nil
		default:
		}
		if (ended || stopped) && handle == nil {
			return
		}
		if !ended && (stopped || !ready || phase == sent) {
			continue
		}
		if !ended && handle != nil && time.Since(lastSent) < interval {
			timer.Reset(time.Until(lastSent.Add(interval)))
			continue
		}
		// A terminal update is best effort even during engine shutdown. A slow
		// platform never blocks the Engine's cancellation/event loop.
		parent := ctx
		if ended {
			parent = context.WithoutCancel(ctx)
		}
		callCtx, cancel := context.WithTimeout(parent, 2*time.Second)
		var err error
		if handle == nil {
			handle, err = sender.StartTaskProgress(callCtx, replyCtx, tr.T(phase))
		} else {
			err = sender.UpdateTaskProgress(callCtx, handle, tr.T(phase))
		}
		cancel()
		if err != nil || handle == nil {
			slog.Warn("task progress unavailable; no retry")
			return
		}
		sent, lastSent = phase, time.Now()
		if ended {
			return
		}
	}
}
