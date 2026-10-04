//go:build !windows

package core

import (
	"context"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsolationProbeDarwinAccountLookupTransientFailure(t *testing.T) {
	for _, fixture := range []struct {
		name, body, home, attempts string
	}{
		{"ready", "printf 'NFSHomeDirectory: /fixture/supervisor\\n'", "/fixture/supervisor", "1"},
		{"transient", "[ \"$n\" -ge 3 ] || exit 1; printf 'NFSHomeDirectory: /fixture/supervisor\\n'", "/fixture/supervisor", "3"},
		{"unresolved", "exit 1", "", "3"},
		{"missing_attribute", "printf 'No such key\\n'", "", "3"},
		{"failed_with_output", "printf 'NFSHomeDirectory: /fixture/supervisor\\n'; exit 1", "", "3"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			dir := t.TempDir()
			count := filepath.Join(dir, "count")
			dscl := filepath.Join(dir, "dscl")
			body := "#!/bin/sh\nn=0; [ ! -f " + shellQuote(count) + " ] || n=$(cat " + shellQuote(count) + ")\nn=$((n + 1)); printf '%s' \"$n\" > " + shellQuote(count) + "\n" + fixture.body + "\n"
			if err := os.WriteFile(dscl, []byte(body), 0700); err != nil {
				t.Fatal(err)
			}
			// Exercise the embedded production function with a controlled directory
			// service, including a persistent unknown account that must stay denied.
			script := strings.Split(string(ProbeScript()), "emit \"BEGIN probe-version=1\"")[0]
			script = strings.ReplaceAll(script, "/usr/bin/dscl", shellQuote(dscl))
			cmd := exec.Command("/bin/sh", "-s")
			cmd.Stdin = strings.NewReader("uname() { printf 'Darwin\\n'; }\n" + script + "\naccount_home _fixture_supervisor\n")
			out, err := cmd.CombinedOutput()
			if strings.TrimSpace(string(out)) != fixture.home || (fixture.home != "" && err != nil) || (fixture.home == "" && err == nil) {
				t.Fatalf("lookup = %q, %v; want %q", out, err, fixture.home)
			}
			attempts, err := os.ReadFile(count)
			if err != nil || string(attempts) != fixture.attempts {
				t.Fatalf("lookup attempts = %q, %v; want %s", attempts, err, fixture.attempts)
			}
		})
	}
}

func TestIsolationProbeRequiresSupervisor(t *testing.T) {
	for _, supervisor := range []string{"", "model"} {
		if _, err := RunIsolationProbe(context.Background(), AuditConfig{RunAsUser: "model", Supervisor: supervisor}); err == nil {
			t.Fatal("supervisor audit could be skipped")
		}
	}
}

func TestIsolationProbeNativeHomeAndUnknownUser(t *testing.T) {
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-s")
	cmd.Stdin = strings.NewReader(string(ProbeScript()))
	cmd.Env = append(os.Environ(), "CC_PROBE_WORKDIR="+t.TempDir(), "CC_PROBE_SUPERVISOR="+u.Username,
		"CC_PROBE_OTHER_USERS=_anc_missing_fixture", "HOME="+u.HomeDir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("probe: %v: %s", err, out)
	}
	var r IsolationReport
	parseProbeOutput(&r, string(out))
	if len(computeAuditFatal(r)) == 0 {
		t.Fatal("unknown account silently accepted")
	}
	// Also exercise the native account resolver without assuming getent exists.
	cmd = exec.Command("/bin/sh", "-s")
	cmd.Stdin = strings.NewReader(string(ProbeScript()) + "\naccount_home " + shellQuote(u.Username) + "\n")
	cmd.Env = append(os.Environ(), "CC_PROBE_OTHER_USERS=", "CC_PROBE_SUPERVISOR=")
	out, err = cmd.CombinedOutput()
	if err != nil || !strings.HasSuffix(strings.TrimSpace(string(out)), u.HomeDir) {
		t.Fatalf("native home lookup: %v: %s", err, out)
	}
}

func TestProbeUnknownSupervisorIsFatal(t *testing.T) {
	var r IsolationReport
	parseProbeOutput(&r, "BEGIN probe-version=1\nSUPERVISOR_UNKNOWN missing\nEND probe-version=1\n")
	if len(computeAuditFatal(r)) == 0 {
		t.Fatal("unknown supervisor silently accepted")
	}
}

// Golden-style test: feed a canned probe output through the parser and
// assert the resulting IsolationReport looks right. No real sudo.
func TestParseProbeOutput_Clean(t *testing.T) {
	out := `BEGIN probe-version=1
ID uid=1001(coder) gid=1001(coder) groups=1001(coder)
WHOAMI coder
GROUPS coder
UMASK 0022
PWD /home/coder
HOME /home/coder
SHELL /bin/bash
WORKDIR_PATH /tmp/wd
WORKDIR_EXISTS yes
WORKDIR_READABLE yes
WORKDIR_WRITABLE yes
TARGET_HAS /home/coder/.claude/settings.json
TARGET_MISSING /home/coder/.pgpass
CROSS_DENIED leigh /home/leigh/.claude/settings.json
CROSS_MISSING leigh /home/leigh/keys
SUPERVISOR_DENIED /home/supervisor/.claude/settings.json
END probe-version=1
`
	report := IsolationReport{Project: "demo", RunAsUser: "coder", WorkDir: "/tmp/wd"}
	parseProbeOutput(&report, out)
	report.Fatal = computeAuditFatal(report)

	if report.ProbeVersion != "1" {
		t.Errorf("probe version = %q, want 1", report.ProbeVersion)
	}
	if report.Identity.Whoami != "coder" {
		t.Errorf("whoami = %q, want coder", report.Identity.Whoami)
	}
	if !report.WorkDirStatus.Writable {
		t.Error("expected workdir writable")
	}
	if len(report.TargetPaths) != 2 {
		t.Errorf("target_paths count = %d, want 2", len(report.TargetPaths))
	}
	if len(report.CrossUser) != 2 {
		t.Errorf("cross_user count = %d, want 2", len(report.CrossUser))
	}
	if report.CrossUser[0].OtherUser != "leigh" || report.CrossUser[0].Status != "denied" {
		t.Errorf("first cross_user entry wrong: %+v", report.CrossUser[0])
	}
	if len(report.Supervisor) != 1 {
		t.Errorf("supervisor count = %d, want 1", len(report.Supervisor))
	}
	if report.HasFatal() {
		t.Errorf("want no fatal, got: %v", report.Fatal)
	}
}

func TestParseProbeOutput_CrossLeakIsFatal(t *testing.T) {
	out := `BEGIN probe-version=1
WORKDIR_PATH /tmp/wd
WORKDIR_WRITABLE yes
CROSS_LEAKED leigh /home/leigh/.claude/settings.json
END probe-version=1
`
	report := IsolationReport{Project: "demo", RunAsUser: "coder", WorkDir: "/tmp/wd"}
	parseProbeOutput(&report, out)
	report.Fatal = computeAuditFatal(report)
	if !report.HasFatal() {
		t.Fatal("want fatal for CROSS_LEAKED")
	}
	if !strings.Contains(report.Fatal[0], "CROSS_LEAKED") {
		t.Errorf("fatal message missing CROSS_LEAKED: %v", report.Fatal)
	}
}

func TestParseProbeOutput_SupervisorLeakIsFatal(t *testing.T) {
	out := `BEGIN probe-version=1
WORKDIR_PATH /tmp/wd
WORKDIR_WRITABLE yes
SUPERVISOR_LEAKED /home/supervisor/.pgpass
END probe-version=1
`
	report := IsolationReport{Project: "demo", RunAsUser: "coder", WorkDir: "/tmp/wd"}
	parseProbeOutput(&report, out)
	report.Fatal = computeAuditFatal(report)
	if !report.HasFatal() {
		t.Fatal("want fatal for SUPERVISOR_LEAKED")
	}
	if !strings.Contains(report.Fatal[0], "SUPERVISOR_LEAKED") {
		t.Errorf("fatal message missing SUPERVISOR_LEAKED: %v", report.Fatal)
	}
}

func TestParseProbeOutput_WorkdirNotWritableIsFatal(t *testing.T) {
	out := `BEGIN probe-version=1
WORKDIR_PATH /tmp/wd
WORKDIR_WRITABLE no
END probe-version=1
`
	report := IsolationReport{Project: "demo", RunAsUser: "coder", WorkDir: "/tmp/wd"}
	parseProbeOutput(&report, out)
	report.Fatal = computeAuditFatal(report)
	if !report.HasFatal() {
		t.Fatal("want fatal for non-writable workdir")
	}
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"":             "''",
		"simple":       "'simple'",
		"has space":    "'has space'",
		"it's":         `'it'\''s'`,
		"/tmp/w&d":     "'/tmp/w&d'",
		"$HOME/secret": "'$HOME/secret'",
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFilterOtherUsers(t *testing.T) {
	got := filterOtherUsers([]string{"alice", "", "coder", "bob"}, "coder")
	if len(got) != 2 || got[0] != "alice" || got[1] != "bob" {
		t.Errorf("filterOtherUsers = %v, want [alice bob]", got)
	}
}

func TestEmbeddedProbeScriptBeginsWithShebang(t *testing.T) {
	if !strings.HasPrefix(string(runasProbeScript), "#!/bin/sh\n") {
		t.Fatal("embedded probe script missing /bin/sh shebang")
	}
	// Make sure the script has at least the BEGIN and END markers.
	if !strings.Contains(string(runasProbeScript), "BEGIN probe-version=1") {
		t.Error("probe script missing BEGIN marker")
	}
	if !strings.Contains(string(runasProbeScript), "END probe-version=1") {
		t.Error("probe script missing END marker")
	}
}
