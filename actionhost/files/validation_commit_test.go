package files

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"modernc.org/sqlite"
)

var validationDriverID atomic.Int64

// Lose only the acknowledgement, after the real SQLite COMMIT succeeds.
type lostCommitDriver struct{ commits *atomic.Int32 }
type lostCommitConn struct {
	driver.Conn
	commits *atomic.Int32
}

func (d lostCommitDriver) Open(name string) (driver.Conn, error) {
	c, err := (&sqlite.Driver{}).Open(name)
	if err != nil {
		return nil, err
	}
	return lostCommitConn{c, d.commits}, nil
}

func (c lostCommitConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	exec, ok := c.Conn.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	result, err := exec.ExecContext(ctx, query, args)
	if query == "COMMIT" && err == nil {
		c.commits.Add(1)
		return nil, errors.New("test: committed but acknowledgement lost")
	}
	return result, err
}

func TestValidationUncertainCommitNeverReplaysCallback(t *testing.T) {
	f := newFixture(t, nil)
	if err := f.store.db.Close(); err != nil {
		t.Fatal(err)
	}
	var commits atomic.Int32
	name := fmt.Sprintf("validation-lost-commit-%d", validationDriverID.Add(1))
	sql.Register(name, lostCommitDriver{&commits})
	db, err := sql.Open(name, existingURL(f.database))
	if err != nil {
		t.Fatal(err)
	}
	f.store.db = db
	callbacks := 0
	err = f.store.change(context.Background(), func(st *state) error {
		callbacks++
		st.Works[f.context.WorkID].Messages["committed-once"] = true
		return nil
	})
	if !errors.Is(err, ErrUnavailable) || callbacks != 1 || commits.Load() != 1 || !f.store.failed {
		t.Fatalf("uncertain commit: err=%v callbacks=%d commits=%d failed=%v", err, callbacks, commits.Load(), f.store.failed)
	}
	if err := f.store.change(context.Background(), func(*state) error { callbacks++; return nil }); !errors.Is(err, ErrUnavailable) || callbacks != 1 {
		t.Fatal("failed Store replayed callback", err, callbacks)
	}
	fresh, err := Open(f.database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := fresh.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := fresh.change(context.Background(), func(st *state) error {
		if !st.Works[f.context.WorkID].Messages["committed-once"] {
			return errors.New("real commit missing")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
