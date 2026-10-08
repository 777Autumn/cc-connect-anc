//go:build anc_capacity_canary && darwin

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/actionhost/crmfollowup"
	"github.com/chenhg5/cc-connect/actionhost/files"
	"github.com/chenhg5/cc-connect/config"
	"github.com/chenhg5/cc-connect/core"
)

type capacityJob struct {
	ID, Prompt      string
	Entrance        int
	DeadlineSeconds int
	Queries         int
}

type capacityManifest struct {
	Root, ConfigPath, ConfigSHA256, CRMRoot, Python string
	Cohorts                                         int
	Jobs                                            []capacityJob
}

type capacityEvents struct {
	mu   sync.Mutex
	file *os.File
	err  error
}

func (e *capacityEvents) write(kind string, data any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.err == nil {
		e.err = json.NewEncoder(e.file).Encode(map[string]any{"at": time.Now().UTC(), "kind": kind, "data": data})
	}
}

// Only Feishu input/output is substituted. File bytes are copied from the real
// host callback; these receipts never claim delivery to an actual Feishu user.
type capacityPlatform struct {
	core.Platform
	root    string
	events  *capacityEvents
	mu      sync.Mutex
	replies int
}

func (*capacityPlatform) Name() string                        { return "feishu" }
func (*capacityPlatform) Start(core.MessageHandler) error     { return nil }
func (*capacityPlatform) Stop() error                         { return nil }
func (*capacityPlatform) SetFileWorkImagesEnabled(bool) error { return nil }
func (p *capacityPlatform) Reply(_ context.Context, route any, content string) error {
	p.mu.Lock()
	p.replies++
	p.mu.Unlock()
	p.events.write("platform_reply", map[string]any{"route": route, "text": content})
	return nil
}
func (p *capacityPlatform) Send(ctx context.Context, route any, content string) error {
	return p.Reply(ctx, route, content)
}
func (*capacityPlatform) FileReplyRoute(route any) (json.RawMessage, error) {
	return json.Marshal(route)
}
func (p *capacityPlatform) SendFileWithReceipt(_ context.Context, route json.RawMessage, file core.FileAttachment, id string) (string, error) {
	if filepath.Base(file.FileName) != file.FileName || file.FileName == "" {
		return "", core.ErrFileNotSubmitted
	}
	name := fmt.Sprintf("%x-%s", sha256.Sum256([]byte(id)), file.FileName)
	path := filepath.Join(p.root, name)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return "", fmt.Errorf("capture file: %w: %w", core.ErrFileNotSubmitted, err)
	}
	_, writeErr := f.Write(file.Data)
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		return "", fmt.Errorf("write captured file: %w: %w", core.ErrFileNotSubmitted, errors.Join(writeErr, closeErr))
	}
	p.events.write("fixture_file_capture", map[string]any{"route": json.RawMessage(route), "name": file.FileName, "path": path, "sha256": fmt.Sprintf("%x", sha256.Sum256(file.Data)), "bytes": len(file.Data)})
	return "capacity-fixture-" + id, nil
}

func capacityHost(m capacityManifest, cohort int, events *capacityEvents) *crmfollowup.Adapter {
	return crmfollowup.NewCapacityFixture("offline-gateway-host-secret-00000000000001",
		func(ctx context.Context, _ string, command string, input []byte, env []string) ([]byte, error) {
			started := time.Now()
			cmd := exec.CommandContext(ctx, m.Python, "-I", "-B", filepath.Join(m.CRMRoot, "spikes", "host_tool_fixture.py"), command)
			cmd.Env = append(env, "MYANC_SPIKE_SEED=capacity", "MYANC_SPIKE_DIR="+filepath.Join(m.Root, fmt.Sprint(cohort), "crm"))
			cmd.Stdin = bytes.NewReader(input)
			out, err := cmd.Output()
			var request map[string]any
			_ = json.Unmarshal(input, &request)
			var result map[string]any
			_ = json.Unmarshal(out, &result)
			events.write("host_call", map[string]any{"cohort": cohort, "command": command, "operation": request["command"], "elapsed_ms": time.Since(started).Milliseconds(), "ok": err == nil, "status": result["status"], "code": result["code"]})
			return out, err
		})
}

func capacityConfig(t *testing.T, m capacityManifest, source *config.Config, cohort int, events *capacityEvents) (*config.Config, map[string]*crmfollowup.Adapter) {
	t.Helper()
	c := &config.Config{}
	hosts := map[string]*crmfollowup.Adapter{}
	for i, original := range source.Projects {
		p := original
		p.Name = fmt.Sprintf("capacity-%d", i)
		p.Agent.Options = make(map[string]any)
		for k, v := range original.Agent.Options {
			p.Agent.Options[k] = v
		}
		base := filepath.Join(m.Root, fmt.Sprint(cohort))
		p.Agent.Options["work_dir"] = filepath.Join(base, fmt.Sprint(i), "workspace")
		// Keep the combined budget <= the installed four-entry 340 MiB budget.
		p.Agent.Options["image_cache_capacity_mib"] = int64(340 / (4 * m.Cohorts))
		actor := fmt.Sprintf("sender-%d", i/2+1)
		chat := "group-1"
		if i%2 == 0 {
			chat = "private-" + actor
		}
		p.Platforms = []config.PlatformConfig{{Type: "feishu", Options: map[string]any{
			"app_id": "capacity-app", "app_secret": "offline-only", "allow_from": actor, "allow_chat": chat,
			"enable_feishu_card": true, "thread_isolation": false, "share_session_in_channel": false}}}
		p.FileWork.Ledger = filepath.Join(base, "host", "files.sqlite3")
		p.FileWork.Snapshots = filepath.Join(base, "host", "snapshots")
		p.FileWork.WorkBaseDir = filepath.Join(base, fmt.Sprint(i), "works")
		hosts[p.Name] = capacityHost(m, cohort, events)
		c.Projects = append(c.Projects, p)
	}
	if err := validateCRMProjectSet(c, hosts); err != nil {
		t.Fatal(err)
	}
	if err := validatePartnerRuntimeAccounts(c, user.Lookup); err != nil {
		t.Fatal(err)
	}
	if err := runRunAsUserStartupChecks(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	return c, hosts
}

// TestCapacityNative is opt-in and requires a protected pre-provisioned manifest
// and an exclusive maintenance window. It cannot start a Feishu receiver.
func TestCapacityNative(t *testing.T) {
	path := os.Getenv("ANC_CAPACITY_MANIFEST")
	if path == "" {
		t.Skip("requires reviewed exclusive-window capacity manifest")
	}
	if os.Geteuid() != 455 {
		t.Fatal("requires the existing ordinary gateway UID")
	}
	if err := files.ValidateHostFile(path); err != nil {
		t.Fatal("unprotected manifest", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m capacityManifest
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err = d.Decode(&m); err != nil {
		t.Fatal(err)
	}
	if m.Cohorts < 1 || m.Cohorts > 2 || len(m.Jobs) < 1 || len(m.Jobs) > 8 || !strings.HasPrefix(m.Root, "/private/var/lib/anc-capacity/") || filepath.Clean(m.Root) != m.Root {
		t.Fatal("invalid capacity manifest")
	}
	for _, p := range []string{m.Root, m.CRMRoot} {
		if err := files.ValidateHostDirectory(p); err != nil {
			t.Fatal("unprotected fixture directory", err)
		}
	}
	if m.Python != "/opt/team-files/bin/python" || m.ConfigPath != "/usr/local/etc/cc-connect/config.toml" {
		t.Fatal("unexpected installed runtime")
	}
	raw, err = os.ReadFile(m.ConfigPath)
	if err != nil || fmt.Sprintf("%x", sha256.Sum256(raw)) != m.ConfigSHA256 {
		t.Fatal("installed configuration changed")
	}
	source, err := config.Load(m.ConfigPath)
	if err != nil || len(source.Projects) != 4 {
		t.Fatal("requires four pinned source entrances", err)
	}
	for _, p := range source.Projects {
		if p.RunAsUser != "_anctestp" || p.Agent.Options["shared_app_server"] != true || p.Agent.Options["permission_profile"] != "anc-shared" || p.Agent.Options["model"] != "gpt-6-luna" || p.Agent.Options["cmd"] != "/usr/local/lib/anc-codex/0.157.1/bin/codex" {
			t.Fatal("source runtime differs from reviewed inventory")
		}
	}
	used := map[int]bool{}
	for _, j := range m.Jobs {
		if j.Entrance < 0 || j.Entrance >= 4*m.Cohorts || used[j.Entrance] || j.ID == "" || j.Prompt == "" || j.DeadlineSeconds < 1 || j.DeadlineSeconds > 600 {
			t.Fatal("invalid job or duplicate entrance")
		}
		used[j.Entrance] = true
		if j.Queries != 0 && (j.Queries != 3 || j.Entrance%2 != 1) {
			t.Fatal("three sequential probes require a shared read entrance")
		}
	}
	processes, err := exec.Command("/bin/ps", "-axo", "uid=,comm=").Output()
	if err != nil {
		t.Fatal("cannot verify exclusive model ownership", err)
	}
	for _, line := range strings.Split(string(processes), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "451" && filepath.Base(fields[1]) == "codex" {
			t.Fatal("an existing model owner is still running; use normal maintenance lifecycle")
		}
	}
	logFile, err := os.OpenFile(filepath.Join(m.Root, "events.jsonl"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	events := &capacityEvents{file: logFile}
	t.Cleanup(func() { logFile.Close() })
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(logFile, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	engines := []*core.Engine{}
	platforms := []*capacityPlatform{}
	for cohort := 0; cohort < m.Cohorts; cohort++ {
		cfg, hosts := capacityConfig(t, m, source, cohort, events)
		for i, p := range cfg.Projects {
			base := filepath.Join(m.Root, fmt.Sprint(cohort), fmt.Sprint(i))
			opts := p.Agent.Options
			opts["run_as_user"], opts["run_as_env"], opts["file_work_native"] = p.RunAsUser, p.RunAsEnv, true
			ag, err := core.CreateAgent("codex", opts)
			if err != nil {
				t.Fatal(err)
			}
			platform := &capacityPlatform{root: filepath.Join(base, "delivered"), events: events}
			engine := core.NewEngine(p.Name, ag, []core.Platform{platform}, filepath.Join(base, "sessions.json"), core.LangChinese)
			closeFiles := func() {}
			t.Cleanup(func() {
				if err := engine.Stop(); err != nil {
					t.Error(err)
				}
				closeFiles()
			})
			host := hosts[p.Name]
			if err = host.SetWorkDir(opts["work_dir"].(string), p.RunAsUser); err != nil {
				t.Fatal(err)
			}
			enabled, err := host.ConfigureConnections(context.Background(), "capacity-app")
			if err != nil || !enabled {
				t.Fatal("real connection guard missing", err)
			}
			engine.SetActionHost(host)
			engine.SetConnectionHost(host)
			if source.Queue.MaxDepth != nil {
				engine.SetMaxQueuedMessages(*source.Queue.MaxDepth)
			}
			if source.IdleTimeoutMins != nil {
				engine.SetEventIdleTimeout(time.Duration(*source.IdleTimeoutMins) * time.Minute)
			}
			if source.MaxTurnTimeMins != nil {
				engine.SetMaxTurnTime(time.Duration(*source.MaxTurnTimeMins) * time.Minute)
			}
			_, closeHost, err := configureFileWork(p, engine, []core.Platform{platform})
			if err != nil {
				t.Fatal(err)
			}
			closeFiles = closeHost
			engines = append(engines, engine)
			platforms = append(platforms, platform)
		}
	}
	server, err := core.ListenActionTools("127.0.0.1:18743", core.ActionToolsHandler(engines...))
	if err != nil {
		t.Fatal("exclusive tool port unavailable", err)
	}
	t.Cleanup(func() { _ = server.Close() })
	barrier := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		if err := server.Serve(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			events.write("tool_server_failed", map[string]any{"error": err.Error()})
			t.Errorf("capacity tool server failed: %v", err)
			cancel()
		}
	}()
	// An operator can create this protected marker; models cannot write m.Root.
	go func() {
		warningSince := time.Time{}
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_, markerErr := os.Lstat(filepath.Join(m.Root, "CANCEL"))
				probeCtx, probeCancel := context.WithTimeout(ctx, 3*time.Second)
				pressure, probeErr := exec.CommandContext(probeCtx, "/usr/sbin/sysctl", "-n", "kern.memorystatus_vm_pressure_level").Output()
				probeCancel()
				level := strings.TrimSpace(string(pressure))
				if level == "2" && warningSince.IsZero() {
					warningSince = time.Now()
				} else if level != "2" {
					warningSince = time.Time{}
				}
				if markerErr == nil || !os.IsNotExist(markerErr) || probeErr != nil || (level != "1" && level != "2") || (!warningSince.IsZero() && time.Since(warningSince) >= time.Minute) {
					events.write("stop_requested", map[string]any{"pressure": level, "operator_marker": markerErr == nil, "probe_failed": probeErr != nil})
					cancel()
					return
				}
			}
		}
	}()
	var wg sync.WaitGroup
	for _, job := range m.Jobs {
		wg.Add(1)
		go func(j capacityJob) {
			defer wg.Done()
			<-barrier
			count := j.Queries
			if count == 0 {
				count = 1
			}
			for n := 0; n < count; n++ {
				probe := j
				if count > 1 {
					probe.ID = fmt.Sprintf("%s-%d", j.ID, n+1)
					probe.Prompt += fmt.Sprintf("\n本次查询编号 %s；必须重新读取，不复述上一轮。", probe.ID)
				}
				if !capacityRunJob(ctx, t, probe, engines[j.Entrance], platforms[j.Entrance], events) {
					cancel()
					return
				}
			}
		}(job)
	}
	close(barrier)
	wg.Wait()
	events.mu.Lock()
	err = events.err
	events.mu.Unlock()
	if err != nil {
		t.Fatal("capacity evidence write failed", err)
	}
}

func capacityRunJob(ctx context.Context, t *testing.T, j capacityJob, e *core.Engine, p *capacityPlatform, events *capacityEvents) bool {
	t.Helper()
	i := j.Entrance % 4
	actor := fmt.Sprintf("sender-%d", i/2+1)
	chat := "group-1"
	if i%2 == 0 {
		chat = "private-" + actor
	}
	key := "feishu:" + chat + ":" + actor
	msg := &core.Message{Platform: "feishu", SessionKey: key, ChannelID: chat, UserID: actor, MessageID: j.ID,
		Content: j.Prompt, ReplyCtx: j.ID, ControlledFileWork: true, FileWorkPrivate: i%2 == 0}
	events.write("submitted", map[string]any{"case": j.ID, "entrance": j.Entrance})
	p.mu.Lock()
	previousReplies := p.replies
	p.mu.Unlock()
	started := time.Now()
	if ctx.Err() != nil {
		return false
	}
	e.ReceiveMessage(p, msg)
	deadline := started.Add(time.Duration(j.DeadlineSeconds) * time.Second)
	for ctx.Err() == nil && time.Now().Before(deadline) {
		sessions := e.GetSessions().ListSessions(key)
		busy := false
		for _, s := range sessions {
			busy = busy || s.Busy()
		}
		p.mu.Lock()
		replied := p.replies > previousReplies
		p.mu.Unlock()
		if len(sessions) > 0 && !busy && replied {
			// A refusal or EventError can also reply and unlock. Require the
			// existing Engine EventResult receipt for this exact message ID.
			raw, err := os.ReadFile(events.file.Name())
			if err != nil || !capacityCompleted(raw, j.ID) {
				events.write("missing_success_event", map[string]any{"case": j.ID})
				t.Errorf("%s ended without a confirmed Engine result", j.ID)
				return false
			}
			events.write("engine_finished", map[string]any{"case": j.ID, "elapsed_ms": time.Since(started).Milliseconds(), "quality": "requires_artifact_and_native_event_validation"})
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	events.write("deadline", map[string]any{"case": j.ID})
	stop := *msg
	stop.Content = "/stop"
	stop.MessageID = j.ID + "-stop"
	e.ReceiveMessage(p, &stop)
	t.Errorf("%s cancelled or exceeded the frozen test deadline; no escalation", j.ID)
	return false
}

func capacityCompleted(raw []byte, id string) bool {
	for _, line := range bytes.Split(raw, []byte("\n")) {
		var event struct{ Msg, MsgID string }
		var fields map[string]json.RawMessage
		if json.Unmarshal(line, &fields) == nil {
			_ = json.Unmarshal(fields["msg"], &event.Msg)
			_ = json.Unmarshal(fields["msg_id"], &event.MsgID)
			if event.Msg == "turn complete" && event.MsgID == id {
				return true
			}
		}
	}
	return false
}

func TestCapacityCompletionEvidence(t *testing.T) {
	for _, raw := range []string{`{"msg":"agent error","msg_id":"case"}`, `{"msg":"turn complete","msg_id":"other"}`, `{"kind":"platform_reply"}`} {
		if capacityCompleted([]byte(raw), "case") {
			t.Fatal("reply/error/another turn is not successful completion")
		}
	}
	if !capacityCompleted([]byte(`{"msg":"turn complete","msg_id":"case"}`), "case") {
		t.Fatal("matching Engine result missing")
	}
}
