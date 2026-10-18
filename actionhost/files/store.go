// Package files binds work artifacts and delivery receipts to a trusted session.
package files

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/chenhg5/cc-connect/core"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

var (
	ErrUnavailable = errors.New("file_host_unavailable")
	ErrScope       = errors.New("file_work_scope_mismatch")
	ErrInvalid     = errors.New("invalid_file_request")
	ErrFormat      = errors.New("file_format_unsupported")
	ErrVersion     = errors.New("file_version_conflict")
	ErrContentRead = errors.New("file_content_read_required")
	ErrUncertain   = core.ErrFileWorkUnconfirmed
)

type Store struct {
	db     *sql.DB
	path   string
	file   os.FileInfo
	mu     chan struct{}
	failed bool
}

type state struct {
	Schema int              `json:"schema"`
	Works  map[string]*work `json:"works"`
}

// Initialize is an explicit operator action, never a startup/recovery fallback.
func Initialize(path string) error {
	r, err := protectedRoot(filepath.Dir(path))
	if err != nil || !filepath.IsAbs(path) {
		return ErrUnavailable
	}
	defer func() { _ = r.Close() }()
	info, err := r.Stat(".")
	if err != nil || info.Mode().Perm()&0077 != 0 {
		return ErrUnavailable
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return ErrUnavailable
	}
	if err = f.Close(); err != nil {
		return ErrUnavailable
	}
	db, err := sql.Open("sqlite", existingURL(path))
	if err != nil {
		return ErrUnavailable
	}
	defer func() { _ = db.Close() }()
	_, err = db.Exec(`PRAGMA synchronous=FULL; CREATE TABLE file_state (id INTEGER PRIMARY KEY CHECK(id=1), payload TEXT NOT NULL); INSERT INTO file_state VALUES(1, ?);`, `{"schema":1,"works":{}}`)
	if err != nil {
		return ErrUnavailable
	}
	return nil
}

// Open fails closed for absent/replaced/corrupt storage; uncertain sends stay uncertain.
func Open(path string) (*Store, error) {
	info, err := databaseFile(path)
	if err != nil {
		return nil, ErrUnavailable
	}
	db, err := sql.Open("sqlite", existingURL(path))
	if err != nil {
		return nil, ErrUnavailable
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, path: path, file: info, mu: make(chan struct{}, 1)}
	if _, err = db.Exec(`PRAGMA busy_timeout=50; PRAGMA synchronous=FULL;`); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err = s.change(ctx, func(st *state) error {
			for _, w := range st.Works {
				for _, d := range w.Deliveries {
					if d.Status == "submitted" {
						d.Status = "unknown"
					}
				}
			}
			return nil
		})
	}
	if err != nil {
		_ = db.Close()
		return nil, ErrUnavailable
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func existingURL(path string) string {
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(path), RawQuery: "mode=rw"}).String()
}

func databaseFile(path string) (os.FileInfo, error) {
	root, err := protectedRoot(filepath.Dir(path))
	if err != nil {
		return nil, ErrUnavailable
	}
	defer func() { _ = root.Close() }()
	info, err := root.Lstat(filepath.Base(path))
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || checkOwner(info, os.Geteuid(), true) != nil {
		return nil, ErrUnavailable
	}
	return info, nil
}

// shortcut: one JSON document still serializes local validation and file I/O;
// split the stored state only if measured local transaction latency requires it.
func (s *Store) change(ctx context.Context, fn func(*state) error) error {
	return s.changeExpected(ctx, "", fn)
}

// Recovery pins the raw payload inside this transaction, before decoding it.
func (s *Store) changeExpected(ctx context.Context, expected string, fn func(*state) error) error {
	select {
	case s.mu <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-s.mu }()
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := databaseFile(s.path)
	if s.failed || err != nil || !os.SameFile(s.file, info) {
		s.failed = true
		return ErrUnavailable
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return s.storageError(ctx)
	}
	defer func() { _ = conn.Close() }()
	for {
		_, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE")
		if err == nil {
			break
		}
		var busy *sqlite.Error
		if !errors.As(err, &busy) || busy.Code()&0xff != sqlite3.SQLITE_BUSY {
			return s.storageError(ctx)
		}
		// No callback ran: wait for another project's transaction, never replay
		// business work or retry an uncertain commit. Bound SQLite's own wait to
		// 50ms so cancellation remains observable before the next attempt.
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	defer func() { _, _ = conn.ExecContext(context.Background(), "ROLLBACK") }()
	info, err = databaseFile(s.path)
	if err != nil || !os.SameFile(s.file, info) {
		s.failed = true
		return ErrUnavailable
	}
	var data []byte
	var st state
	if err = conn.QueryRowContext(ctx, "SELECT payload FROM file_state WHERE id=1").Scan(&data); err != nil {
		s.failed = true
		return ErrUnavailable
	}
	if expected != "" && hash(data) != expected {
		return ErrVersion
	}
	if json.Unmarshal(data, &st) != nil || !validState(st) {
		s.failed = true
		return ErrUnavailable
	}
	originalSchema := st.Schema
	if err = fn(&st); err != nil {
		return err
	}
	if st.Schema < originalSchema {
		return ErrUnavailable
	}
	updated, err := json.Marshal(st)
	if err == nil && !bytes.Equal(data, updated) {
		_, err = conn.ExecContext(ctx, "UPDATE file_state SET payload=? WHERE id=1", string(updated))
	}
	if err == nil {
		_, err = conn.ExecContext(ctx, "COMMIT")
	}
	if err != nil {
		return s.storageError(ctx)
	}
	return nil
}

func (s *Store) storageError(ctx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	s.failed = true
	return ErrUnavailable
}

func validState(st state) bool {
	if st.Schema < 1 || st.Schema > 7 || st.Works == nil {
		return false
	}
	sessions, roots := map[string]bool{}, map[string]bool{}
	for id, w := range st.Works {
		if w == nil || id != w.ID || w.ID == "" || !validPrincipal(w.Principal) || w.SessionID == "" || !json.Valid(w.Route) || !filepath.IsAbs(w.Root) || filepath.Clean(w.Root) != w.Root || w.RootIdentity == "" || w.InputIdentity == "" || w.OutputIdentity == "" || w.OwnerUID < 0 || w.Version < 0 || w.Deliveries == nil || w.Messages == nil {
			return false
		}
		key := scope(w.Principal) + ":" + w.SessionID
		if w.GroupRealm != "" && (st.Schema < 2 || !validHash(w.GroupRealm)) {
			return false
		}
		if w.ProjectImportRealm != "" && (st.Schema < 3 || w.GroupRealm != "" || !validHash(w.ProjectImportRealm)) {
			return false
		}
		if sessions[key] || roots[w.RootIdentity] {
			return false
		}
		sessions[key], roots[w.RootIdentity] = true, true
		allInputs := append([]Input(nil), w.Inputs...)
		if len(w.PendingInputs) > 128 {
			return false
		}
		for message, pending := range w.PendingInputs {
			if !w.Messages[message] || len(pending) > 4 {
				return false
			}
			allInputs = append(allInputs, pending...)
		}
		for _, i := range allInputs {
			if !validHash(i.ID) || i.Path != filepath.Join(w.Root, "inputs", i.ID+filepath.Ext(i.Path)) || !safeName(i.Name) || !validHash(i.SHA256) {
				return false
			}
			if i.Source != nil {
				source := st.Works[i.Source.WorkID]
				if source == nil || source.Principal.Platform != w.Principal.Platform {
					return false
				}
				sameGroup := w.GroupRealm != "" && source.GroupRealm == w.GroupRealm && source.Principal.ChatID == w.Principal.ChatID
				privateImport := w.GroupRealm == "" && w.ProjectImportRealm != "" && source.GroupRealm == w.ProjectImportRealm && source.Principal.ChatID != w.Principal.ChatID
				published := i.Source.PublicationID != "" && st.Schema >= 4 && validPublicationID(i.Source.PublicationID)
				if i.Source.PublicationID != "" && !published {
					return false
				}
				if i.Source.Purpose != "" || i.Source.ReferenceOnly || i.Source.PublicationBasisID != "" {
					if st.Schema < 5 || !published || (i.Source.Purpose != "current" && i.Source.Purpose != "revision") || i.Source.ReferenceOnly != (i.Source.Purpose == "revision") {
						return false
					}
					if i.Source.Purpose == "revision" && (!strings.HasPrefix(i.Source.PublicationID, "bh_") || !validPublicationID(i.Source.PublicationBasisID)) {
						return false
					}
				}
				if !sameGroup && !privateImport && !published {
					return false
				}
				d := source.Deliveries[i.Source.DeliveryID]
				if d == nil || d.Status != "accepted" || d.Version != i.Source.Version || d.MessageReceipt != i.Source.MessageReceipt || d.SHA256 != i.SHA256 || d.Name != i.Name {
					return false
				}
			}
		}
		versions := map[int]bool{}
		for path, read := range w.ContentReads {
			if st.Schema < 6 || !relativeFile(path) || read.Path != path || !requiresContentRead(path) || !validHash(read.SHA256) || !validHash(read.ContentSHA256) {
				return false
			}
			if read.ReadVersion == 0 {
				if read.ChunksProvided != 0 || read.ChunksTotal != 0 {
					return false
				}
			} else if st.Schema < 7 || read.ReadVersion != 2 || read.ChunksProvided < 1 || read.ChunksProvided > read.ChunksTotal || read.ChunksTotal > 512 {
				return false
			}
		}
		for id, d := range w.Deliveries {
			if d == nil || (d.ContentSHA256 != "" && (st.Schema < 6 || !validHash(d.ContentSHA256))) {
				return false
			}
			if d == nil || d.DeliveryID != id || d.RequestKey == "" || d.Snapshot == "" || !relativeFile(d.Path) || d.Name != filepath.Base(d.Path) || !validHash(d.SHA256) || d.Version < 1 || d.Version > w.Version || versions[d.Version] {
				return false
			}
			versions[d.Version] = true
			switch d.Status {
			case "submitted", "accepted", "failed", "unknown":
			default:
				return false
			}
			if d.Status == "accepted" && d.MessageReceipt == "" {
				return false
			}
		}
		if len(w.Deliveries) != w.Version {
			return false
		}
	}
	return true
}
