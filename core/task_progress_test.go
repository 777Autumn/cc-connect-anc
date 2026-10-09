package core

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTaskProgressConfigReloadWhileStartingTurns(t *testing.T) {
	e := &Engine{}
	var writers sync.WaitGroup
	writers.Add(1)
	go func() {
		defer writers.Done()
		for i := 0; i < 1000; i++ {
			e.SetDisplayConfig(DisplayCfg{TaskProgress: i%2 == 0})
		}
	}()
	for i := 0; i < 1000; i++ {
		p := &taskStatusPlatform{}
		tp := newTaskProgress(context.Background(), p, "owner", LangEnglish, e.taskProgressEnabled.Load(), nil, time.Now(), time.Hour, time.Second)
		if tp != nil {
			finishTaskStatus(t, tp, MsgTaskEnded)
		}
	}
	writers.Wait()
	e.SetDisplayConfig(DisplayCfg{TaskProgress: true})
	if !e.taskProgressEnabled.Load() {
		t.Fatal("reload did not enable task progress")
	}
}

type taskStatusCall struct {
	route  any
	text   string
	at     time.Time
	create bool
}
type taskStatusPlatform struct {
	stubPlatformEngine
	calls                  []taskStatusCall
	failCreate, failUpdate bool
	gate                   <-chan struct{}
}

func (p *taskStatusPlatform) StartTaskProgress(ctx context.Context, route any, text string) (any, error) {
	p.mu.Lock()
	p.calls = append(p.calls, taskStatusCall{route, text, time.Now(), true})
	fail, gate := p.failCreate, p.gate
	p.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if fail {
		return nil, errors.New("private failure detail")
	}
	return route, nil
}
func (p *taskStatusPlatform) UpdateTaskProgress(_ context.Context, handle any, text string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, taskStatusCall{handle, text, time.Now(), false})
	if p.failUpdate {
		return errors.New("private failure detail")
	}
	return nil
}
func (p *taskStatusPlatform) snapshotCalls() []taskStatusCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]taskStatusCall(nil), p.calls...)
}
func awaitTaskStatus(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("task status did not settle")
}
func finishTaskStatus(t *testing.T, tp *taskProgress, phase MsgKey) {
	t.Helper()
	tp.finish(phase)
	select {
	case <-tp.done:
	case <-time.After(time.Second):
		t.Fatal("progress worker did not exit")
	}
}

func TestTaskProgressShortAndPreStoppedTurnsCreateNothing(t *testing.T) {
	for _, stopped := range []bool{false, true} {
		p := &taskStatusPlatform{}
		stop := make(chan struct{})
		if stopped {
			close(stop)
		}
		tp := newTaskProgress(context.Background(), p, "owner", LangEnglish, true, stop, time.Now(), time.Hour, time.Second)
		finishTaskStatus(t, tp, MsgTaskEnded)
		if len(p.snapshotCalls()) != 0 {
			t.Fatal("short/stopped turn created a card")
		}
	}
}

func TestTaskProgressNoEventsStillCreatesAndCoalescesUpdates(t *testing.T) {
	p := &taskStatusPlatform{}
	start := time.Now()
	tp := newTaskProgress(context.Background(), p, "owner", LangChinese, true, nil, start, 20*time.Millisecond, 30*time.Millisecond)
	awaitTaskStatus(t, func() bool { return len(p.snapshotCalls()) == 1 })
	if got := p.snapshotCalls()[0]; got.at.Sub(start) < 20*time.Millisecond || got.text != "任务处理中。" {
		t.Fatalf("initial status: %+v", got)
	}
	tp.set(MsgTaskActivity, false)
	tp.set(MsgTaskWaiting, false)
	awaitTaskStatus(t, func() bool { return len(p.snapshotCalls()) == 2 })
	calls := p.snapshotCalls()
	if calls[1].text != "等待你的确认。" || calls[1].at.Sub(calls[0].at) < 30*time.Millisecond {
		t.Fatalf("not coalesced/throttled: %+v", calls)
	}
	tp.set(MsgTaskWaiting, false)
	finishTaskStatus(t, tp, MsgTaskEnded) // terminal bypasses the update interval
	calls = p.snapshotCalls()
	if len(calls) != 3 || !strings.Contains(calls[2].text, "本轮处理已结束") {
		t.Fatalf("terminal status: %+v", calls)
	}
	tp.set(MsgTaskActivity, false)
	if len(p.snapshotCalls()) != 3 {
		t.Fatal("late event changed terminal card")
	}
}

func TestTaskProgressFailureNeverRetriesOrBlocksFinish(t *testing.T) {
	for _, createFailure := range []bool{true, false} {
		p := &taskStatusPlatform{failCreate: createFailure, failUpdate: !createFailure}
		tp := newTaskProgress(context.Background(), p, "owner", LangEnglish, true, nil, time.Now().Add(-time.Hour), time.Second, time.Millisecond)
		awaitTaskStatus(t, func() bool { return len(p.snapshotCalls()) == 1 })
		tp.set(MsgTaskActivity, false)
		finishTaskStatus(t, tp, MsgTaskInterrupted)
		count := 2
		if createFailure {
			count = 1
		}
		if len(p.snapshotCalls()) != count {
			t.Fatalf("unexpected retry: %+v", p.snapshotCalls())
		}
	}
}

func TestTaskProgressStopDuringCreationKeepsOriginalRoute(t *testing.T) {
	gate := make(chan struct{})
	p := &taskStatusPlatform{gate: gate}
	stop := make(chan struct{})
	tp := newTaskProgress(context.Background(), p, "owner/turn-one", LangEnglish, true, stop, time.Now().Add(-time.Hour), time.Second, time.Hour)
	awaitTaskStatus(t, func() bool { return len(p.snapshotCalls()) == 1 })
	close(stop)
	tp.finish(MsgTaskStopped)
	close(gate)
	finishTaskStatus(t, tp, MsgTaskInterrupted)
	calls := p.snapshotCalls()
	if len(calls) != 2 || calls[1].text != "Task stopped." || calls[1].route != "owner/turn-one" {
		t.Fatalf("wrong terminal/route: %+v", calls)
	}
}

func TestTaskProgressDistinctTurnsAndShutdown(t *testing.T) {
	p := &taskStatusPlatform{}
	ctx, cancel := context.WithCancel(context.Background())
	a := newTaskProgress(ctx, p, "owner", LangEnglish, true, nil, time.Now().Add(-time.Hour), time.Second, time.Hour)
	b := newTaskProgress(context.Background(), p, "employee", LangEnglish, true, nil, time.Now().Add(-time.Hour), time.Second, time.Hour)
	awaitTaskStatus(t, func() bool { return len(p.snapshotCalls()) == 2 })
	cancel()
	select {
	case <-a.done:
	case <-time.After(time.Second):
		t.Fatal("shutdown leaked worker")
	}
	finishTaskStatus(t, b, MsgTaskEnded)
	for _, c := range p.snapshotCalls() {
		if !c.create && ((c.route == "owner" && !strings.Contains(c.text, "interrupted")) || (c.route == "employee" && !strings.Contains(c.text, "ended"))) {
			t.Fatalf("crossed status: %+v", c)
		}
	}
}

func TestTaskProgressDisabledOrUnsupported(t *testing.T) {
	if newTaskProgress(context.Background(), &taskStatusPlatform{}, "owner", LangEnglish, false, nil, time.Now(), 0, 0) != nil {
		t.Fatal("enabled by default")
	}
	if newTaskProgress(context.Background(), &stubPlatformEngine{}, "owner", LangEnglish, true, nil, time.Now(), 0, 0) != nil {
		t.Fatal("unsupported platform accepted")
	}
}
