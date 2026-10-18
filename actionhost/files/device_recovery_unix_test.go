//go:build linux || darwin

package files

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

// Only the disposable fixture's persisted device number changes. Its actual
// directories, principals, session, contents and receipts stay in place.
func changeFixtureDevice(t *testing.T, f fixture) {
	t.Helper()
	if err := f.store.change(context.Background(), func(st *state) error {
		w := st.Works[f.context.WorkID]
		for _, field := range []*string{&w.RootIdentity, &w.InputIdentity, &w.OutputIdentity} {
			parts := strings.Split(*field, ":")
			if len(parts) != 4 {
				t.Fatal("fixture requires a POSIX device identity")
			}
			dev, err := strconv.ParseInt(parts[0], 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			parts[0] = strconv.FormatInt(dev+3, 10)
			*field = strings.Join(parts, ":")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func fixtureRecoveryRequest(t *testing.T, f fixture) DeviceRecoveryRequest {
	t.Helper()
	var payload []byte
	if err := f.store.db.QueryRow("SELECT payload FROM file_state WHERE id=1").Scan(&payload); err != nil {
		t.Fatal(err)
	}
	entries := map[string]RecoveryEntry{}
	if err := filepath.WalkDir(f.context.WorkRoot, func(path string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(f.context.WorkRoot, path)
		if err != nil {
			return err
		}
		entry := RecoveryEntry{Identity: identity(info), Mode: uint32(info.Sys().(*syscall.Stat_t).Mode) & 07777, Directory: info.IsDir()}
		if !info.IsDir() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			entry.SHA256 = hash(data)
		}
		entries[filepath.ToSlash(relative)] = entry
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return DeviceRecoveryRequest{StateSHA256: hash(payload), Works: []DeviceRecoveryWork{{Project: f.binding.Principal.Project, WorkSHA256: hash([]byte(f.context.WorkID)), Entries: entries}}}
}

func TestRecoverDeviceChangeContinuesOriginalWorkWithoutRedelivery(t *testing.T) {
	sends := 0
	f := newFixture(t, func(context.Context, json.RawMessage, core.FileAttachment, string) (string, error) {
		sends++
		return "accepted-before-restart", nil
	})
	b := f.binding
	b.Inputs = []core.FileAttachment{{FileName: "original.docx", Data: packageBytes(t, documentParts("docx"))}}
	b.Principal.MessageID = "input-before-restart"
	if _, err := f.host.Bind(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	result, err := deliver(t, f, "result.docx", output(t, f, "docx"), 0)
	if err != nil || result["status"] != "accepted" {
		t.Fatal(result, err)
	}
	prior, err := f.host.Bind(context.Background(), f.binding)
	if err != nil {
		t.Fatal(err)
	}
	changeFixtureDevice(t, f)
	if _, err := f.host.Bind(context.Background(), f.binding); !errors.Is(err, ErrScope) {
		t.Fatal("fixture did not reproduce original-work refusal", err)
	}
	request := fixtureRecoveryRequest(t, f)
	var beforePayload []byte
	if err := f.store.db.QueryRow("SELECT payload FROM file_state WHERE id=1").Scan(&beforePayload); err != nil {
		t.Fatal(err)
	}
	recovered, err := RecoverDeviceChange(context.Background(), f.database, f.snapshots, request)
	if err != nil {
		t.Fatal("verified original work could not recover", err)
	}
	if recovered.BeforeSHA256 != request.StateSHA256 || recovered.AfterSHA256 == recovered.BeforeSHA256 {
		t.Fatal("missing recovery state receipt", recovered)
	}
	var afterPayload []byte
	if err := f.store.db.QueryRow("SELECT payload FROM file_state WHERE id=1").Scan(&afterPayload); err != nil {
		t.Fatal(err)
	}
	var beforeState, afterState state
	if json.Unmarshal(beforePayload, &beforeState) != nil || json.Unmarshal(afterPayload, &afterState) != nil {
		t.Fatal("invalid recovery state")
	}
	old, current := beforeState.Works[f.context.WorkID], afterState.Works[f.context.WorkID]
	current.RootIdentity, current.InputIdentity, current.OutputIdentity = old.RootIdentity, old.InputIdentity, old.OutputIdentity
	if !reflect.DeepEqual(beforeState, afterState) {
		t.Fatal("recovery changed fields other than the three directory identities")
	}
	after, err := f.host.Bind(context.Background(), f.binding)
	if err != nil || !reflect.DeepEqual(prior, after) {
		t.Fatal("original work, inputs or deliveries changed", prior, after, err)
	}
	if sends != 1 {
		t.Fatal("recovery repeated delivery", sends)
	}
	if _, err := RecoverDeviceChange(context.Background(), f.database, f.snapshots, request); err == nil {
		t.Fatal("stale manifest replay accepted")
	}
}

func TestRecoverDeviceChangeRejectsUnprovenOrChangedState(t *testing.T) {
	cases := map[string]func(*testing.T, fixture, *DeviceRecoveryRequest){
		"wrong_principal": func(_ *testing.T, _ fixture, r *DeviceRecoveryRequest) { r.Works[0].Project = "other" },
		"wrong_work": func(_ *testing.T, _ fixture, r *DeviceRecoveryRequest) {
			r.Works[0].WorkSHA256 = strings.Repeat("0", 64)
		},
		"stale_state":       func(_ *testing.T, _ fixture, r *DeviceRecoveryRequest) { r.StateSHA256 = strings.Repeat("0", 64) },
		"missing_inventory": func(_ *testing.T, _ fixture, r *DeviceRecoveryRequest) { delete(r.Works[0].Entries, "outputs") },
		"new_file_after_freeze": func(t *testing.T, f fixture, _ *DeviceRecoveryRequest) {
			if err := os.WriteFile(filepath.Join(f.context.OutputDir, "unfrozen.txt"), []byte("unfrozen"), 0600); err != nil {
				t.Fatal(err)
			}
		},
		"replaced_directory_even_with_fresh_inventory": func(t *testing.T, f fixture, r *DeviceRecoveryRequest) {
			path := filepath.Join(f.context.WorkRoot, "inputs")
			if err := os.Rename(path, path+"-old"); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(path, 0700); err != nil {
				t.Fatal(err)
			}
			*r = fixtureRecoveryRequest(t, f)
		},
		"tampered_input_even_with_fresh_inventory": func(t *testing.T, f fixture, r *DeviceRecoveryRequest) {
			entries, err := os.ReadDir(filepath.Join(f.context.WorkRoot, "inputs"))
			if err != nil || len(entries) != 1 {
				t.Fatal(entries, err)
			}
			path := filepath.Join(f.context.WorkRoot, "inputs", entries[0].Name())
			if err := os.Chmod(path, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("tampered"), 0440); err != nil {
				t.Fatal(err)
			}
			*r = fixtureRecoveryRequest(t, f)
		},
		"tampered_snapshot": func(t *testing.T, f fixture, _ *DeviceRecoveryRequest) {
			entries, err := os.ReadDir(f.snapshots)
			if err != nil || len(entries) != 1 {
				t.Fatal(entries, err)
			}
			path := filepath.Join(f.snapshots, entries[0].Name())
			if err := os.Chmod(path, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("tampered"), 0400); err != nil {
				t.Fatal(err)
			}
		},
		"symlink_even_with_fresh_inventory": func(t *testing.T, f fixture, r *DeviceRecoveryRequest) {
			path := filepath.Join(f.context.OutputDir, "foreign.txt")
			if err := os.Symlink(filepath.Join(f.context.WorkRoot, "inputs"), path); err != nil {
				t.Fatal(err)
			}
			// The operator cannot attest a symlink into an ordinary-file inventory.
			r.Works[0].Entries["outputs/foreign.txt"] = RecoveryEntry{Identity: "invented", SHA256: strings.Repeat("0", 64)}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, nil)
			b := f.binding
			b.Principal.MessageID = "source"
			b.Inputs = []core.FileAttachment{{FileName: "source.docx", Data: packageBytes(t, documentParts("docx"))}}
			if _, err := f.host.Bind(context.Background(), b); err != nil {
				t.Fatal(err)
			}
			changeFixtureDevice(t, f)
			request := fixtureRecoveryRequest(t, f)
			mutate(t, f, &request)
			before, err := os.ReadFile(f.database)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := RecoverDeviceChange(context.Background(), f.database, f.snapshots, request); err == nil {
				t.Fatal("unproven recovery accepted")
			}
			after, err := os.ReadFile(f.database)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("rejected recovery changed ledger", err)
			}
		})
	}
}

func TestRecoverDeviceChangeCancellationPreservesLedger(t *testing.T) {
	f := newFixture(t, nil)
	changeFixtureDevice(t, f)
	request := fixtureRecoveryRequest(t, f)
	before, err := os.ReadFile(f.database)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := RecoverDeviceChange(ctx, f.database, f.snapshots, request); err == nil {
		t.Fatal("cancelled recovery succeeded")
	}
	after, err := os.ReadFile(f.database)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("cancelled recovery changed ledger", err)
	}
}

func TestRecoverDeviceChangeBatchIsAtomic(t *testing.T) {
	for _, reject := range []bool{false, true} {
		t.Run(strconv.FormatBool(reject), func(t *testing.T) {
			f, g := newFixture(t, nil), newFixture(t, nil)
			var payload []byte
			if err := g.store.db.QueryRow("SELECT payload FROM file_state WHERE id=1").Scan(&payload); err != nil {
				t.Fatal(err)
			}
			var snapshot state
			if err := json.Unmarshal(payload, &snapshot); err != nil {
				t.Fatal(err)
			}
			second := snapshot.Works[g.context.WorkID]
			// Distinct roots alone do not create a different logical work in the fixture.
			second.ID += "-second"
			second.Principal.Project = "project-second"
			if err := f.store.change(context.Background(), func(st *state) error { st.Works[second.ID] = second; return nil }); err != nil {
				t.Fatal(err)
			}
			g.store = f.store
			g.context.WorkID = second.ID
			g.binding.Principal.Project = second.Principal.Project
			changeFixtureDevice(t, f)
			changeFixtureDevice(t, g)
			request := fixtureRecoveryRequest(t, f)
			request.Works = append(request.Works, fixtureRecoveryRequest(t, g).Works[0])
			var before []byte
			if err := f.store.db.QueryRow("SELECT payload FROM file_state WHERE id=1").Scan(&before); err != nil {
				t.Fatal(err)
			}
			if reject {
				delete(request.Works[1].Entries, "outputs")
			}
			_, err := RecoverDeviceChange(context.Background(), f.database, f.snapshots, request)
			if (err != nil) != reject {
				t.Fatal("unexpected batch result", err)
			}
			var after []byte
			if err := f.store.db.QueryRow("SELECT payload FROM file_state WHERE id=1").Scan(&after); err != nil {
				t.Fatal(err)
			}
			if reject {
				if !bytes.Equal(before, after) {
					t.Fatal("failed batch partially committed")
				}
				return
			}
			var a, b state
			if json.Unmarshal(before, &a) != nil || json.Unmarshal(after, &b) != nil {
				t.Fatal("invalid state")
			}
			changes := 0
			for id, old := range a.Works {
				current := b.Works[id]
				if old.RootIdentity != current.RootIdentity {
					changes++
				}
				if old.InputIdentity != current.InputIdentity {
					changes++
				}
				if old.OutputIdentity != current.OutputIdentity {
					changes++
				}
				current.RootIdentity, current.InputIdentity, current.OutputIdentity = old.RootIdentity, old.InputIdentity, old.OutputIdentity
			}
			if changes != 6 || !reflect.DeepEqual(a, b) {
				t.Fatal("batch did not change exactly six pins", changes)
			}
		})
	}
}

func TestRecoverDeviceChangeDoesNotNormalizeUncertainDeliveries(t *testing.T) {
	for _, status := range []string{"submitted", "unknown"} {
		t.Run(status, func(t *testing.T) {
			f := newFixture(t, nil)
			if _, err := deliver(t, f, "result.docx", output(t, f, "docx"), 0); err != nil {
				t.Fatal(err)
			}
			if err := f.store.change(context.Background(), func(st *state) error {
				for _, d := range st.Works[f.context.WorkID].Deliveries {
					d.Status = status
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			changeFixtureDevice(t, f)
			request := fixtureRecoveryRequest(t, f)
			before, err := os.ReadFile(f.database)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := RecoverDeviceChange(context.Background(), f.database, f.snapshots, request); !errors.Is(err, ErrUncertain) {
				t.Fatal("uncertain delivery accepted", err)
			}
			after, err := os.ReadFile(f.database)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("uncertain delivery normalized", err)
			}
		})
	}
}

func TestRecoverDeviceChangeRejectsChangedRawPayload(t *testing.T) {
	f := newFixture(t, nil)
	changeFixtureDevice(t, f)
	request := fixtureRecoveryRequest(t, f)
	var payload []byte
	if err := f.store.db.QueryRow("SELECT payload FROM file_state WHERE id=1").Scan(&payload); err != nil {
		t.Fatal(err)
	}
	changed := append([]byte(`{"unrecognized_after_freeze":true,`), payload[1:]...)
	if _, err := f.store.db.Exec("UPDATE file_state SET payload=? WHERE id=1", string(changed)); err != nil {
		t.Fatal(err)
	}
	if _, err := RecoverDeviceChange(context.Background(), f.database, f.snapshots, request); !errors.Is(err, ErrVersion) {
		t.Fatal("changed raw state accepted", err)
	}
	var after []byte
	if err := f.store.db.QueryRow("SELECT payload FROM file_state WHERE id=1").Scan(&after); err != nil || !bytes.Equal(after, changed) {
		t.Fatal("rejected state rewritten", err)
	}
}

func TestBindDeviceChangeRejectsOriginalWorkWithoutMutation(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(strconv.FormatBool(changed), func(t *testing.T) {
			sends := 0
			f := newFixture(t, func(context.Context, json.RawMessage, core.FileAttachment, string) (string, error) {
				sends++
				return "unexpected-send", nil
			})
			if changed {
				changeFixtureDevice(t, f)
			}
			before, err := os.ReadFile(f.database)
			if err != nil {
				t.Fatal(err)
			}
			b := f.binding
			b.Principal.MessageID = "later-plain-text"
			result, err := f.host.Bind(context.Background(), b)
			if changed {
				if !errors.Is(err, ErrScope) || result.WorkID != "" {
					t.Fatal("changed device was not rejected", result, err)
				}
				after, readErr := os.ReadFile(f.database)
				if readErr != nil || !bytes.Equal(before, after) {
					t.Fatal("rejected binding mutated the ledger", readErr)
				}
			} else if err != nil || result.WorkID != f.context.WorkID {
				t.Fatal("unchanged original work did not continue", result, err)
			}
			if sends != 0 {
				t.Fatal("binding sent a message")
			}
		})
	}
}
