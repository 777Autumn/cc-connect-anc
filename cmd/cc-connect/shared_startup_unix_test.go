//go:build !windows

package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/chenhg5/cc-connect/actionhost/files"
	"github.com/chenhg5/cc-connect/actionhost/teambrain"
	"github.com/chenhg5/cc-connect/config"
	"github.com/chenhg5/cc-connect/core"
)

// Only the network boundary is mocked: no receiver or model starts here.
type startupFileSender struct{ core.Platform }

func (startupFileSender) FileReplyRoute(any) (json.RawMessage, error) {
	return nil, errors.New("offline fixture")
}
func (startupFileSender) SendFileWithReceipt(context.Context, json.RawMessage, core.FileAttachment, string) (string, error) {
	return "", core.ErrFileNotSubmitted
}

func TestSharedStartupOrdinaryIdentities(t *testing.T) {
	path := os.Getenv("ANC_STARTUP_FIXTURE")
	if path == "" {
		t.Skip("requires disposable ordinary-identity CI fixture")
	}
	var f struct {
		Model, Home, Root    string
		Works                []string
		GatewayUID, ModelUID int
	}
	b, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(b, &f) != nil || len(f.Works) != 4 || os.Geteuid() != f.GatewayUID || f.GatewayUID <= 0 || f.ModelUID <= 0 || f.ModelUID == f.GatewayUID {
		t.Fatal("invalid ordinary startup fixture", err)
	}
	cfg, hosts, _ := sharedPartnerFixture(t)
	for i := range cfg.Projects {
		p := &cfg.Projects[i]
		p.RunAsUser = f.Model
		p.Agent.Options["work_dir"] = f.Works[i]
		p.Agent.Options["codex_home"] = f.Home
	}
	if err := validateCRMProjectSet(cfg, hosts); err != nil {
		t.Fatal(err)
	}
	if err := validatePartnerRuntimeAccounts(cfg, user.Lookup); err != nil {
		t.Fatal(err)
	}
	u, err := user.Lookup(f.Model)
	if err != nil || u.Uid != strconv.Itoa(f.ModelUID) {
		t.Fatal("native account lookup", err)
	}
	if err := teambrain.ValidateAgentUser(f.Model); err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadDir(f.Home); !os.IsPermission(err) {
		t.Fatal("gateway must not read authentication directory:", err)
	}
	if err := runRunAsUserStartupChecks(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	supervisor, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	for i, p := range cfg.Projects {
		p.Agent.Options["run_as_user"] = f.Model
		p.Agent.Options["cmd"] = "/usr/bin/true" // Constructor only; no CLI/model process.
		p.Agent.Options["file_work_native"] = true
		agent, err := core.CreateAgent(p.Agent.Type, p.Agent.Options)
		if err != nil {
			t.Fatal("ordinary shared agent registration:", err)
		}
		t.Cleanup(func() {
			if err := agent.Stop(); err != nil {
				t.Error(err)
			}
		})
		a := hosts[p.Name]
		if err := a.EnableTools(); err != nil {
			t.Fatal(err)
		}
		if err := a.SetWorkDir(f.Works[i], f.Model); err != nil {
			t.Fatal(err)
		}
		pf := core.PreflightRunAsUser(context.Background(), core.PreflightConfig{Project: p.Name, RunAsUser: f.Model, WorkDir: f.Works[i]})
		if pf.HasFatal() || len(pf.Warnings) != 0 {
			t.Fatalf("startup audit incomplete: %+v", pf)
		}
		audit := core.AuditConfig{Project: p.Name, RunAsUser: f.Model, WorkDir: f.Works[i], Supervisor: supervisor.Username}
		r, err := core.RunIsolationProbe(context.Background(), audit)
		if err != nil || r.HasFatal() || len(r.Supervisor) < 6 || r.Identity.Whoami != f.Model {
			t.Fatalf("native supervisor audit: %+v: %v", r, err)
		}
		if i == 0 {
			audit.Supervisor = "_anc_missing_fixture"
			r, err = core.RunIsolationProbe(context.Background(), audit)
			if err == nil && !r.HasFatal() {
				t.Fatal("unknown supervisor accepted")
			}
			audit.Supervisor = supervisor.Username
			for _, output := range []string{"", "BEGIN probe-version=1\n", "BEGIN probe-version=1\nEND probe-version=1\n"} {
				audit.ProbeScriptOverride = []byte("cat <<'END_FIXTURE'\n" + output + "END_FIXTURE\n")
				if _, err := core.RunIsolationProbe(context.Background(), audit); err == nil {
					t.Fatal("incomplete audit accepted")
				}
			}
		}
		state := t.TempDir()
		ledger := filepath.Join(state, "files.db")
		if err := files.Initialize(ledger); err != nil {
			t.Fatal(err)
		}
		snapshots := filepath.Join(state, "snapshots")
		if err := os.Mkdir(snapshots, 0700); err != nil {
			t.Fatal(err)
		}
		p.FileWork = config.FileWorkConfig{Enabled: true, Runtime: "native", WorkBaseDir: filepath.Join(f.Root, "works"), Ledger: ledger, Snapshots: snapshots}
		engine := core.NewEngine(p.Name, agent, nil, filepath.Join(state, "session.json"), core.LangEnglish)
		server, closeHost, err := configureFileWork(p, engine, []core.Platform{startupFileSender{}})
		if err != nil || server != nil {
			t.Fatal("ordinary file startup:", err)
		}
		closeHost()
		work, err := files.PrepareWork(p.FileWork.WorkBaseDir, strings.Repeat(strconv.Itoa(i+1), 32), f.ModelUID)
		if err != nil || work == "" {
			t.Fatal("ordinary work creation:", err)
		}
	}
}
