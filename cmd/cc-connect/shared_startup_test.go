package main

import (
	"os"
	"os/user"
	"path/filepath"
	"testing"

	"github.com/chenhg5/cc-connect/actionhost/crmfollowup"
	"github.com/chenhg5/cc-connect/config"
)

func sharedPartnerFixture(t *testing.T) (*config.Config, map[string]*crmfollowup.Adapter, func(string) (*user.User, error)) {
	t.Helper()
	cfg, hosts := partnerConfigFixture(t)
	home := t.TempDir()
	auth := filepath.Join(home, ".codex")
	if err := os.Mkdir(auth, 0700); err != nil {
		t.Fatal(err)
	}
	for i := range cfg.Projects {
		p := &cfg.Projects[i]
		p.RunAsUser = "_shared_fixture"
		p.Agent.Type = "codex"
		p.Agent.Options["shared_app_server"] = true
		p.Agent.Options["backend"] = "app_server"
		p.Agent.Options["app_server_url"] = "stdio"
		p.Agent.Options["mode"] = "full-auto"
		p.Agent.Options["permission_profile"] = "anc-shared"
		p.Agent.Options["codex_home"] = auth
	}
	return cfg, hosts, func(name string) (*user.User, error) {
		return &user.User{Username: name, Uid: "61009", Gid: "61009", HomeDir: home}, nil
	}
}

func TestSharedPartnerStartupKeepsFixedRoutesAndOrdinaryOwner(t *testing.T) {
	cfg, hosts, lookup := sharedPartnerFixture(t)
	if err := validateCRMProjectSet(cfg, hosts); err != nil {
		t.Fatal("shared static startup:", err)
	}
	if err := validatePartnerRuntimeAccounts(cfg, lookup); err != nil {
		t.Fatal("shared OS startup:", err)
	}
	registered := map[string]bool{}
	for name := range hosts {
		registered[name] = true
	}
	routes, err := reminderRoutes(cfg, registered)
	if err != nil || len(routes) != 4 {
		t.Fatal("four-entry reminders:", err)
	}
	for _, r := range routes {
		if r.PrivateChat == "" || r.PrivateChat == "group" {
			t.Fatal("missing private binding")
		}
	}
	for name, mutate := range map[string]func(*config.Config){
		"mixed mode":     func(c *config.Config) { c.Projects[1].Agent.Options["shared_app_server"] = false },
		"different user": func(c *config.Config) { c.Projects[1].RunAsUser = "other" },
		"different auth": func(c *config.Config) {
			c.Projects[1].Agent.Options["codex_home"] = filepath.Join(t.TempDir(), ".codex")
		},
		"wrong backend": func(c *config.Config) { c.Projects[1].Agent.Options["backend"] = "exec" },
		"wide profile":  func(c *config.Config) { c.Projects[1].Agent.Options["permission_profile"] = ":workspace" },
		"overlap": func(c *config.Config) {
			c.Projects[1].Agent.Options["work_dir"] = c.Projects[0].Agent.Options["work_dir"]
		},
		"route alias": func(c *config.Config) { c.Projects[1].Platforms[0].Options["allow_chat"] = "private-owner" },
	} {
		t.Run(name, func(t *testing.T) {
			c, h, _ := sharedPartnerFixture(t)
			mutate(c)
			if validateCRMProjectSet(c, h) == nil {
				t.Fatal("unsafe shared configuration accepted")
			}
		})
	}
	for _, uid := range []string{"0", "", "-1", "invalid", userIDForTest()} {
		bad := func(name string) (*user.User, error) { a, _ := lookup(name); a.Uid = uid; return a, nil }
		if validatePartnerRuntimeAccounts(cfg, bad) == nil {
			t.Fatalf("unsafe owner UID %q accepted", uid)
		}
	}
}

func userIDForTest() string {
	u, e := user.Current()
	if e != nil {
		return "0"
	}
	return u.Uid
}
