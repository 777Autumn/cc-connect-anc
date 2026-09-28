package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

type sharedTestRequest struct {
	ID     any            `json:"id"`
	Method string         `json:"method"`
	Params map[string]any `json:"params"`
}

// Real multiplexed JSON-RPC pipes, with only the native CLI boundary faked.
func sharedTestServer(t *testing.T, notifyClosed bool) (*sharedAppServer, <-chan sharedTestRequest, func(any), func()) {
	t.Helper()
	s := makeAppServerSession(context.Background(), "", nil, "stdio://", t.TempDir(), "", "", "full-auto", "", "", "", "", nil, t.TempDir(), "", "", core.SpawnOptions{})
	h := &sharedAppServer{transport: s, threads: map[string]*appServerSession{}, closing: map[string]chan struct{}{}, unavailable: map[string]bool{}, workspaces: map[string]bool{}, blocked: map[string]int{}}
	s.hub = h
	requests, stdin := io.Pipe()
	stdout, replies := io.Pipe()
	s.stdin = stdin
	var writeMu sync.Mutex
	write := func(v any) {
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = json.NewEncoder(replies).Encode(v)
	}
	s.wg.Add(1)
	go s.readLoop(stdout)
	turns := make(chan sharedTestRequest, 8)
	go func() {
		threads := map[string]string{}
		scanner := bufio.NewScanner(requests)
		for scanner.Scan() {
			var r sharedTestRequest
			if json.Unmarshal(scanner.Bytes(), &r) != nil || r.Method == "" {
				continue
			}
			var result any = map[string]any{}
			switch r.Method {
			case "config/read":
				result = map[string]any{"config": map[string]any{"permissions": map[string]any{"isolated": map[string]any{"filesystem": map[string]any{":minimal": "read"}}}}}
			case "thread/start", "thread/resume":
				work := r.Params["cwd"].(string)
				id, _ := r.Params["threadId"].(string)
				if id == "" {
					id = filepath.Base(work)
				}
				threads[id] = work
				result = map[string]any{"thread": map[string]any{"id": id}, "cwd": work, "model": "gpt-6-luna", "reasoningEffort": "medium"}
			case "thread/read":
				id := r.Params["threadId"].(string)
				result = map[string]any{"thread": map[string]any{"id": id, "cwd": threads[id]}}
			case "turn/start":
				turns <- r // Do not answer until both employees' starts arrived.
				continue
			}
			write(map[string]any{"id": r.ID, "result": result})
			if r.Method == "thread/unsubscribe" && notifyClosed {
				write(map[string]any{"method": "thread/closed", "params": map[string]any{"threadId": r.Params["threadId"]}})
			}
		}
	}()
	t.Cleanup(func() { s.abortTransport(); _ = requests.Close(); _ = replies.Close(); _ = stdout.Close(); s.wg.Wait() })
	return h, turns, write, func() { _ = replies.Close() }
}

func sharedTestChild(t *testing.T, h *sharedAppServer, work, token, resume string) *appServerSession {
	t.Helper()
	s := makeAppServerSession(context.Background(), "", nil, "stdio://", work, "gpt-6-luna", "medium", "full-auto", "isolated", resume, "", "", nil, filepath.Join(filepath.Dir(work), "auth"), "", "", core.SpawnOptions{})
	s.threadConfig = sharedThreadConfig(work, "", "isolated", "", []string{"CC_FILE_ACTION_TOKEN=" + token}, core.SpawnOptions{})
	if err := h.attach(s, resume); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.cancel)
	return s
}

func TestCodexSharedConcurrentStopResumeAndQuestionScope(t *testing.T) {
	h, starts, write, _ := sharedTestServer(t, true)
	root := t.TempDir()
	a := sharedTestChild(t, h, filepath.Join(root, "sales"), "fictional-a", "")
	b := sharedTestChild(t, h, filepath.Join(root, "operations"), "fictional-b", "")
	done := make(chan error, 2)
	go func() { done <- a.Send("sales draft", "a", nil, nil) }()
	go func() { done <- b.Send("execution draft", "b", nil, nil) }()
	var pending []sharedTestRequest
	for i := 0; i < 2; i++ {
		var r sharedTestRequest
		select {
		case r = <-starts:
		case <-time.After(time.Second):
			t.Fatal("turns were serialized")
		}
		pending = append(pending, r)
	}
	for _, r := range pending {
		id := r.Params["threadId"].(string)
		write(map[string]any{"method": "turn/started", "params": map[string]any{"threadId": id, "turn": map[string]any{"id": "turn-" + id}}})
		write(map[string]any{"id": r.ID, "result": map[string]any{"turn": map[string]any{"id": "turn-" + id}}})
	}
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	h.serverRequest(serverRequestProbe(t, `"question-a"`, "item/tool/requestUserInput", map[string]any{
		"threadId": "sales", "turnId": "turn-sales", "questions": []any{map[string]any{"id": "choice", "question": "Which package?"}},
	}))
	select {
	case event := <-a.events:
		if event.ToolName != "AskUserQuestion" {
			t.Fatal(event)
		}
	case <-time.After(time.Second):
		t.Fatal("missing question")
	}
	if err := b.RespondPermission(`"question-a"`, core.PermissionResult{Behavior: "allow"}); err == nil {
		t.Fatal("other employee answered question")
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if !b.Alive() || !h.transport.Alive() {
		t.Fatal("stopping sales killed operations")
	}
	a2 := sharedTestChild(t, h, filepath.Join(root, "sales"), "fictional-new-a", "sales")
	if a2.transport != b.transport {
		t.Fatal("resume created another authentication owner")
	}
	write(map[string]any{"method": "item/completed", "params": map[string]any{"threadId": "operations", "item": map[string]any{"type": "agentMessage", "text": "execution ready"}}})
	write(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "operations", "turn": map[string]any{"id": "turn-operations", "status": "completed"}}})
	select {
	case event := <-b.events:
		if event.Content != "execution ready" {
			t.Fatal(event)
		}
	case <-time.After(time.Second):
		t.Fatal("operations did not finish")
	}
	select {
	case event := <-a2.events:
		t.Fatalf("operations leaked to sales: %+v", event)
	default:
	}
	if err := a2.Close(); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCodexSharedUnconfirmedUnloadBlocksFreshEntry(t *testing.T) {
	h, _, _, _ := sharedTestServer(t, false)
	root := t.TempDir()
	a := sharedTestChild(t, h, filepath.Join(root, "a"), "", "")
	if err := a.Close(); err == nil || !strings.Contains(err.Error(), "unconfirmed") {
		t.Fatalf("missing unload confirmation was accepted: %v", err)
	}
	s := makeAppServerSession(context.Background(), "", nil, "stdio://", a.workDir, "", "", "full-auto", "isolated", "", "", "", nil, "", "", "", core.SpawnOptions{})
	defer s.cancel()
	if err := h.attach(s, ""); err == nil || !strings.Contains(err.Error(), "unconfirmed") {
		t.Fatal("fresh start bypassed uncertain unload")
	}
}

func TestCodexSharedEOFAndBlockedQuestionFailAllThreads(t *testing.T) {
	for _, fault := range []string{"stdout-eof", "question-write"} {
		t.Run(fault, func(t *testing.T) {
			h, _, write, closeOutput := sharedTestServer(t, true)
			root := t.TempDir()
			a := sharedTestChild(t, h, filepath.Join(root, "a"), "", "")
			b := sharedTestChild(t, h, filepath.Join(root, "b"), "", "")
			if fault == "stdout-eof" {
				closeOutput()
			} else {
				// The native process stops consuming stdin; the question arrives over
				// the real readLoop. No response path may block that loop indefinitely.
				reader, blocked := io.Pipe()
				t.Cleanup(func() { _ = reader.Close(); _ = blocked.Close() })
				h.transport.procMu.Lock()
				old := h.transport.stdin
				h.transport.stdin = blocked
				h.transport.procMu.Unlock()
				_ = old.Close()
				write(map[string]any{"id": "empty-question", "method": "item/tool/requestUserInput", "params": map[string]any{"threadId": "a", "questions": []any{}}})
			}
			for _, child := range []*appServerSession{a, b} {
				select {
				case event := <-child.events:
					if event.Type != core.EventError {
						t.Fatal(event)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("missing failure")
				}
				if child.Alive() {
					t.Fatal("dead owner left a live child")
				}
				select {
				case <-child.events:
					t.Fatal("duplicate failure")
				default:
				}
			}
			if _, err := h.connection(); err == nil {
				t.Fatal("silently restarted authentication owner")
			}
		})
	}
}

func TestCodexSharedPermissionsAndCapabilities(t *testing.T) {
	root := t.TempDir()
	work := filepath.Join(root, "work")
	home := filepath.Join(root, "auth")
	file := filepath.Join(root, "file")
	base := func(fs map[string]any, extra map[string]any) map[string]any {
		p := map[string]any{"filesystem": fs}
		for k, v := range extra {
			p[k] = v
		}
		return map[string]any{"permissions": map[string]any{"isolated": p}}
	}
	for name, config := range map[string]map[string]any{
		"extends":         base(map[string]any{":minimal": "read"}, map[string]any{"extends": ":workspace"}),
		"read-everything": base(map[string]any{":minimal": "read", root: "read"}, nil),
		"write-tool":      base(map[string]any{":minimal": "read", filepath.Join(root, "tools"): "write"}, nil),
		"auth":            base(map[string]any{":minimal": "read", home: "read"}, nil),
	} {
		t.Run(name, func(t *testing.T) {
			if validateSharedConfig(config, "isolated", []string{work, home}) == nil {
				t.Fatal("unsafe base accepted")
			}
		})
	}
	if err := validateSharedConfig(base(map[string]any{":minimal": "read", filepath.Join(root, "tools"): "read"}, nil), "isolated", []string{work, home}); err != nil {
		t.Fatal(err)
	}
	// Exact shape returned by pinned 0.157.1 config/read (no model invocation).
	if err := validateSharedConfig(base(map[string]any{":minimal": "read", "glob_scan_max_depth": nil}, map[string]any{"description": nil, "extends": nil, "workspace_roots": nil}), "isolated", []string{work, home}); err != nil {
		t.Fatalf("native null defaults rejected: %v", err)
	}
	c := sharedThreadConfig(work, file, "isolated", filepath.Join(root, "office.sock"), []string{"CC_FILE_ACTION_TOKEN=fictional", "CODEX_HOME=private", "OPENAI_API_KEY=private", "HOME=private", "MYANC_CRM_HOST_SECRET=private"}, core.SpawnOptions{})
	b, _ := json.Marshal(c)
	if strings.Contains(string(b), `:"private"`) {
		t.Fatalf("process credential in thread config: %s", b)
	}
	set := c["shell_environment_policy"].(map[string]any)["set"].(map[string]string)
	if set["CC_FILE_ACTION_TOKEN"] != "fictional" || set["HOME"] != "/var/empty" || set["TMPDIR"] != filepath.Join(file, "outputs") {
		t.Fatal(set)
	}
}

func TestCodexSharedFailedTurnNeverReportsSuccess(t *testing.T) {
	h, _, write, _ := sharedTestServer(t, true)
	root := t.TempDir()
	a := sharedTestChild(t, h, filepath.Join(root, "a"), "", "")
	b := sharedTestChild(t, h, filepath.Join(root, "b"), "", "")
	write(map[string]any{"method": "turn/started", "params": map[string]any{"threadId": "a", "turn": map[string]any{"id": "failed-turn"}}})
	write(map[string]any{"method": "error", "params": map[string]any{"threadId": "a", "willRetry": true, "error": map[string]any{"message": "temporary"}}})
	write(map[string]any{"method": "thread/status/changed", "params": map[string]any{"threadId": "a", "status": map[string]any{"type": "idle"}}})
	write(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "a", "turn": map[string]any{"id": "failed-turn", "status": "failed", "error": map[string]any{"message": "synthetic failure"}}}})
	select {
	case event := <-a.events:
		if event.Type != core.EventError {
			t.Fatal(event)
		}
	case <-time.After(time.Second):
		t.Fatal("failed turn disappeared")
	}
	select {
	case event := <-a.events:
		t.Fatalf("unexpected success: %+v", event)
	default:
	}
	select {
	case event := <-b.events:
		t.Fatalf("failure leaked to other thread: %+v", event)
	default:
	}
	if !b.Alive() {
		t.Fatal("one failed turn stopped another employee")
	}
	// A duplicate terminal event from the failed turn must not finish the
	// next queued turn on the same thread.
	write(map[string]any{"method": "turn/started", "params": map[string]any{"threadId": "a", "turn": map[string]any{"id": "new-turn"}}})
	write(map[string]any{"method": "error", "params": map[string]any{"threadId": "a", "turnId": "failed-turn", "willRetry": false, "error": map[string]any{"message": "old failure"}}})
	write(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "a", "turn": map[string]any{"id": "failed-turn", "status": "completed"}}})
	write(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "a", "turn": map[string]any{"id": "new-turn", "status": "completed"}}})
	select {
	case event := <-a.events:
		if event.Type != core.EventResult || !event.Done {
			t.Fatal(event)
		}
	case <-time.After(time.Second):
		t.Fatal("new turn was lost")
	}
	select {
	case event := <-a.events:
		t.Fatalf("duplicate terminal event: %+v", event)
	default:
	}
}

func TestCodexSharedThreadOverrideRetainsVerifiedNativeBase(t *testing.T) {
	base := map[string]any{"filesystem": map[string]any{":minimal": "read", "/opt/tools": "read", "glob_scan_max_depth": nil}, "network": map[string]any{"enabled": true, "domains": map[string]any{"127.0.0.1": "allow"}, "unix_sockets": nil}}
	dynamic := map[string]any{"filesystem": map[string]any{"/work": "write"}, "network": map[string]any{"unix_sockets": map[string]string{"/tmp/exact-office": "allow"}}}
	merged := sharedPermissionOverrides(base, dynamic)
	fs := merged["filesystem"].(map[string]any)
	if fs[":minimal"] != "read" || fs["/opt/tools"] != "read" || fs["/work"] != "write" {
		t.Fatal(fs)
	}
	if _, ok := fs["glob_scan_max_depth"]; ok {
		t.Fatal("native null default forwarded as an override")
	}
	network := merged["network"].(map[string]any)
	if network["enabled"] != true || network["domains"] == nil || network["unix_sockets"] == nil {
		t.Fatal(network)
	}
	if _, ok := base["filesystem"].(map[string]any)["/work"]; ok {
		t.Fatal("one thread changed base for another")
	}
}

func TestCodexSharedRegistrationKeepsAuthAndBorrowedStop(t *testing.T) {
	a := &Agent{cmd: "synthetic", codexHome: t.TempDir(), workDir: t.TempDir()}
	a.cmd, a.cliExtraArgs = core.ParseCmdOpts(map[string]any{"cmd": "synthetic"}, "codex")
	h, err := registerSharedServer(a)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.release() }()
	if !strings.Contains(strings.Join(h.prototype.spawnOpts.EnvAllowlist, ","), "CODEX_HOME") {
		t.Fatal("authentication directory stripped")
	}
	borrowed := &Agent{sharedServer: h, sharedBorrowed: true}
	if err := borrowed.Stop(); err != nil {
		t.Fatal(err)
	}
	if h.refs != 1 {
		t.Fatal("borrowed file agent released owner")
	}
	a.workDir = t.TempDir()
	other, err := registerSharedServer(a)
	if err != nil || other != h {
		t.Fatal("same CLI with no extra args rejected:", err)
	}
	defer func() { _ = other.release() }()
	a.workDir = t.TempDir()
	a.cliExtraArgs = []string{"changed"}
	if _, err := registerSharedServer(a); err == nil {
		t.Fatal("changed CLI arguments accepted")
	}
}

func TestCodexSharedRegistrationRejectsOverlappingEntrances(t *testing.T) {
	root := t.TempDir()
	work := filepath.Join(root, "sales")
	child := filepath.Join(work, "nested")
	if err := os.MkdirAll(child, 0700); err != nil {
		t.Fatal(err)
	}
	a := &Agent{cmd: "synthetic", codexHome: t.TempDir(), workDir: work}
	h, err := registerSharedServer(a)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.release() }()
	paths := []string{work, root, child}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(work, alias); err == nil {
		paths = append(paths, alias, filepath.Join(alias, "nested"))
	} else if runtime.GOOS != "windows" {
		t.Fatal(err)
	}
	for _, path := range paths {
		a.workDir = path
		if _, err := registerSharedServer(a); err == nil {
			t.Fatalf("overlapping entrance accepted: %s", path)
		}
	}
	if runtime.GOOS != "windows" {
		authAlias := filepath.Join(root, "auth-alias")
		if err := os.Symlink(a.codexHome, authAlias); err != nil {
			t.Fatal(err)
		}
		a.workDir = authAlias
		if _, err := registerSharedServer(a); err == nil {
			t.Fatal("workspace alias exposed auth directory")
		}
	}
	if h.refs != 1 {
		t.Fatal("failed registration retained an owner reference")
	}
	a.workDir = t.TempDir()
	other, err := registerSharedServer(a)
	if err != nil {
		t.Fatal(err)
	}
	if other != h {
		t.Fatal("separate login for another entrance")
	}
	_ = other.release()
}

func TestCodexSharedRegistryUsesNativeIdentity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("ordinary Unix identity")
	}
	name, err := exec.Command("/usr/bin/id", "-un").Output()
	if err != nil {
		t.Fatal(err)
	}
	username := strings.TrimSpace(string(name))
	uid, err := ordinaryRuntimeUID(username)
	if err != nil {
		t.Fatal(err)
	}
	if int(uid) != os.Getuid() {
		t.Fatal("wrong native UID")
	}
	a := &Agent{cmd: "synthetic", codexHome: t.TempDir(), workDir: t.TempDir(), spawnOpts: core.SpawnOptions{RunAsUser: username}}
	h, err := registerSharedServer(a)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.release() }()
	alias := filepath.Join(t.TempDir(), "same-auth")
	if err := os.Symlink(a.codexHome, alias); err != nil {
		t.Fatal(err)
	}
	a.codexHome = alias
	if _, err := registerSharedServer(a); err == nil {
		t.Fatal("second path for one auth owner accepted")
	}
}
