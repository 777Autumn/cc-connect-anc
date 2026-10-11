package teambrain

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

func TestKnowledgeTwoToFiveMembersProtectedEntrances(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("production host ownership checks require POSIX")
	}
	for count := 2; count <= 12; count++ {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			projects := []string{}
			attached := map[string]bool{}
			for i := 0; i < count; i++ {
				name := fmt.Sprintf("fictional-%d", i)
				projects = append(projects, name)
				attached[name] = true
			}
			t.Setenv("CC_TEAM_BRAIN_COMMAND", "/usr/bin/true")
			t.Setenv("CC_TEAM_BRAIN_PROJECTS", strings.Join(projects, ","))
			a, err := NewFromEnv()
			valid := count >= 4 && count <= 10 && count%2 == 0
			if valid {
				if err != nil || !a.AllAttached(attached) || a.Enabled("unregistered") {
					t.Fatalf("fixed entrances: %v %v", a, err)
				}
				delete(attached, projects[0])
				if a.AllAttached(attached) {
					t.Fatal("missing protected entrance accepted")
				}
			} else if err == nil {
				t.Fatal("unsupported entrance count accepted")
			}
			if os.Getenv("CC_TEAM_BRAIN_COMMAND") != "" || os.Getenv("CC_TEAM_BRAIN_PROJECTS") != "" {
				t.Fatal("host configuration retained in inherited environment")
			}
		})
	}
	for _, names := range []string{"a,b,c,a", "a,b,c, d", "a,b,c,"} {
		t.Setenv("CC_TEAM_BRAIN_COMMAND", "/usr/bin/true")
		t.Setenv("CC_TEAM_BRAIN_PROJECTS", names)
		if _, err := NewFromEnv(); err == nil {
			t.Fatal("invalid identity mapping accepted")
		}
	}
}

func TestKnowledgeAttachedMatchesExactConfiguredSet(t *testing.T) {
	a := &Adapter{projects: map[string]bool{"one": true, "two": true, "three": true, "four": true}}
	if !a.AllAttached(map[string]bool{"one": true, "two": true, "three": true, "four": true}) {
		t.Fatal("complete mapping rejected")
	}
	for _, attached := range []map[string]bool{
		{"one": true, "two": true, "three": true},
		{"one": true, "two": true, "three": true, "unknown": true},
		{"one": true, "two": true, "three": true, "four": false},
		{"one": true, "two": true, "three": true, "four": true, "unknown": true},
	} {
		if a.AllAttached(attached) {
			t.Fatal("incomplete or substituted mapping accepted")
		}
	}
}

func TestDisabledHostAndSessionEnvironmentExposeNoPrivateConfiguration(t *testing.T) {
	t.Setenv("CC_TEAM_BRAIN_COMMAND", "")
	t.Setenv("CC_TEAM_BRAIN_PROJECTS", "")
	a, err := NewFromEnv()
	if err != nil || a != nil {
		t.Fatalf("disabled host = %v %v", a, err)
	}
	a = &Adapter{command: "private-host-path", projects: map[string]bool{"project": true}}
	env, err := a.SessionEnv("session")
	if err != nil || len(env) != 1 || env[0] != "TEAM_BRAIN_SESSION_TOKEN=session" {
		t.Fatal("private config in model env")
	}
	for _, command := range []string{"approve", "execute", "raw-api", "pending", "knowledge_pending"} {
		result, card, err := a.Tool(context.Background(), command, json.RawMessage(`{}`), core.ActionPrincipal{}, "session", core.LangEnglish)
		if err != nil || card != nil || result["status"] != "blocked" {
			t.Fatal("model obtained privileged operation")
		}
	}
}

func TestSupervisorRestartPreservesKnowledgeConfigurationWithoutDuplicateKeys(t *testing.T) {
	a := &Adapter{command: "/protected/host", projects: map[string]bool{"second": true, "first": true}}
	env := a.RestartEnv([]string{"PATH=/bin", "CC_TEAM_BRAIN_COMMAND=wrong", "CC_TEAM_BRAIN_PROJECTS=wrong"})
	if len(env) != 3 || env[0] != "PATH=/bin" || env[1] != "CC_TEAM_BRAIN_COMMAND=/protected/host" || env[2] != "CC_TEAM_BRAIN_PROJECTS=first,second" {
		t.Fatalf("invalid supervisor environment: %v", env)
	}
	model, err := a.SessionEnv("session")
	if err != nil || len(model) != 1 || strings.Contains(model[0], "protected") {
		t.Fatal("restart config leaked to model")
	}
}

func TestPresentationHasCompletePreviewAndTrustedButtonsInFiveLanguages(t *testing.T) {
	for _, lang := range []core.Language{core.LangEnglish, core.LangChinese, core.LangTraditionalChinese, core.LangJapanese, core.LangSpanish} {
		result := present(map[string]any{"status": "pending", "approval_id": "bound-id", "preview": "Title: page one\nTarget: https://synthetic.feishu.cn/wiki/page1\nPrior: Old context\n- Old\n+ New ``` untrusted"}, lang)
		if !strings.Contains(result.Card.RenderText(), "Old") || !strings.Contains(result.Card.RenderText(), "New") {
			t.Fatal("preview lost changes")
		}
		if !strings.Contains(result.Card.RenderText(), "page1") || !strings.Contains(result.Card.RenderText(), "Old context") {
			t.Fatal("lost destination or prior context")
		}
		count := 0
		for _, element := range result.Card.Elements {
			if actions, ok := element.(core.CardActions); ok {
				for _, button := range actions.Buttons {
					count++
					if button.Extra["kind"] != Kind || button.Extra["approval_id"] != "bound-id" || button.Text == "" {
						t.Fatal("unbound button")
					}
				}
			}
		}
		if count != 3 {
			t.Fatal("missing approval choices")
		}
	}
	unknown := present(map[string]any{"status": "unknown"}, core.LangChinese)
	if unknown.Code != "execution_result_unknown" || unknown.Card.HasButtons() {
		t.Fatal("unknown write allowed replay")
	}
}

func TestKnowledgeReviewWrapsLiteralSourceAndDistinguishesTerminalStates(t *testing.T) {
	const source = "**客户** <at id=all> [同意](fake) ```\n完整中文正文🧪"
	for _, operation := range []string{"create", "append", "replace"} {
		data := map[string]any{"status": "pending", "approval_id": "fixture", "preview": "legacy", "review": map[string]any{
			"operation": operation, "title": "虚构页面", "destination": "https://synthetic.feishu.cn/wiki/node", "before": "旧正文", "after": source}}
		card := present(data, core.LangChinese).Card
		found := false
		for _, element := range card.Elements {
			if body, ok := element.(core.CardPlainText); ok && body.Content == source {
				found = true
			}
			if body, ok := element.(core.CardMarkdown); ok && strings.Contains(body.Content, "<at") {
				t.Fatal("source parsed as markup")
			}
		}
		if !found || card.MaxBytes != 24000 || strings.Contains(card.RenderText(), "legacy") {
			t.Fatal("incomplete structured review")
		}
		if operation != "create" && !strings.Contains(card.RenderText(), "旧正文") {
			t.Fatal("before omitted")
		}
	}
	for status, expected := range map[string]string{
		"cancelled": "已取消，不会发布", "needs_revision": "请修改草稿", "expired": "已过期",
		"conflict": "页面内容已变化", "unknown": "可能已部分或全部完成", "unrecognized": "可能已部分或全部完成",
	} {
		card := present(map[string]any{"status": status}, core.LangChinese).Card
		if card.HasButtons() || !strings.Contains(card.RenderText(), expected) {
			t.Fatalf("incorrect %s card", status)
		}
		if status == "unknown" && strings.Contains(card.RenderText(), "未发布") {
			t.Fatal("uncertain result claimed not published")
		}
	}
	card := present(map[string]any{"status": "verified", "title": "虚构页面", "receipt": map[string]any{
		"url": "https://synthetic.feishu.cn/wiki/node", "summary": strings.Repeat("巨大差异", 1000)}}, core.LangChinese).Card
	if card.HasButtons() || strings.Contains(card.RenderText(), "巨大差异") || !strings.Contains(card.RenderText(), "虚构页面") {
		t.Fatal("success repeats diff or loses summary")
	}
	legacy := present(map[string]any{"status": "pending", "approval_id": "old", "preview": source}, core.LangChinese).Card
	if !strings.Contains(legacy.RenderText(), source) {
		t.Fatal("legacy preview mutated")
	}
}

func TestSubprocessReceivesAuthenticatedPrincipalAndNoInheritedSecrets(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("production adapter is POSIX only")
	}
	directory := t.TempDir()
	capture := filepath.Join(directory, "request.json")
	script := filepath.Join(directory, "host")
	body := "#!/bin/sh\nif [ \"$PWD\" != '" + strings.ReplaceAll(directory, "'", "'\"'\"'") + "' ]; then exit 2; fi\ncat > '" + capture + "'\nif [ -n \"$SYNTHETIC_SECRET\" ]; then exit 1; fi\nprintf '%s' '{\"status\":\"ok\"}'\n"
	if err := os.WriteFile(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SYNTHETIC_SECRET", "must-not-forward")
	a := &Adapter{command: script, projects: map[string]bool{"project": true}}
	principal := core.ActionPrincipal{UserID: "member", ChatID: "chat", SessionKey: "session", Project: "project", Platform: "test"}
	result, _, err := a.Tool(context.Background(), "knowledge_search", json.RawMessage(`{"query":"synthetic","subject":"team"}`), principal, "scoped-token", core.LangEnglish)
	if err != nil || result["status"] != "ok" {
		t.Fatalf("host call %v %v", result, err)
	}
	raw, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	var request map[string]any
	if json.Unmarshal(raw, &request) != nil {
		t.Fatal("invalid envelope")
	}
	actor := request["principal"].(map[string]any)
	if actor["user_id"] != "member" || actor["project"] != "project" || request["operation"] != "tool" || request["session_token"] != "scoped-token" {
		t.Fatal("lost trusted context")
	}
}

func TestPendingActionsHostEnvelopeAndFailClosedResults(t *testing.T) {
	// Build a tiny native host so the private subprocess contract also runs on Windows.
	directory := t.TempDir()
	source := filepath.Join(directory, "host.go")
	host := filepath.Join(directory, "host")
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("locate Go fixture compiler: %v", err)
	}
	if runtime.GOOS == "windows" {
		host += ".exe"
	}
	program := `package main
import ("io"; "os")
func main() {
 if os.Getenv("SYNTHETIC_SECRET") != "" { os.Exit(2) }
 request, err := io.ReadAll(os.Stdin); if err != nil { os.Exit(3) }
 if os.WriteFile("request.json", request, 0600) != nil { os.Exit(4) }
 response, err := os.ReadFile("response.json"); if err != nil { os.Exit(5) }
 os.Stdout.Write(response)
}`
	if err := os.WriteFile(source, []byte(program), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(goTool, "build", "-o", host, source).CombinedOutput(); err != nil {
		t.Fatalf("build fixture host: %v: %s", err, out)
	}
	t.Setenv("SYNTHETIC_SECRET", "must-not-forward")
	a := &Adapter{command: host, projects: map[string]bool{"project": true}}
	principal := core.ActionPrincipal{Platform: "feishu", UserID: "fixture-member", ChatID: "fixture-chat",
		SessionKey: "physical-session", Project: "project", MessageID: "switch-card"}
	for _, tc := range []struct {
		name, response  string
		pending, failed bool
	}{
		{"pending", `{"pending":true}`, true, false},
		{"clear", `{"pending":false}`, false, false},
		{"blocked", `{"status":"blocked","code":"principal_mismatch"}`, false, true},
		{"missing", `{}`, false, true},
		{"null", `{"pending":null}`, false, true},
		{"string", `{"pending":"false"}`, false, true},
		{"extra", `{"pending":false,"approval_id":"must-not-surface"}`, false, true},
		{"invalid", `not-json`, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(filepath.Join(directory, "response.json"), []byte(tc.response), 0600); err != nil {
				t.Fatal(err)
			}
			pending, err := a.PendingActions(context.Background(), principal)
			if pending != tc.pending || (err != nil) != tc.failed {
				t.Fatalf("pending=%t err=%v", pending, err)
			}
			if err != nil && strings.Contains(err.Error(), "must-not-surface") {
				t.Fatal("host details leaked")
			}
		})
	}
	raw, err := os.ReadFile(filepath.Join(directory, "request.json"))
	if err != nil {
		t.Fatal(err)
	}
	var request map[string]json.RawMessage
	if json.Unmarshal(raw, &request) != nil || len(request) != 2 || string(request["operation"]) != `"pending"` {
		t.Fatal("pending request exposed a model token or omitted the operation")
	}
	var got core.ActionPrincipal
	if json.Unmarshal(request["principal"], &got) != nil || got != principal {
		t.Fatal("pending query lost the authenticated physical principal")
	}
	principal.Project = "not-configured"
	if pending, err := a.PendingActions(context.Background(), principal); pending || err == nil {
		t.Fatal("disabled project passed pending query")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	principal.Project = "project"
	if pending, err := a.PendingActions(ctx, principal); pending || err == nil {
		t.Fatal("cancelled pending query passed")
	}
}
