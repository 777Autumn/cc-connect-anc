package files

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
	"modernc.org/sqlite"
)

// Uses the real publication import/activation and two independently opened
// Stores on one SQLite file, matching configureFileWork's per-project setup.
// Only the remote authorization service is replaced by a channel-controlled stub.
func blockedPublication(t *testing.T, blockCall int) (fixture, *Store, *Host, func(), <-chan error, *atomic.Int32) {
	t.Helper()
	f, target, work, grant, _, _ := publicationFixture(t)
	other, err := Open(f.database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close() })
	otherHost, err := New(other, f.snapshots, f.host.send)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = otherHost.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	entered, released := make(chan struct{}), make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(released) }) }
	t.Cleanup(release)
	calls := &atomic.Int32{}
	grant.Purpose, grant.PublicationBasisID = "current", "chg_lock_fixture"
	f.host.SetPublicationAccess(func(ctx context.Context, p core.ActionPrincipal, id, pub, purpose string) (core.CasePublishedSource, error) {
		if p != target.Principal || id != work.WorkID || pub != "bh_chg_lock_fixture_0" || purpose != "current" {
			return core.CasePublishedSource{}, ErrScope
		}
		if int(calls.Add(1)) == blockCall {
			close(entered)
			select {
			case <-released:
			case <-ctx.Done():
				return core.CasePublishedSource{}, ctx.Err()
			}
		}
		return grant, nil
	})
	done := make(chan error, 1)
	go func() {
		result, err := f.host.Tool(ctx, "host-case-import", json.RawMessage(`{"publication_id":"bh_chg_lock_fixture_0"}`), target.Principal, work.WorkID)
		if err == nil {
			raw, marshalErr := json.Marshal(result)
			var imported WorkContext
			if marshalErr != nil {
				err = marshalErr
			} else if err = json.Unmarshal(raw, &imported); err == nil && (len(imported.Inputs) != 1 || imported.Inputs[0].SHA256 != grant.SHA256) {
				err = fmt.Errorf("publication import lost the authorized input")
			}
		}
		done <- err
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("authorization callback not entered", ctx.Err())
	}
	return f, other, otherHost, release, done, calls
}

// Evidence only: an independent driver connection observes the actual SQLite
// result while the real authorization callback is blocked, without changing data.
func observePublicationWriter(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", existingURL(path))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	_, err = db.Exec("BEGIN IMMEDIATE")
	if err == nil {
		if _, err = db.Exec("ROLLBACK"); err != nil {
			t.Fatal(err)
		}
		t.Log("diagnostic: authorization callback did not hold SQLite write lock")
		return
	}
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) {
		t.Fatal("unexpected observer database error", err)
	}
	t.Logf("diagnostic: callback holds writer; SQLite code=%d error=%v", sqliteErr.Code(), err)
}

func TestSharedLedgerPublicationContentionRecoversSameStore(t *testing.T) {
	for _, tc := range []struct {
		name string
		call int
	}{{"import_authorization", 1}, {"activation_authorization", 2}} {
		t.Run(tc.name, func(t *testing.T) {
			f, other, otherHost, release, done, calls := blockedPublication(t, tc.call)
			observePublicationWriter(t, f.database)
			// The callback-entry handshake establishes the authorization window.
			// Retain the historical 5.5s condition while reads now finish inside it.
			timer := time.AfterFunc(5500*time.Millisecond, release)
			defer timer.Stop()
			start := time.Now()
			ref, firstErr := otherHost.FindByMessage(context.Background(), f.binding.Principal, f.binding.Principal.MessageID)
			t.Logf("lookup elapsed=%s first_error=%v other_store_failed=%v", time.Since(start), firstErr, other.failed)
			if err := <-done; err != nil {
				t.Fatal("original publication did not finish", err)
			}
			if calls.Load() != 2 {
				t.Errorf("import and activation must authorize exactly once each; got %d", calls.Load())
			}
			if firstErr != nil || ref.WorkID != f.context.WorkID {
				t.Errorf("transient writer contention rejected an existing work: ref=%+v err=%v", ref, firstErr)
			}
			ref, err := otherHost.FindByMessage(context.Background(), f.binding.Principal, f.binding.Principal.MessageID)
			if err != nil || ref.WorkID != f.context.WorkID {
				t.Errorf("same Store remains unavailable after writer completed: ref=%+v err=%v failed=%v", ref, err, other.failed)
			}
			fresh := f.binding.Principal
			fresh.MessageID = "fresh-top-level-request"
			if _, err := otherHost.FindByMessage(context.Background(), fresh, fresh.MessageID); !errors.Is(err, core.ErrFileWorkNotFound) {
				t.Errorf("fresh top-level request cannot reach new-work branch: %v", err)
			}
		})
	}
}

func TestSharedLedgerWaitCancellationDoesNotPoisonStore(t *testing.T) {
	for _, shared := range []bool{false, true} {
		t.Run(fmt.Sprint(shared), func(t *testing.T) {
			f := newFixture(t, nil)
			other, otherHost := f.store, f.host
			if shared {
				var err error
				other, err = Open(f.database)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = other.Close() }()
				otherHost, err = New(other, f.snapshots, f.host.send)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = otherHost.Close() }()
			}
			entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
			go func() {
				done <- f.store.change(context.Background(), func(*state) error { close(entered); <-release; return nil })
			}()
			<-entered
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			read := make(chan error, 1)
			go func() {
				_, err := otherHost.FindByMessage(ctx, f.binding.Principal, f.binding.Principal.MessageID)
				read <- err
			}()
			var err error
			select {
			case err = <-read:
			case <-time.After(500 * time.Millisecond):
				t.Error("cancelled lock waiter did not return within 400ms of cancellation")
				close(release)
				release = nil
				err = <-read
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("waiting request did not retain its deadline: %v", err)
			}
			if release != nil {
				close(release)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			ref, err := otherHost.FindByMessage(context.Background(), f.binding.Principal, f.binding.Principal.MessageID)
			if err != nil || ref.WorkID != f.context.WorkID || other.failed {
				t.Errorf("cancelled waiter changed durable state: err=%v failed=%v", err, other.failed)
			}
		})
	}
}
