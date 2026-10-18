package files

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
)

// DeviceRecoveryRequest is a frozen operator manifest, never an agent tool input.
type DeviceRecoveryRequest struct {
	StateSHA256 string               `json:"state_sha256"`
	Works       []DeviceRecoveryWork `json:"works"`
}

type DeviceRecoveryWork struct {
	Project    string                   `json:"project"`
	WorkSHA256 string                   `json:"work_sha256"`
	Entries    map[string]RecoveryEntry `json:"entries"`
}

type RecoveryEntry struct {
	Identity  string `json:"identity"`
	Mode      uint32 `json:"mode"`
	Directory bool   `json:"directory"`
	SHA256    string `json:"sha256,omitempty"`
}

type DeviceRecoveryResult struct {
	BeforeSHA256 string   `json:"before_sha256"`
	AfterSHA256  string   `json:"after_sha256"`
	Works        []string `json:"works"`
}

// DecodeDeviceRecoveryRequest requires the canonical struct/map encoding. This
// rejects duplicate, unknown or case-aliased identity keys at every depth.
func DecodeDeviceRecoveryRequest(raw []byte) (DeviceRecoveryRequest, error) {
	var request DeviceRecoveryRequest
	if len(raw) > 2<<20 || json.Unmarshal(raw, &request) != nil {
		return request, ErrInvalid
	}
	encoded, err := json.Marshal(request)
	var compact bytes.Buffer
	if err != nil || json.Compact(&compact, raw) != nil || !bytes.Equal(encoded, compact.Bytes()) {
		return request, ErrInvalid
	}
	return request, nil
}

// RecoverDeviceChange is an offline maintenance operation. The operator must
// exclude gateway/model/render writers and retain a verified backup first.
func RecoverDeviceChange(ctx context.Context, path, snapshotDir string, request DeviceRecoveryRequest) (DeviceRecoveryResult, error) {
	var result DeviceRecoveryResult
	if !validHash(request.StateSHA256) || len(request.Works) < 1 || len(request.Works) > 2 {
		return result, ErrInvalid
	}
	info, err := databaseFile(path)
	if err != nil {
		return result, err
	}
	// Open() also recovers submitted deliveries. This operation must not do so.
	db, err := sql.Open("sqlite", existingURL(path))
	if err != nil {
		return result, ErrUnavailable
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	if _, err = db.ExecContext(ctx, "PRAGMA busy_timeout=50; PRAGMA synchronous=FULL;"); err != nil {
		return result, ErrUnavailable
	}
	s := &Store{db: db, path: path, file: info, mu: make(chan struct{}, 1)}
	snapshots, err := protectedRoot(snapshotDir)
	if err != nil {
		return result, err
	}
	defer func() { _ = snapshots.Close() }()
	si, err := snapshots.Stat(".")
	if err != nil || si.Mode().Perm()&0077 != 0 {
		return result, ErrUnavailable
	}
	err = s.changeExpected(ctx, request.StateSHA256, func(st *state) error {
		var err error
		result, err = recoverWorkIdentities(ctx, st, request, snapshots)
		return err
	})
	if err != nil {
		// The caller must inspect a failed/uncertain commit; never replay blindly.
		return DeviceRecoveryResult{}, err
	}
	return result, nil
}

func recoverWorkIdentities(ctx context.Context, st *state, request DeviceRecoveryRequest, snapshots *os.Root) (DeviceRecoveryResult, error) {
	var result DeviceRecoveryResult
	before, err := json.Marshal(st)
	if err != nil || hash(before) != request.StateSHA256 {
		return result, ErrVersion
	}
	selected := map[string]*work{}
	for _, proof := range request.Works {
		if !validHash(proof.WorkSHA256) || proof.Project == "" || selected[proof.WorkSHA256] != nil {
			return result, ErrInvalid
		}
		for _, w := range st.Works {
			if hash([]byte(w.ID)) == proof.WorkSHA256 && w.Principal.Project == proof.Project {
				selected[proof.WorkSHA256] = w
			}
		}
		if selected[proof.WorkSHA256] == nil {
			return result, ErrScope
		}
	}
	// Recheck the complete frozen trees immediately before changing any pins.
	// Filesystem exclusion is still an operator precondition, not a SQLite lock.
	for pass := 0; pass < 2; pass++ {
		for _, proof := range request.Works {
			if err := verifyRecoveryWork(ctx, selected[proof.WorkSHA256], proof, snapshots); err != nil {
				return result, err
			}
		}
	}
	for _, proof := range request.Works {
		w := selected[proof.WorkSHA256]
		w.RootIdentity = proof.Entries["."].Identity
		w.InputIdentity = proof.Entries["inputs"].Identity
		w.OutputIdentity = proof.Entries["outputs"].Identity
		result.Works = append(result.Works, proof.WorkSHA256)
	}
	after, err := json.Marshal(st)
	if err != nil || !validState(*st) {
		return result, ErrInvalid
	}
	result.BeforeSHA256, result.AfterSHA256 = hash(before), hash(after)
	return result, nil
}

func verifyRecoveryWork(ctx context.Context, w *work, proof DeviceRecoveryWork, snapshots *os.Root) error {
	if len(proof.Entries) < 3 || len(proof.Entries) > 1024 || len(w.PendingInputs) != 0 {
		return ErrInvalid
	}
	oldDev, newDev := "", ""
	for name, saved := range map[string]string{".": w.RootIdentity, "inputs": w.InputIdentity, "outputs": w.OutputIdentity} {
		current := proof.Entries[name]
		old, next := strings.Split(saved, ":"), strings.Split(current.Identity, ":")
		if !current.Directory || len(old) != 4 || len(next) != 4 || old[0] == next[0] || !reflect.DeepEqual(old[1:], next[1:]) {
			return ErrScope
		}
		if oldDev != "" && (oldDev != old[0] || newDev != next[0]) {
			return ErrScope
		}
		oldDev, newDev = old[0], next[0]
	}
	r, err := protectedRoot(w.Root)
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()
	remaining := int64(200 << 20)
	entries := map[string]RecoveryEntry{}
	if err := recoveryTree(ctx, r, ".", w.OwnerUID, newDev, &remaining, entries, 0); err != nil {
		return err
	}
	if !reflect.DeepEqual(entries, proof.Entries) {
		return ErrScope
	}
	if len(entries)+len(w.Inputs)+len(w.Deliveries) > 1024 {
		return ErrInvalid
	}
	for _, input := range w.Inputs {
		name := filepath.Base(input.Path)
		if entries["inputs/"+name].SHA256 != input.SHA256 {
			return ErrInvalid
		}
		digest, err := recoveryFile(snapshots, "input_"+name, os.Geteuid(), &remaining)
		if err != nil || digest != input.SHA256 {
			return ErrInvalid
		}
	}
	latest := map[string]*delivery{}
	for _, d := range w.Deliveries {
		if d.Status != "accepted" || d.MessageReceipt == "" {
			return ErrUncertain
		}
		digest, err := recoveryFile(snapshots, d.Snapshot, os.Geteuid(), &remaining)
		if err != nil || digest != d.SHA256 {
			return ErrInvalid
		}
		if prior := latest[d.Path]; prior == nil || prior.Version < d.Version {
			latest[d.Path] = d
		}
	}
	for path, d := range latest {
		if entries["outputs/"+path].SHA256 != d.SHA256 {
			return ErrInvalid
		}
	}
	return ctx.Err()
}

func recoveryTree(ctx context.Context, r *os.Root, path string, owner int, dev string, remaining *int64, entries map[string]RecoveryEntry, depth int) error {
	if depth > 12 || len(entries) >= 1024 {
		return ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := r.Stat(".")
	if err != nil {
		return ErrUnavailable
	}
	entries[path] = RecoveryEntry{Identity: identity(info), Mode: recoveryMode(info), Directory: true}
	if !strings.HasPrefix(identity(info), dev+":") {
		return ErrScope
	}
	dir, err := r.Open(".")
	if err != nil {
		return ErrUnavailable
	}
	names, err := dir.ReadDir(-1)
	_ = dir.Close()
	if err != nil {
		return ErrUnavailable
	}
	for _, name := range names {
		if len(entries) >= 1024 || (path == "." && name.Name() != "inputs" && name.Name() != "outputs") {
			return ErrInvalid
		}
		relative := filepath.ToSlash(filepath.Join(path, name.Name()))
		input := relative == "inputs" || strings.HasPrefix(relative, "inputs/")
		uid := owner
		if input {
			uid = os.Geteuid()
		}
		before, err := r.Lstat(name.Name())
		if err != nil || before.Mode()&os.ModeSymlink != 0 || !strings.HasPrefix(identity(before), dev+":") {
			return ErrScope
		}
		if before.IsDir() {
			child, _, err := childRoot(r, name.Name(), uid, !input)
			if err != nil {
				return err
			}
			err = recoveryTree(ctx, child, relative, owner, dev, remaining, entries, depth+1)
			_ = child.Close()
			if err != nil {
				return err
			}
		} else {
			digest, err := recoveryFile(r, name.Name(), uid, remaining)
			if err != nil {
				return err
			}
			entries[relative] = RecoveryEntry{Identity: identity(before), Mode: recoveryMode(before), SHA256: digest}
		}
		after, err := r.Lstat(name.Name())
		if err != nil || !unchanged(before, after) {
			return ErrScope
		}
	}
	after, err := r.Stat(".")
	if err != nil || !unchanged(info, after) {
		return ErrScope
	}
	return nil
}

func recoveryFile(root *os.Root, name string, uid int, remaining *int64) (string, error) {
	if filepath.Base(name) != name || name == "." || name == ".." {
		return "", ErrInvalid
	}
	before, err := root.Lstat(name)
	if err != nil || !before.Mode().IsRegular() || before.Mode().Perm()&0022 != 0 || checkOwner(before, uid, true) != nil || before.Size() > MaxFileBytes || before.Size() > *remaining {
		return "", ErrInvalid
	}
	f, err := root.OpenFile(name, os.O_RDONLY|noFollowFlag(), 0)
	if err != nil {
		return "", ErrUnavailable
	}
	defer func() { _ = f.Close() }()
	opened, err := f.Stat()
	if err != nil || !unchanged(before, opened) {
		return "", ErrScope
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxFileBytes+1))
	after, statErr := f.Stat()
	current, pathErr := root.Lstat(name)
	if err != nil || statErr != nil || pathErr != nil || !unchanged(opened, after) || !unchanged(after, current) || int64(len(data)) != before.Size() {
		return "", ErrScope
	}
	*remaining -= int64(len(data))
	return hash(data), nil
}
