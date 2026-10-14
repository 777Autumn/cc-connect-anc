package files

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func TestPublicationActivationConcurrentSameMessageIsIdempotent(t *testing.T) {
	f, target, work, grant, _, sends := publicationFixture(t)
	var calls atomic.Int32
	f.host.SetPublicationAccess(func(context.Context, core.ActionPrincipal, string, string, string) (core.CasePublishedSource, error) {
		if calls.Add(1) == 2 {
			return grant, ErrScope
		}
		return grant, nil
	})
	if _, err := f.host.Tool(context.Background(), "host-case-import", json.RawMessage(`{"publication_id":"pub_selected"}`), target.Principal, work.WorkID); err == nil {
		t.Fatal("fixture must leave pending import")
	}
	entered, release := make(chan struct{}, 2), make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	f.host.SetPublicationAccess(func(ctx context.Context, _ core.ActionPrincipal, _, _, _ string) (core.CasePublishedSource, error) {
		entered <- struct{}{}
		select {
		case <-release:
			return grant, nil
		case <-ctx.Done():
			return grant, ctx.Err()
		}
	})
	done := make(chan error, 2)
	for range 2 {
		go func() {
			current, err := f.host.ActivateInputs(ctx, target.Principal, work.WorkID)
			if err == nil && len(current.Inputs) != 1 {
				err = ErrInvalid
			}
			done <- err
		}()
	}
	for range 2 {
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("activation did not release the lock")
		}
	}
	close(release)
	for range 2 {
		if err := <-done; err != nil {
			t.Error("same pending material must activate once and succeed for both callers", err)
		}
	}
	if *sends != 1 {
		t.Fatal("activation resent delivery")
	}
}

func TestPublicationAuthorizationCancellationNeverPublishesBytes(t *testing.T) {
	for _, stage := range []int{1, 2} {
		for _, returnGrant := range []bool{false, true} {
			t.Run(string(rune('0'+stage))+"/grant="+map[bool]string{true: "yes", false: "no"}[returnGrant], func(t *testing.T) {
				f, target, work, grant, _, sends := publicationFixture(t)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				calls := 0
				f.host.SetPublicationAccess(func(context.Context, core.ActionPrincipal, string, string, string) (core.CasePublishedSource, error) {
					calls++
					if calls == stage {
						cancel()
						if !returnGrant {
							return grant, ctx.Err()
						}
					}
					return grant, nil
				})
				_, err := f.host.Tool(ctx, "host-case-import", json.RawMessage(`{"publication_id":"pub_selected"}`), target.Principal, work.WorkID)
				if !errors.Is(err, context.Canceled) || calls != stage {
					t.Fatal("cancelled authorization was committed or repeated", err, calls)
				}
				entries, err := os.ReadDir(work.WorkRoot + "/inputs")
				if err != nil || len(entries) != 0 || *sends != 1 {
					t.Fatal("cancelled import published bytes", entries, err, *sends)
				}
				if _, err := f.host.FindByMessage(context.Background(), target.Principal, target.Principal.MessageID); err != nil {
					t.Fatal("cancellation poisoned Store", err)
				}
			})
		}
	}
}

func TestPublicationActivationRevalidatesWaitingSnapshot(t *testing.T) {
	for _, mutation := range []string{"pending_removed", "pending_replaced", "source_actor_changed", "unrelated_work"} {
		t.Run(mutation, func(t *testing.T) {
			f, target, work, grant, _, _ := publicationFixture(t)
			entered, release := make(chan struct{}), make(chan struct{})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			calls := 0
			f.host.SetPublicationAccess(func(ctx context.Context, _ core.ActionPrincipal, _, _, _ string) (core.CasePublishedSource, error) {
				calls++
				if calls == 2 {
					close(entered)
					select {
					case <-release:
					case <-ctx.Done():
						return grant, ctx.Err()
					}
				}
				return grant, nil
			})
			done := make(chan error, 1)
			go func() {
				_, err := f.host.Tool(ctx, "host-case-import", json.RawMessage(`{"publication_id":"pub_selected"}`), target.Principal, work.WorkID)
				done <- err
			}()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("activation never reached authorization")
			}
			err := f.store.change(ctx, func(st *state) error {
				w := st.Works[work.WorkID]
				switch mutation {
				case "pending_removed":
					delete(w.PendingInputs, target.Principal.MessageID)
				case "pending_replaced":
					input := w.PendingInputs[target.Principal.MessageID][0]
					input.ID = hash([]byte("replacement"))
					input.Path = filepath.Join(w.Root, "inputs", input.ID+".xlsx")
					w.PendingInputs[target.Principal.MessageID] = []Input{input}
				case "source_actor_changed":
					st.Works[grant.WorkID].Principal.UserID = "other-source-owner"
				case "unrelated_work":
					st.Works[grant.WorkID].Messages["unrelated-read"] = true
				}
				return nil
			})
			close(release)
			if err != nil {
				t.Fatal(err)
			}
			err = <-done
			if mutation == "unrelated_work" {
				if err != nil {
					t.Fatal("unrelated write invalidated this import", err)
				}
				return
			}
			if !errors.Is(err, ErrScope) {
				t.Fatal("waiting snapshot change accepted", err)
			}
			entries, err := os.ReadDir(filepath.Join(work.WorkRoot, "inputs"))
			if err != nil || len(entries) != 0 {
				t.Fatal("changed source published bytes", entries, err)
			}
			if calls != 2 {
				t.Fatal("authorization replayed", calls)
			}
		})
	}
}
