package codex

import (
	"context"
	"fmt"
	"os"
	"os/exec"

	"github.com/chenhg5/cc-connect/core"
)

// All three CLI paths must use the same identity and environment: turns,
// persistent app-server sessions, and the exec backend's config inspection.
func buildCodexCommand(ctx context.Context, spawn core.SpawnOptions, bin string, args []string, workDir string, extraEnv []string) (*exec.Cmd, error) {
	// Check before allocating pipes: a rejected identity must not leak handles.
	if spawn.RunAsUser != "" {
		if err := core.VerifyRunAsUserCheap(ctx, core.ExecSudoRunner{}, spawn.RunAsUser); err != nil {
			return nil, fmt.Errorf("codex: runtime identity check: %w", err)
		}
	}
	if bin == "" {
		bin = "codex"
	}
	spawn.WorkDir = workDir
	cmd := core.BuildSpawnCommand(ctx, spawn, bin, args...)
	if !spawn.IsolationMode() {
		cmd.Dir = workDir
	}
	env := core.MergeEnv(os.Environ(), extraEnv)
	if spawn.IsolationMode() {
		env = core.MergeEnv(env, []string{core.RunAsChdirEnv + "=" + workDir})
	}
	cmd.Env = core.FilterEnvForSpawn(env, spawn)
	prepareCmdForKill(cmd)
	return cmd, nil
}
