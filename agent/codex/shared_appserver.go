package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os/user"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// One native process owns one login. Only its trusted in-process clients can
// address threads; there is no listener or credential copying service.
type sharedAppServer struct {
	mu          sync.Mutex
	connMu      sync.Mutex
	startMu     sync.Mutex
	prototype   *appServerSession
	transport   *appServerSession
	refs        int
	threads     map[string]*appServerSession
	closing     map[string]chan struct{}
	unavailable map[string]bool
	failureSent atomic.Bool
	workspaces  map[string]bool
	ownerUID    string
	stopped     bool
	blocked     map[string]int
}

var sharedServers = struct {
	sync.Mutex
	byHome map[string]*sharedAppServer
}{byHome: make(map[string]*sharedAppServer)}

func registerSharedServer(a *Agent) (*sharedAppServer, error) {
	sharedServers.Lock()
	defer sharedServers.Unlock()
	// Only process-wide transport settings enter the authentication owner.
	// Employee identity, host capabilities and work roots belong to threads.
	var configEnv []string
	for _, pair := range a.configEnv {
		key, _, _ := strings.Cut(pair, "=")
		switch key {
		case "PATH", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "SSL_CERT_FILE", "SSL_CERT_DIR":
			configEnv = append(configEnv, pair)
		}
	}
	sort.Strings(configEnv)
	spawn := a.spawnOpts
	spawn.EnvAllowlist = append(append([]string(nil), spawn.EnvAllowlist...), "CODEX_HOME")
	ownerUID := ""
	if spawn.RunAsUser != "" {
		owner, err := user.Lookup(spawn.RunAsUser)
		if err != nil {
			return nil, fmt.Errorf("codex: resolve shared process identity: %w", err)
		}
		ownerUID = owner.Uid
		for home, server := range sharedServers.byHome {
			if server.ownerUID == ownerUID && home != a.codexHome {
				return nil, errors.New("codex: one shared identity must use one exact authentication path")
			}
		}
	}
	if current := sharedServers.byHome[a.codexHome]; current != nil {
		p := current.prototype
		if p.cliBin != a.cmd || !reflect.DeepEqual(p.cliExtraArgs, a.cliExtraArgs) ||
			!reflect.DeepEqual(p.spawnOpts, spawn) || !reflect.DeepEqual(p.extraEnv, configEnv) {
			return nil, errors.New("codex: shared CODEX_HOME has inconsistent process configuration")
		}
		current.refs++
		current.mu.Lock()
		current.workspaces[a.workDir] = true
		current.mu.Unlock()
		return current, nil
	}
	p := makeAppServerSession(context.Background(), a.cmd, a.cliExtraArgs, "stdio://", "/var/empty", "", "", "full-auto", "", "", "", "", configEnv, a.codexHome, "", "", spawn)
	hub := &sharedAppServer{prototype: p, refs: 1, threads: make(map[string]*appServerSession), closing: make(map[string]chan struct{}), unavailable: make(map[string]bool), workspaces: map[string]bool{a.workDir: true}, ownerUID: ownerUID, blocked: make(map[string]int)}
	sharedServers.byHome[a.codexHome] = hub
	return hub, nil
}

func (h *sharedAppServer) connection() (*appServerSession, error) {
	h.connMu.Lock()
	defer h.connMu.Unlock()
	h.mu.Lock()
	stopped := h.stopped
	h.mu.Unlock()
	if stopped {
		return nil, errors.New("codex: shared process owner is stopped")
	}
	if h.transport != nil {
		if !h.transport.Alive() {
			return nil, errors.New("codex: shared process stopped; inspect it before restarting")
		}
		return h.transport, nil
	}
	p := h.prototype
	args := append(append([]string(nil), p.cliExtraArgs...), "-c", "thread_unload_delay_secs=0")
	s := makeAppServerSession(context.Background(), p.cliBin, args, p.url, p.workDir, "", "", "full-auto", "", "", "", "", p.extraEnv, p.codexHome, "", "", p.spawnOpts)
	s.hub = h
	// Set before launching: a failed startup must not silently create a new
	// authenticated process on the next employee message.
	h.transport = s
	if err := s.connect(); err != nil {
		s.alive.Store(false)
		s.cancel()
		return nil, err
	}
	// initialize responses do not need the thread map. Avoid waiting for a
	// process-exit callback while holding its map lock.
	if err := s.initialize(); err != nil {
		s.abortTransport()
		return nil, err
	}
	return s, nil
}

func (h *sharedAppServer) attach(s *appServerSession, resumeID string) error {
	// Only native start/resume is serialized. Model turns on different threads
	// run concurrently, and stopping one never closes this process's pipes.
	h.startMu.Lock()
	defer h.startMu.Unlock()
	h.mu.Lock()
	blocked := h.blocked[s.workDir]
	h.mu.Unlock()
	if blocked > 0 {
		return errors.New("codex: entry has an unconfirmed native operation; inspect before reuse")
	}
	conn, err := h.connection()
	if err != nil {
		return err
	}
	s.transport, s.hub = conn, h
	var config struct {
		Config map[string]any `json:"config"`
	}
	if err := s.request("config/read", map[string]any{"cwd": s.workDir, "includeLayers": false}, &config); err != nil {
		return fmt.Errorf("codex: cannot verify shared permissions: %w", err)
	}
	h.mu.Lock()
	private := []string{s.codexHome}
	for work := range h.workspaces {
		private = append(private, work)
	}
	h.mu.Unlock()
	if err := validateSharedConfig(config.Config, s.permissionProfile, private); err != nil {
		return err
	}
	if resumeID != "" && resumeID != core.ContinueSession {
		h.mu.Lock()
		busy := h.threads[resumeID] != nil || h.unavailable[resumeID]
		h.mu.Unlock()
		if busy {
			return errors.New("codex: thread is active or its unload is unconfirmed")
		}
		// Scope before loading any transcript or changing runtime parameters.
		if _, err := readNativeThread(s.request, s.workDir, resumeID, false); err != nil {
			return err
		}
		h.mu.Lock()
		h.unavailable[resumeID] = true
		h.mu.Unlock()
	}
	expectedWork := s.workDir
	if err := s.ensureThread(resumeID); err != nil {
		h.mu.Lock()
		h.blocked[expectedWork]++
		h.mu.Unlock()
		return err
	}
	if filepath.Clean(s.GetWorkDir()) != expectedWork {
		h.mu.Lock()
		h.unavailable[s.CurrentSessionID()] = true
		h.blocked[expectedWork]++
		h.mu.Unlock()
		return errors.New("codex: shared thread workspace mismatch")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.stopped || h.failureSent.Load() || !conn.Alive() {
		return errors.New("codex: shared process stopped while opening the thread")
	}
	id := s.CurrentSessionID()
	if h.threads[id] != nil {
		return errors.New("codex: shared thread already attached")
	}
	h.threads[id] = s
	delete(h.unavailable, id)
	return nil
}

// The protected base grants only read access to tools. Work roots and the one
// Office socket are added per thread; :workspace would otherwise read other
// employees' private files under the shared process identity.
func validateSharedConfig(config map[string]any, profile string, private []string) error {
	permissions, _ := config["permissions"].(map[string]any)
	policy, _ := permissions[profile].(map[string]any)
	fs, _ := policy["filesystem"].(map[string]any)
	if len(fs) == 0 || fs[":minimal"] != "read" {
		return errors.New("codex: shared permissions require an explicit minimal read allowlist")
	}
	for key, value := range policy {
		if key == "description" || ((key == "extends" || key == "workspace_roots") && value == nil) {
			continue // Native config/read includes these unset fields as null.
		}
		if key != "filesystem" && key != "network" {
			return errors.New("codex: shared permissions cannot inherit or add static work roots")
		}
	}
	for path, access := range fs {
		if path == "glob_scan_max_depth" && access == nil {
			continue
		}
		if access != "read" || (path != ":minimal" && (!filepath.IsAbs(path) || filepath.Clean(path) != path)) {
			return errors.New("codex: shared base filesystem must contain only explicit read paths")
		}
		if path == ":minimal" {
			continue
		}
		for _, root := range private {
			if root != "" && (withinPath(path, root) || withinPath(root, path)) {
				return errors.New("codex: shared base exposes a private workspace or authentication directory")
			}
		}
	}
	return nil
}

func withinPath(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (h *sharedAppServer) detach(s *appServerSession) error {
	if !s.alive.Swap(false) {
		s.closeEvents()
		return nil
	}
	h.mu.Lock()
	h.blocked[s.GetWorkDir()]++
	h.mu.Unlock()
	// Wait only for this thread's turn/start response, not for model completion.
	// Cancelling that RPC first would lose its turn ID and leave a task running.
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	s.cancel()
	s.rejectPendingApprovals(context.Canceled)
	id := s.CurrentSessionID()
	s.stateMu.Lock()
	turnID := s.currentTurn
	s.stateMu.Unlock()
	h.mu.Lock()
	closed := make(chan struct{})
	h.closing[id], h.unavailable[id] = closed, true
	h.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	call := func(method string, params any) error {
		return s.transport.requestWithContext(ctx, method, params, nil, 15*time.Second)
	}
	var err error
	if turnID != "" {
		err = call("turn/interrupt", map[string]any{"threadId": id, "turnId": turnID})
	}
	if err == nil {
		err = call("thread/unsubscribe", map[string]any{"threadId": id})
	}
	if err == nil {
		select {
		case <-closed:
		case <-ctx.Done():
			err = fmt.Errorf("codex: native thread unload unconfirmed: %w", ctx.Err())
		}
	}
	h.mu.Lock()
	delete(h.threads, id)
	delete(h.closing, id)
	if err == nil {
		delete(h.unavailable, id)
		h.blocked[s.GetWorkDir()]--
	}
	h.mu.Unlock()
	s.closeEvents()
	return err
}

func (h *sharedAppServer) notification(method string, params json.RawMessage) {
	var scope struct {
		ThreadID string `json:"threadId"`
	}
	if json.Unmarshal(params, &scope) != nil {
		return
	}
	h.mu.Lock()
	if method == "thread/closed" {
		if closed := h.closing[scope.ThreadID]; closed != nil {
			close(closed)
			delete(h.closing, scope.ThreadID)
		}
	}
	var targets []*appServerSession
	if method == "account/rateLimits/updated" {
		for _, s := range h.threads {
			targets = append(targets, s)
		}
	} else if s := h.threads[scope.ThreadID]; s != nil {
		targets = append(targets, s)
	}
	h.mu.Unlock()
	for _, s := range targets {
		s.handleNotification(method, params)
	}
}

func (h *sharedAppServer) serverRequest(probe map[string]json.RawMessage) {
	var scope struct {
		ThreadID string `json:"threadId"`
	}
	var method string
	_ = json.Unmarshal(probe["params"], &scope)
	_ = json.Unmarshal(probe["method"], &method)
	h.mu.Lock()
	s := h.threads[scope.ThreadID]
	h.mu.Unlock()
	// Questions remain interactive. A model cannot enlarge the shared-process
	// file/network policy through an employee-facing technical approval.
	if s != nil && s.Alive() && method == "item/tool/requestUserInput" {
		s.handleServerRequest(probe)
		return
	}
	if err := h.transport.writeJSONWithTimeout("reject request", map[string]any{"jsonrpc": "2.0", "id": probe["id"], "error": map[string]any{"code": -32601, "message": "request unavailable for this thread"}}, appServerUsageRefreshTimeout); err != nil {
		slog.Error("codex: shared request response failed", "error", err)
		h.failed(err)
	}
}

func (h *sharedAppServer) failed(err error) {
	if h.failureSent.Swap(true) {
		return
	}
	h.mu.Lock()
	var sessions []*appServerSession
	for _, s := range h.threads {
		sessions = append(sessions, s)
	}
	h.mu.Unlock()
	for _, s := range sessions {
		s.alive.Store(false)
		s.emitError(fmt.Errorf("codex: shared process exited: %w", err))
		s.cancel()
		s.rejectPendingApprovals(err)
	}
}

func (h *sharedAppServer) release() error {
	sharedServers.Lock()
	defer sharedServers.Unlock()
	h.refs--
	if h.refs != 0 {
		return nil
	}
	delete(sharedServers.byHome, h.prototype.codexHome)
	h.mu.Lock()
	h.stopped = true
	h.mu.Unlock()
	h.prototype.cancel()
	h.connMu.Lock()
	defer h.connMu.Unlock()
	if h.transport != nil {
		return h.transport.Close()
	}
	return nil
}

func sharedThreadConfig(work, fileRoot, profile, officeSocket string, env []string, spawn core.SpawnOptions) map[string]any {
	set := map[string]string{"PATH": "/usr/bin:/bin:/usr/sbin:/sbin"}
	for _, pair := range core.FilterEnvForSpawn(env, core.PreserveCRMStageEnvironment(spawn, env)) {
		key, value, ok := strings.Cut(pair, "=")
		if !ok {
			continue
		}
		switch key {
		case "CODEX_HOME", "OPENAI_API_KEY", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "MYANC_CRM_HOST_SECRET", "MYANC_CRM_HOST_SECRET_FILE":
			continue
		}
		set[key] = value
	}
	tmp := work
	fs := map[string]any{work: "write"}
	for _, name := range []string{"AGENTS.md", "AGENTS.override.md", "CLAUDE.md", ".agents", ".codex", ".claude", ".git"} {
		fs[filepath.Join(work, name)] = "read"
	}
	if fileRoot != "" {
		fs[fileRoot] = "write"
		tmp = filepath.Join(fileRoot, "outputs")
	}
	set["HOME"], set["TMPDIR"], set["TMP"], set["TEMP"] = "/var/empty", tmp, tmp, tmp
	policy := map[string]any{"filesystem": fs}
	if officeSocket != "" {
		fs[officeSocket] = "write"
		policy["network"] = map[string]any{"unix_sockets": map[string]string{officeSocket: "allow"}}
	}
	return map[string]any{
		"features":                 map[string]bool{"multi_agent": false, "shell_snapshot": false},
		"shell_environment_policy": map[string]any{"inherit": "none", "set": set},
		"permissions":              map[string]any{profile: policy},
	}
}
