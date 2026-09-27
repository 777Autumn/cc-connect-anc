package core

import "strings"

// PreserveCRMStageEnvironment carries only the existing scoped staging capability.
// Host secrets are still removed by each agent before filtering its child env.
func PreserveCRMStageEnvironment(opts SpawnOptions, env []string) SpawnOptions {
	opts.EnvAllowlist = append([]string(nil), opts.EnvAllowlist...)
	if envHasNonEmptyValue(env, "MYANC_CRM_ACTION_TOKEN") {
		// Root-owned sudoers keeps this single session value without placing
		// it in sudo's explicit-environment audit record. Logging stays on.
		opts.SudoEnvKeep = append(append([]string(nil), opts.SudoEnvKeep...), "MYANC_CRM_ACTION_TOKEN")
	}
	for _, key := range []string{"MYANC_CRM_ACTION_TOKEN", "MYANC_CRM_STAGE_INPUT"} {
		if !envHasNonEmptyValue(env, key) {
			continue
		}
		found := false
		for _, existing := range opts.EnvAllowlist {
			if existing == key {
				found = true
				break
			}
		}
		if !found {
			opts.EnvAllowlist = append(opts.EnvAllowlist, key)
		}
	}
	return opts
}

func envHasNonEmptyValue(env []string, key string) bool {
	prefix := key + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) && len(entry) > len(prefix) {
			return true
		}
	}
	return false
}
