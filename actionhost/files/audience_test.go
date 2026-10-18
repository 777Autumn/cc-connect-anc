package files

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

func TestWorkContextAudienceComesFromTransportAndSurvivesReopen(t *testing.T) {
	for _, group := range []bool{false, true} {
		t.Run(map[bool]string{false: "private", true: "group"}[group], func(t *testing.T) {
			f := newFixture(t, nil)
			b := f.binding
			b.Group = group
			b.SessionID, b.Principal.MessageID = "new-native", "new-message"
			var err error
			b.WorkRoot, err = PrepareWork(t.TempDir(), "new-work", os.Geteuid())
			if err != nil {
				t.Fatal(err)
			}
			b.Route = json.RawMessage(`{"audience":"unrelated route label"}`)
			w, err := f.host.Bind(context.Background(), b)
			if err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(f.database)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := reopened.Close(); err != nil {
					t.Error(err)
				}
			}()
			f.host.store = reopened
			got, err := f.host.Tool(context.Background(), "work-context", json.RawMessage(`{}`), b.Principal, w.WorkID)
			want := map[bool]string{false: "private", true: "group"}[group]
			if err != nil || got["delivery_audience"] != want {
				t.Fatalf("audience=%v want=%s err=%v", got["delivery_audience"], want, err)
			}
			// An independent chat type cannot relabel the same work or its frozen route.
			b.Group = !group
			if _, err := f.host.Bind(context.Background(), b); err == nil {
				t.Fatal("changed audience accepted")
			}
		})
	}
}

func TestCUJ_PrivateDraftAudienceThroughAttachmentQueuedRequestAndContinuation(t *testing.T) {
	p := &fileJourneyPlatform{}
	f := newFixture(t, p.SendFileWithReceipt)
	base := t.TempDir()
	_ = os.Chmod(base, 0700)
	shared := contextOnlyJourney()
	e := conversationEngine(t, f, p, shared, base, filepath.Join(t.TempDir(), "sessions.json"))
	e.ReceiveMessage(p, naturalMessage("scan", "", "Read this attachment", core.FileAttachment{FileName: "brief.docx", Data: packageBytes(t, documentParts("docx"))}))
	awaitFileCondition(t, func() bool { return shared.turns.Load() == 1 })
	e.ReceiveMessage(p, naturalMessage("draft", "", "Prepare a Word draft just for me; no CRM or formal handoff"))
	first := nextFileResult(t, shared)
	second := nextFileResult(t, shared)
	awaitFileIdle(t, e)
	e.ReceiveMessage(p, naturalMessage("continue", "", "Continue the same draft"))
	third := nextFileResult(t, shared)
	awaitFileIdle(t, e)
	for _, w := range []WorkContext{first, second, third} {
		if w.WorkID != first.WorkID || resultMap(w)["delivery_audience"] != "private" {
			t.Fatal("private work context lost across turns", resultMap(w))
		}
	}
}

func TestLegacyAudienceStaysUnknownUntilAuthenticatedRebind(t *testing.T) {
	f := newFixture(t, nil)
	if err := f.store.change(context.Background(), func(st *state) error {
		st.Works[f.context.WorkID].DeliveryAudience = ""
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	got, err := f.host.Tool(context.Background(), "work-context", json.RawMessage(`{}`), f.binding.Principal, f.context.WorkID)
	if err != nil || got["delivery_audience"] != "unknown" {
		t.Fatal(got, err)
	}
	if _, err := f.host.Tool(context.Background(), "work-context", json.RawMessage(`{"delivery_audience":"private"}`), f.binding.Principal, f.context.WorkID); err == nil {
		t.Fatal("model supplied audience accepted")
	}
	wrong := f.binding
	wrong.WorkRoot += "-missing"
	if _, err := f.host.Bind(context.Background(), wrong); err == nil {
		t.Fatal("invalid rebind accepted")
	}
	w, err := f.host.Bind(context.Background(), f.binding)
	if err != nil || resultMap(w)["delivery_audience"] != "private" {
		t.Fatal(w, err)
	}
}
