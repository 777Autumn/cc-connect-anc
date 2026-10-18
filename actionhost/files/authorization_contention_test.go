package files

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func TestPublicationAuthorizationDoesNotLockOtherWork(t *testing.T) {
	testAuthorizationDoesNotLockOtherWork(t, false)
}

func TestProjectAuthorizationDoesNotLockOtherWork(t *testing.T) {
	testAuthorizationDoesNotLockOtherWork(t, true)
}

func testAuthorizationDoesNotLockOtherWork(t *testing.T, project bool) {
	for _, shared := range []bool{false, true} {
		for _, stage := range []int32{1, 2} {
			t.Run(fmt.Sprintf("shared=%v/stage=%d", shared, stage), func(t *testing.T) {
				f, target, work, grant, _, sends := publicationFixture(t)
				reader := f.host
				if shared {
					store, err := Open(f.database)
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = store.Close() }()
					reader, err = New(store, f.snapshots, f.host.send)
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = reader.Close() }()
				}
				entered, release := make(chan struct{}), make(chan struct{})
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				var calls atomic.Int32
				wait := func(ctx context.Context) error {
					if calls.Add(1) == stage {
						close(entered)
						select {
						case <-release:
						case <-ctx.Done():
							return ctx.Err()
						}
					}
					return nil
				}
				f.host.SetPublicationAccess(func(ctx context.Context, _ core.ActionPrincipal, _, _, _ string) (core.CasePublishedSource, error) {
					return grant, wait(ctx)
				})
				request := json.RawMessage(`{"publication_id":"pub_selected"}`)
				if project {
					realm := hash([]byte("fixture-project-realm"))
					if err := f.store.change(ctx, func(st *state) error {
						st.Works[grant.WorkID].GroupRealm = realm
						st.Works[grant.WorkID].DeliveryAudience = "group"
						st.Schema = max(st.Schema, 2)
						return nil
					}); err != nil {
						t.Fatal(err)
					}
					f.host.SetProjectAccess(func(ctx context.Context, _ core.ActionPrincipal, _, _ string, _ bool) (string, string, error) {
						return grant.ChatID, realm, wait(ctx)
					})
					request = json.RawMessage(`{"receipt":"private-receipt-1"}`)
				}
				done := make(chan error, 1)
				go func() {
					_, err := f.host.Tool(ctx, "host-case-import", request, target.Principal, work.WorkID)
					done <- err
				}()
				select {
				case <-entered:
				case <-ctx.Done():
					t.Fatal("authorization never entered")
				}
				read := make(chan error, 1)
				go func() {
					ref, err := reader.FindByMessage(ctx, f.binding.Principal, f.binding.Principal.MessageID)
					if err == nil && ref.WorkID != f.context.WorkID {
						err = ErrScope
					}
					read <- err
				}()
				select {
				case err := <-read:
					if err != nil {
						t.Error(err)
					}
				case <-time.After(300 * time.Millisecond):
					t.Error("other work blocked behind external authorization")
				}
				close(release)
				if err := <-done; err != nil {
					t.Fatal(err)
				}
				if calls.Load() != 2 || *sends != 1 {
					t.Fatal("authorization callback or delivery replayed", calls.Load(), *sends)
				}
			})
		}
	}
}
