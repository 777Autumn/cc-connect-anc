package main

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"testing"

	"github.com/chenhg5/cc-connect/actionhost/crmfollowup"
	"github.com/chenhg5/cc-connect/config"
)

func sharedPartnerFixture(t *testing.T) (*config.Config, map[string]*crmfollowup.Adapter, func(string) (*user.User, error)) {
	return sharedPartnerFixtureForActors(t, 2)
}

func sharedPartnerFixtureForActors(t *testing.T, count int) (*config.Config, map[string]*crmfollowup.Adapter, func(string) (*user.User, error)) {
	t.Helper()
	cfg, hosts := partnerConfigFixtureForActors(t, count)
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

func TestSharedPartnerStartupTwoToFiveMembers(t *testing.T) {
	for count := 2; count <= 5; count++ {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			cfg, hosts, lookup := sharedPartnerFixtureForActors(t, count)
			if err := validateCRMProjectSet(cfg, hosts); err != nil {
				t.Fatal(err)
			}
			if err := validatePartnerRuntimeAccounts(cfg, lookup); err != nil {
				t.Fatal(err)
			}
			registered := map[string]bool{}
			for name := range hosts {
				registered[name] = true
			}
			routes, err := reminderRoutes(cfg, registered)
			if err != nil || len(routes) != 2*count {
				t.Fatalf("reminder routes: %v %v", routes, err)
			}
			for _, route := range routes {
				if route.PrivateChat != "private-"+route.User {
					t.Fatal("cross-person reminder route", route)
				}
			}
			cfg.Projects[2*count-1].Platforms[0].Options["allow_from"] = "owner"
			if validateCRMProjectSet(cfg, hosts) == nil {
				t.Fatal("uneven identity mapping accepted")
			}
		})
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
