package codex

import (
	"errors"
	"fmt"
	"path/filepath"
	"runtime"

	"github.com/chenhg5/cc-connect/core"
)

var _ core.FileWorkAgent = (*Agent)(nil)

// Preserve the employee's project and authentication; add only this host-bound
// work to the existing workspace-write policy. This never enables a listener.
func (a *Agent) ForFileWork(root string) (core.Agent, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if (runtime.GOOS != "linux" && runtime.GOOS != "darwin") || !a.fileWorkNative ||
		!filepath.IsAbs(root) || filepath.Clean(root) != root ||
		!filepath.IsAbs(a.cmd) || !filepath.IsAbs(a.workDir) || !filepath.IsAbs(a.codexHome) ||
		a.spawnOpts.RunAsUser == "" || a.backend != "app_server" || a.appServerURL != "stdio://" ||
		(a.mode != "full-auto" && a.mode != "auto-edit") {
		return nil, errors.New("codex: native file runtime requires an isolated stdio workspace-write agent")
	}
	spawn := a.spawnOpts
	spawn.EnvAllowlist = append(append([]string(nil), spawn.EnvAllowlist...), "CC_FILE_ACTION_TOKEN", "CC_FILE_TRANSPORT", "CC_FILE_WORK_ROOT")
	// Reuse the deployment's env_keep rule; capability values must not appear
	// in sudo's explicit-environment audit record.
	spawn.SudoEnvKeep = append(append([]string(nil), spawn.SudoEnvKeep...), "CC_FILE_ACTION_TOKEN")
	args := append(append([]string(nil), a.cliExtraArgs...), "-c", fmt.Sprintf("sandbox_workspace_write.writable_roots=[%q]", root))
	return &Agent{
		cmd: a.cmd, cliExtraArgs: args, workDir: a.workDir, codexHome: a.codexHome,
		model: a.model, reasoningEffort: a.reasoningEffort, mode: a.mode,
		backend: a.backend, appServerURL: a.appServerURL, spawnOpts: spawn,
		systemPrompt: a.systemPrompt, appendPrompt: a.appendPrompt,
		configEnv: append(append([]string(nil), a.configEnv...), "CC_FILE_TRANSPORT=native", "CC_FILE_WORK_ROOT="+root),
		providers: append([]core.ProviderConfig(nil), a.providers...), activeIdx: a.activeIdx,
		fileWorkNative: true, fileWorkRoot: root,
	}, nil
}
