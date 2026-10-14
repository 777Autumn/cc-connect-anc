package files

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// Timers are injected by the experiment's build overlay; shipping source is unchanged.
var ledgerTimingOverlay bool

type ledgerTimingKey struct{}
type ledgerTimings map[string]float64

func validationObserve(ctx context.Context, phase string, start time.Time) {
	if values, ok := ctx.Value(ledgerTimingKey{}).(ledgerTimings); ok {
		values[phase] += float64(time.Since(start)) / float64(time.Millisecond)
	}
}

func TestValidationTimingAccumulatesOnlySelectedContext(t *testing.T) {
	values := ledgerTimings{}
	ctx := context.WithValue(context.Background(), ledgerTimingKey{}, values)
	start := time.Now().Add(-time.Second)
	validationObserve(ctx, "mutex", start)
	first := values["mutex"]
	validationObserve(ctx, "mutex", start)
	validationObserve(context.Background(), "sqlite_begin", start)
	if first < 1000 || values["mutex"] < first+1000 || len(values) != 1 {
		t.Fatal("timing accumulation or context isolation", values)
	}
}
func experimentRecord(t *testing.T, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	t.Log("VALIDATION_JSON " + string(raw))
}
func experimentEnabled(t *testing.T) {
	t.Helper()
	if os.Getenv("MYANC_LEDGER_EXPERIMENT") != "1" {
		t.Skip("explicit isolated experiment only")
	}
	if !ledgerTimingOverlay {
		t.Fatal("use ops/build_ledger_experiment.py timing overlay")
	}
}

func TestValidationLedgerMatrix(t *testing.T) {
	experimentEnabled(t)
	for _, topology := range []string{"same_store", "shared_sqlite", "independent_sqlite"} {
		for _, stage := range []int{1, 2} {
			for _, delay := range []time.Duration{0, 5500 * time.Millisecond, 30 * time.Second} {
				for repeat := 1; repeat <= 3; repeat++ {
					t.Run(fmt.Sprintf("%s/stage%d/%dms/%d", topology, stage, delay.Milliseconds(), repeat), func(t *testing.T) { runLedgerExperiment(t, topology, stage, delay, false, repeat) })
				}
			}
		}
	}
}
func TestValidationLedgerCancellation(t *testing.T) {
	experimentEnabled(t)
	for _, topology := range []string{"same_store", "shared_sqlite", "independent_sqlite"} {
		for _, stage := range []int{1, 2} {
			t.Run(fmt.Sprintf("%s/%d", topology, stage), func(t *testing.T) { runLedgerExperiment(t, topology, stage, 1500*time.Millisecond, true, 1) })
		}
	}
}

type readResult struct {
	Operation            string        `json:"operation"`
	TotalMS              float64       `json:"total_ms"`
	Phases               ledgerTimings `json:"phases_ms"`
	Error                string        `json:"error,omitempty"`
	CancellationReturnMS *float64      `json:"cancel_to_return_ms,omitempty"`
	CancellationOutcome  string        `json:"cancellation_outcome,omitempty"`
}

func measureLedgerReads(t *testing.T, ctx context.Context, reader *Host, readerFixture fixture, deliveryID, topology string, cancelWait bool, startWait chan struct{}) []readResult {
	results := make(chan readResult, 3)
	var ready sync.WaitGroup
	ready.Add(3)
	for _, operation := range []string{"message_lookup", "work_context", "file_status"} {
		go func(operation string) {
			phases := ledgerTimings{}
			readCtx := context.WithValue(ctx, ledgerTimingKey{}, phases)
			var cancel context.CancelFunc
			var cancelAt time.Time
			if cancelWait {
				cancelAt = time.Now().Add(100 * time.Millisecond)
				readCtx, cancel = context.WithDeadline(readCtx, cancelAt)
				defer cancel()
			}
			ready.Done()
			<-startWait
			start := time.Now()
			var err error
			switch operation {
			case "message_lookup":
				var ref WorkRef
				ref, err = reader.FindByMessage(readCtx, readerFixture.binding.Principal, readerFixture.binding.Principal.MessageID)
				if err == nil && ref.WorkID != readerFixture.context.WorkID {
					err = fmt.Errorf("wrong work")
				}
			case "work_context":
				_, err = reader.Tool(readCtx, "work-context", json.RawMessage(`{}`), readerFixture.binding.Principal, readerFixture.context.WorkID)
			case "file_status":
				raw, _ := json.Marshal(map[string]string{"work_id": readerFixture.context.WorkID, "delivery_id": deliveryID})
				_, err = reader.Tool(readCtx, "file-status", raw, readerFixture.binding.Principal, readerFixture.context.WorkID)
			}
			row := readResult{Operation: operation, TotalMS: float64(time.Since(start)) / float64(time.Millisecond), Phases: phases}
			if err != nil {
				row.Error = err.Error()
			}
			if !cancelAt.IsZero() {
				elapsed := float64(time.Since(cancelAt)) / float64(time.Millisecond)
				if errors.Is(err, context.DeadlineExceeded) {
					row.CancellationReturnMS = &elapsed
					row.CancellationOutcome = "deadline_observed"
				} else if err != nil || elapsed >= 0 {
					t.Error("cancellation outcome", err)
				} else {
					row.CancellationOutcome = "completed_before_deadline"
				}
			}
			results <- row
		}(operation)
	}
	ready.Wait()
	close(startWait)
	rows := []readResult{}
	for range 3 {
		rows = append(rows, <-results)
	}
	return rows
}

func runLedgerExperiment(t *testing.T, topology string, stage int, delay time.Duration, cancelWait bool, repeat int) {
	f, target, work, grant, _, sends := publicationFixture(t)
	reader, readerStore, readerFixture, deliveryID := f.host, f.store, f, grant.DeliveryID
	switch topology {
	case "shared_sqlite":
		var err error
		readerStore, err = Open(f.database)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = readerStore.Close() })
		reader, err = New(readerStore, f.snapshots, f.host.send)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = reader.Close() })
	case "independent_sqlite":
		other, _, _, otherGrant, _, _ := publicationFixture(t)
		reader, readerStore, readerFixture, deliveryID = other.host, other.store, other, otherGrant.DeliveryID
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	entered, startWait := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	var authorizationMS float64
	grant.Purpose, grant.PublicationBasisID = "current", "chg_validation_fixture"
	f.host.SetPublicationAccess(func(ctx context.Context, p core.ActionPrincipal, id, pub, purpose string) (core.CasePublishedSource, error) {
		if p != target.Principal || id != work.WorkID || pub != "bh_chg_validation_fixture_0" || purpose != "current" {
			return core.CasePublishedSource{}, ErrScope
		}
		if int(calls.Add(1)) == stage {
			close(entered)
			select {
			case <-startWait:
			case <-ctx.Done():
				return core.CasePublishedSource{}, ctx.Err()
			}
			start := time.Now()
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-ctx.Done():
				return core.CasePublishedSource{}, ctx.Err()
			}
			authorizationMS = float64(time.Since(start)) / float64(time.Millisecond)
		}
		return grant, nil
	})
	done := make(chan error, 1)
	importStart := time.Now()
	var importMS float64
	go func() {
		_, err := f.host.Tool(ctx, "host-case-import", json.RawMessage(`{"publication_id":"bh_chg_validation_fixture_0"}`), target.Principal, work.WorkID)
		importMS = float64(time.Since(importStart)) / float64(time.Millisecond)
		done <- err
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("authorization not entered")
	}
	rows := measureLedgerReads(t, ctx, reader, readerFixture, deliveryID, topology, cancelWait, startWait)
	importErr := <-done
	ref, reuseErr := reader.FindByMessage(context.Background(), readerFixture.binding.Principal, readerFixture.binding.Principal.MessageID)
	reused := reuseErr == nil && ref.WorkID == readerFixture.context.WorkID && !readerStore.failed
	value := map[string]any{"kind": "contention", "topology": topology, "authorization_stage": stage, "delay_ms": delay.Milliseconds(), "repeat": repeat, "cancel": cancelWait, "authorization_wait_ms": authorizationMS, "import_total_ms": importMS, "authorization_calls": calls.Load(), "source_sends": *sends, "post_wait_reusable": reused, "reads": rows}
	if importErr != nil {
		value["import_error"] = importErr.Error()
	}
	experimentRecord(t, value)
	if importErr != nil || !reused || calls.Load() != 2 || *sends != 1 {
		t.Error("import/reuse/replay invariant failed")
	}
	if !cancelWait {
		for _, r := range rows {
			if r.Error != "" {
				t.Error(r.Operation, r.Error)
			}
		}
	}
}

func TestValidationLedgerHistory(t *testing.T) {
	experimentEnabled(t)
	for _, size := range []int{100, 1000, 10000} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			f := newFixture(t, nil)
			err := f.store.change(context.Background(), func(st *state) error {
				template := *st.Works[f.context.WorkID]
				for i := 1; i < size; i++ {
					w := template
					id := fmt.Sprintf("synthetic-%d", i)
					w.ID, w.SessionID, w.RootIdentity = id, id, id
					w.Messages = map[string]bool{id: true}
					w.Root = filepath.Join(filepath.Dir(template.Root), id)
					st.Works[id] = &w
				}
				if !validState(*st) {
					return fmt.Errorf("invalid synthetic state")
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			var payloadBytes int64
			if err = f.store.db.QueryRow("SELECT length(CAST(payload AS BLOB)) FROM file_state WHERE id=1").Scan(&payloadBytes); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(f.database)
			if err != nil {
				t.Fatal(err)
			}
			for repeat := 1; repeat <= 3; repeat++ {
				phases := ledgerTimings{}
				ctx := context.WithValue(context.Background(), ledgerTimingKey{}, phases)
				start := time.Now()
				ref, err := f.host.FindByMessage(ctx, f.binding.Principal, f.binding.Principal.MessageID)
				elapsed := time.Since(start)
				if err != nil || ref.WorkID != f.context.WorkID {
					t.Fatal("history lookup", err)
				}
				experimentRecord(t, map[string]any{"kind": "history", "synthetic_works": size, "repeat": repeat, "payload_bytes": payloadBytes, "sqlite_bytes": info.Size(), "total_ms": float64(elapsed) / float64(time.Millisecond), "phases_ms": phases})
			}
		})
	}
}

func TestValidationLedgerAuthorizationCancellation(t *testing.T) {
	experimentEnabled(t)
	for _, stage := range []int{1, 2} {
		t.Run(fmt.Sprint(stage), func(t *testing.T) {
			f, target, work, grant, _, sends := publicationFixture(t)
			grant.Purpose, grant.PublicationBasisID = "current", "chg_validation_cancel"
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			entered := make(chan struct{})
			var calls atomic.Int32
			f.host.SetPublicationAccess(func(ctx context.Context, _ core.ActionPrincipal, _, _, _ string) (core.CasePublishedSource, error) {
				if int(calls.Add(1)) == stage {
					close(entered)
					<-ctx.Done()
					return core.CasePublishedSource{}, ctx.Err()
				}
				return grant, nil
			})
			done := make(chan error, 1)
			go func() {
				_, err := f.host.Tool(ctx, "host-case-import", json.RawMessage(`{"publication_id":"bh_chg_validation_cancel_0"}`), target.Principal, work.WorkID)
				done <- err
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("authorization not entered")
			}
			start := time.Now()
			cancel()
			err := <-done
			elapsed := float64(time.Since(start)) / float64(time.Millisecond)
			ref, reuseErr := f.host.FindByMessage(context.Background(), f.binding.Principal, f.binding.Principal.MessageID)
			reused := reuseErr == nil && ref.WorkID == f.context.WorkID && !f.store.failed
			experimentRecord(t, map[string]any{"kind": "authorization_cancellation", "authorization_stage": stage, "cancel_to_return_ms": elapsed, "error": fmt.Sprint(err), "post_wait_reusable": reused, "authorization_calls": calls.Load(), "source_sends": *sends})
			if !errors.Is(err, context.Canceled) || !reused || int(calls.Load()) != stage || *sends != 1 {
				t.Fatal("cancel/reuse/replay invariant", err)
			}
		})
	}
}
