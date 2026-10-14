package core

import (
	"path/filepath"
	"testing"
	"time"
)

func TestCUJOutOfOrderSelectedAttachmentsArePreservedAndDrainedOnce(t *testing.T) {
	p := &filePlatformStub{stubPlatformEngine: stubPlatformEngine{n: "fixture"}}
	a := &fileWorkCUJAgent{}
	a.nextSessionEvents = []Event{} // Keep the first model turn open while uploads arrive.
	e := NewEngine("fixture-project", a, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangEnglish)
	t.Cleanup(func() { _ = e.Stop() })
	h := &selectedInputHost{}
	h.find = func(principal ActionPrincipal, id string) (FileWorkRef, error) {
		for _, b := range h.bindings {
			if b.Principal == principal && b.Principal.MessageID == id {
				return FileWorkRef{WorkID: "fixture-work", SessionID: b.SessionID}, nil
			}
		}
		return FileWorkRef{}, ErrFileWorkNotFound
	}
	root := t.TempDir()
	if err := e.SetFileWorkHost(h, func(id string) (string, int, error) { return filepath.Join(root, id), 0, nil }); err != nil {
		t.Fatal(err)
	}
	env := &cujEnv{t: t}
	upload := func(id string, at int64) {
		msg := &Message{Platform: "fixture", SessionKey: "fixture:chat:user", UserID: "user", ChannelID: "chat", MessageID: id, ControlledFileWork: true, FileWorkPrivate: true, UserMessageTimeMs: at,
			Files: []FileAttachment{{FileName: id + ".pdf", Data: []byte("fixture"), RequireSave: true}}}
		e.ReceiveMessage(p, msg)
	}
	upload("first", 2000)
	var model *cujAgentSession
	env.waitFor("first upload reaches model", 3*time.Second, func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		if len(a.sessions) == 0 {
			return false
		}
		model = a.sessions[0]
		return len(model.getSentPrompts()) == 1
	})
	upload("newer", 3073)
	upload("late-scan", 3000)
	upload("late-scan", 3000) // Same transport message must still be deduplicated.
	if len(h.bindings) != 3 {
		t.Fatalf("lost or duplicated attachment: bindings=%d, want 3", len(h.bindings))
	}
	for i, name := range []string{"first", "newer", "late-scan"} {
		if h.bindings[i].Principal.MessageID != name || len(h.bindings[i].Inputs) != 1 || h.bindings[i].Inputs[0].FileName != name+".pdf" {
			t.Fatalf("wrong source: %+v", h.bindings[i])
		}
	}
	// An unrelated old command remains stale even in a controlled file conversation.
	e.ReceiveMessage(p, &Message{Platform: "fixture", SessionKey: "fixture:chat:user", UserID: "user", ChannelID: "chat", MessageID: "old-text", Content: "Change the budget", ControlledFileWork: true, FileWorkPrivate: true, UserMessageTimeMs: 2500})
	if len(h.bindings) != 3 {
		t.Fatal("stale text was allowed through")
	}
	e.ReceiveMessage(p, &Message{Platform: "fixture", SessionKey: "fixture:chat:user", UserID: "user", ChannelID: "chat", MessageID: "old-mixed", Content: "Change the budget", ControlledFileWork: true, FileWorkPrivate: true, UserMessageTimeMs: 2500,
		Files: []FileAttachment{{FileName: "old.pdf", Data: []byte("fixture"), RequireSave: true}}})
	if len(h.bindings) != 3 {
		t.Fatal("attachment allowed an old business instruction through")
	}
	model.events <- Event{Type: EventResult, Content: "first done", Done: true}
	session := e.sessions.GetOrCreateActive("fixture:chat:user")
	env.waitFor("all selected attachments drain", 3*time.Second, func() bool { return !session.Busy() && len(model.getSentPrompts()) == 3 })
	for _, turn := range session.fileTurns() {
		if turn.Status != "completed" {
			t.Fatalf("unfinished attachment: %+v", turn)
		}
	}
	if len(session.fileTurns()) != 2 {
		t.Fatal("durable supplement history lost an attachment")
	}
}
