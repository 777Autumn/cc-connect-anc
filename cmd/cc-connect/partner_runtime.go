package main

import (
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/chenhg5/cc-connect/config"
)

// Sharing is explicit and all-or-nothing for the fixed Codex entrances.
// The adapter also verifies canonical roots and process configuration before
// any platform receiver is prepared.
func sharedCodexOwner(cfg *config.Config) (string, error) {
	owner, auth := "", ""
	count := 0
	for _, p := range cfg.Projects {
		shared, _ := p.Agent.Options["shared_app_server"].(bool)
		if !shared {
			continue
		}
		count++
		home, _ := p.Agent.Options["codex_home"].(string)
		if p.Agent.Type != "codex" || validateCRMActionHostProject(p) != nil ||
			!filepath.IsAbs(home) || filepath.Clean(home) != home ||
			(owner != "" && (owner != p.RunAsUser || auth != home)) {
			return "", errors.New("shared entrances require one explicit Codex runtime and authentication directory")
		}
		owner, auth = p.RunAsUser, home
	}
	if count != 0 && (count < 4 || count > 10 || count%2 != 0 || count != len(cfg.Projects)) {
		return "", errors.New("shared runtime requires all fixed entrances for two to five members; mixed modes are unsupported")
	}
	return owner, nil
}

// A different login name need not mean a different UID or HOME. Check the OS
// accounts before constructing agents, even when the configuration is valid.
func validatePartnerRuntimeAccounts(cfg *config.Config, lookup func(string) (*user.User, error)) error {
	shared, err := sharedCodexOwner(cfg)
	if err != nil {
		return err
	}
	uids, homes := map[string]bool{}, map[string]bool{}
	sharedUID, sharedHome := "", ""
	for _, project := range cfg.Projects {
		account, err := lookup(project.RunAsUser)
		if err != nil || account == nil || account.Username != project.RunAsUser || account.Uid == "" || account.Uid == "0" || (shared == "" && uids[account.Uid]) {
			return errors.New("fixed environments require verified distinct non-root runtime UIDs")
		}
		home := filepath.Clean(account.HomeDir)
		if shared != "" {
			uid, err := strconv.ParseUint(account.Uid, 10, 32)
			if err != nil || uid == 0 || uid == uint64(os.Geteuid()) || !filepath.IsAbs(home) ||
				(sharedUID != "" && (sharedUID != account.Uid || sharedHome != home)) {
				return errors.New("shared runtime requires one verified ordinary owner distinct from the gateway")
			}
			sharedUID, sharedHome = account.Uid, home
			continue
		}
		work, _ := project.Agent.Options["work_dir"].(string)
		rel, err := filepath.Rel(home, work)
		if !filepath.IsAbs(home) || homes[home] || err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return errors.New("each fixed workspace must be inside its own distinct runtime HOME")
		}
		uids[account.Uid], homes[home] = true, true
	}
	return nil
}
