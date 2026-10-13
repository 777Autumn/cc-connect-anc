package files

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

func TestRevisionReferenceSurvivesWorkContextAndSchema(t *testing.T) {
	var source core.FileWorkSource
	if err := json.Unmarshal([]byte(`{"publication_id":"bh_chg_fixture_0","purpose":"revision","reference_only":true,"publication_basis_id":"chg_fixture"}`), &source); err != nil {
		t.Fatal(err)
	}
	w := &work{Inputs: []Input{{Source: &source}}}
	raw, err := json.Marshal(workContext(w))
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	_ = json.Unmarshal(raw, &result)
	visible := result["inputs"].([]any)[0].(map[string]any)["source"].(map[string]any)
	if visible["reference_only"] != true || visible["purpose"] != "revision" || visible["publication_basis_id"] != "chg_fixture" {
		t.Fatal("reference-only source erased from work context", string(raw))
	}
	if !validState(state{Schema: 5, Works: map[string]*work{}}) {
		t.Fatal("revision provenance schema unsupported")
	}
}

func TestRevisionReferencePersistsAcrossRestartDuplicateAndQueueActivation(t *testing.T) {
	for _, queued := range []bool{false, true} {
		t.Run(map[bool]string{false: "active", true: "queued"}[queued], func(t *testing.T) {
			f, target, work, grant, _, _ := publicationFixture(t)
			grant.Purpose, grant.ReferenceOnly, grant.PublicationBasisID = "revision", true, "chg_fixture"
			calls, deny := 0, queued
			check := func(_ context.Context, p core.ActionPrincipal, id, pub, purpose string) (core.CasePublishedSource, error) {
				calls++
				if p != target.Principal || id != work.WorkID || pub != "bh_chg_fixture_0" || (deny && calls == 2) {
					return core.CasePublishedSource{}, ErrScope
				}
				result := grant
				// Even a later current grant cannot erase a recorded reference restriction.
				if purpose == "current" {
					result.Purpose, result.ReferenceOnly = "current", false
				}
				return result, nil
			}
			f.host.SetPublicationAccess(check)
			request := json.RawMessage(`{"publication_id":"bh_chg_fixture_0","purpose":"revision"}`)
			ctx := context.Background()
			_, err := f.host.Tool(ctx, "host-case-import", request, target.Principal, work.WorkID)
			if (err != nil) != queued {
				t.Fatal("queue/activation outcome", err)
			}
			if queued {
				entries, err := os.ReadDir(filepath.Join(target.WorkRoot, "inputs"))
				if err != nil || len(entries) != 0 {
					t.Fatal("revoked activation copied bytes", err)
				}
			}
			reopened, err := Open(f.database)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = reopened.Close() }()
			f.host.store, deny = reopened, false
			current, err := f.host.ActivateInputs(ctx, target.Principal, work.WorkID)
			if err != nil || len(current.Inputs) != 1 || !current.Inputs[0].Source.ReferenceOnly || current.Inputs[0].Source.Purpose != "revision" || current.Inputs[0].Source.PublicationBasisID != grant.PublicationBasisID {
				t.Fatal("restart/queue lost revision restriction", current, err)
			}
			for range 2 {
				if _, err := f.host.Tool(ctx, "host-case-import", request, target.Principal, work.WorkID); err != nil {
					t.Fatal("authorized duplicate revision refused", err)
				}
			}
			for _, raw := range []string{`{"publication_id":"bh_chg_fixture_0"}`, `{"publication_id":"bh_chg_fixture_0","purpose":"current"}`, `{"publication_id":"bh_chg_fixture_0","purpose":"forged"}`, `{"publication_id":"bh_chg_fixture_0","purpose":"revision","reference_only":false}`} {
				if _, err := f.host.Tool(ctx, "host-case-import", json.RawMessage(raw), target.Principal, work.WorkID); err == nil {
					t.Fatal("revision restriction cleared", raw)
				}
			}
			if err := reopened.change(ctx, func(st *state) error { st.Schema = 4; return nil }); err == nil {
				t.Fatal("store permitted schema downgrade")
			}
			if err := reopened.change(ctx, func(st *state) error {
				if st.Schema != 5 || !st.Works[work.WorkID].Inputs[0].Source.ReferenceOnly || !validState(*st) {
					t.Fatal("persisted source damaged")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRevisionReferenceActivationRejectsChangedBasisOrReferenceGrant(t *testing.T) {
	for _, field := range []string{"basis", "reference", "purpose"} {
		t.Run(field, func(t *testing.T) {
			f, target, work, grant, _, _ := publicationFixture(t)
			grant.Purpose, grant.ReferenceOnly, grant.PublicationBasisID = "revision", true, "chg_fixture"
			calls := 0
			f.host.SetPublicationAccess(func(context.Context, core.ActionPrincipal, string, string, string) (core.CasePublishedSource, error) {
				calls++
				if calls > 1 {
					switch field {
					case "basis":
						grant.PublicationBasisID = "chg_other"
					case "reference":
						grant.ReferenceOnly = false
					case "purpose":
						grant.Purpose = "current"
					}
				}
				return grant, nil
			})
			if _, err := f.host.Tool(context.Background(), "host-case-import", json.RawMessage(`{"publication_id":"bh_chg_fixture_0","purpose":"revision"}`), target.Principal, work.WorkID); err == nil {
				t.Fatal("changed grant accepted")
			}
			entries, err := os.ReadDir(filepath.Join(target.WorkRoot, "inputs"))
			if err != nil || len(entries) != 0 {
				t.Fatal("changed grant copied bytes", err)
			}
		})
	}
}

func TestRevisionSchemaReadsLegacyPublicationAndUpgradesOnlyAfterAuthorization(t *testing.T) {
	f, target, work, grant, _, _ := publicationFixture(t)
	grant.Purpose, grant.PublicationBasisID = "current", "chg_fixture"
	f.host.SetPublicationAccess(func(context.Context, core.ActionPrincipal, string, string, string) (core.CasePublishedSource, error) {
		return grant, nil
	})
	ctx := context.Background()
	request := json.RawMessage(`{"publication_id":"bh_chg_fixture_0"}`)
	if _, err := f.host.Tool(ctx, "host-case-import", request, target.Principal, work.WorkID); err != nil {
		t.Fatal(err)
	}
	// Reconstruct an old on-disk fixture, not a production downgrade path.
	var raw []byte
	if err := f.store.db.QueryRow("SELECT payload FROM file_state WHERE id=1").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var legacy state
	if err := json.Unmarshal(raw, &legacy); err != nil {
		t.Fatal(err)
	}
	legacy.Schema = 4
	legacy.Works[work.WorkID].Inputs[0].Source.Purpose = ""
	legacy.Works[work.WorkID].Inputs[0].Source.PublicationBasisID = ""
	if !validState(legacy) {
		t.Fatal("valid schema 4 rejected")
	}
	raw, _ = json.Marshal(legacy)
	if _, err := f.store.db.Exec("UPDATE file_state SET payload=? WHERE id=1", string(raw)); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(f.database)
	if err != nil {
		t.Fatal("legacy publication could not reopen", err)
	}
	defer func() { _ = reopened.Close() }()
	f.host.store = reopened
	if _, err := f.host.Tool(ctx, "host-case-import", request, target.Principal, work.WorkID); err != nil {
		t.Fatal("current legacy publication could not reauthorize", err)
	}
	if err := reopened.change(ctx, func(st *state) error {
		source := st.Works[work.WorkID].Inputs[0].Source
		if st.Schema != 5 || source.Purpose != "current" || source.ReferenceOnly || source.PublicationBasisID != grant.PublicationBasisID {
			t.Fatal("authorized legacy upgrade lost provenance")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
