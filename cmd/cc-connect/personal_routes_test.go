package main

import (
	"testing"

	"github.com/chenhg5/cc-connect/actionhost/crmfollowup"
	"github.com/chenhg5/cc-connect/config"
)

func personalPartnerFixture(t *testing.T) (*config.Config, map[string]*crmfollowup.Adapter) {
	t.Helper()
	cfg, hosts, _ := sharedPartnerFixture(t)
	for i := range cfg.Projects {
		o := cfg.Projects[i].Platforms[0].Options
		member := o["allow_from"].(string)
		o["member_id"], o["source_tenant_key"] = member, "fixture-tenant"
		if i%2 == 0 {
			o["app_id"], o["app_secret"], o["allow_from"] = "personal-"+member, "fixture-"+member, "personal-open-"+member
		} else {
			o["legacy_private_chat"], o["legacy_private_redirect"] = "old-private-"+member, "Use your personal assistant."
		}
	}
	return cfg, hosts
}

func TestPersonalAssistantsShareBackendAndRouteMembersAcrossApps(t *testing.T) {
	cfg, hosts := personalPartnerFixture(t)
	if err := validateCRMProjectSet(cfg, hosts); err != nil {
		t.Fatal(err)
	}
	for i, project := range cfg.Projects {
		want := "private"
		if i%2 == 1 {
			want = "shared"
		}
		if got := connectionAudience(project.Platforms[0].Options); got != want {
			t.Fatalf("startup audience = %s, want CRM wire value %s", got, want)
		}
	}
	projects := map[string]bool{}
	for name := range hosts {
		projects[name] = true
	}
	routes, err := reminderRoutes(cfg, projects)
	if err != nil || len(routes) != 4 {
		t.Fatalf("routes: %v %v", routes, err)
	}
	for _, r := range routes {
		if r.PrivateAppID != "personal-"+r.Member || r.PrivateUser != "personal-open-"+r.Member || r.PrivateChat != "private-"+r.Member || r.PrivateProject == "" {
			t.Fatalf("wrong private member destination: %#v", r)
		}
	}
	if routes[1].User == routes[0].User || routes[1].MemberID() != routes[0].MemberID() {
		t.Fatal("physical and member identities conflated")
	}
	// Identical app-local IDs in distinct apps still identify different members.
	cfg.Projects[2].Platforms[0].Options["allow_from"] = cfg.Projects[0].Platforms[0].Options["allow_from"]
	if err := validateCRMProjectSet(cfg, hosts); err != nil {
		t.Fatal("app-local ID collision merged members", err)
	}
}

func TestPersonalAssistantsRejectPartialAndCrossMemberTopology(t *testing.T) {
	for name, mutate := range map[string]func(*config.Config){
		"partial member":            func(c *config.Config) { delete(c.Projects[0].Platforms[0].Options, "member_id") },
		"partial tenant":            func(c *config.Config) { delete(c.Projects[0].Platforms[0].Options, "source_tenant_key") },
		"other tenant":              func(c *config.Config) { c.Projects[0].Platforms[0].Options["source_tenant_key"] = "other" },
		"wrong member":              func(c *config.Config) { c.Projects[0].Platforms[0].Options["member_id"] = "partner" },
		"shared private app":        func(c *config.Config) { c.Projects[2].Platforms[0].Options["app_id"] = "personal-owner" },
		"public credentials differ": func(c *config.Config) { c.Projects[3].Platforms[0].Options["app_secret"] = "different" },
		"public domain differs":     func(c *config.Config) { c.Projects[3].Platforms[0].Options["domain"] = "https://open.larksuite.com" },
		"missing redirect":          func(c *config.Config) { delete(c.Projects[1].Platforms[0].Options, "legacy_private_redirect") },
		"shared legacy chat": func(c *config.Config) {
			c.Projects[3].Platforms[0].Options["legacy_private_chat"] = c.Projects[1].Platforms[0].Options["legacy_private_chat"]
		},
		"extra backend": func(c *config.Config) { c.Projects[0].Agent.Options["shared_app_server"] = false },
		"excess cache":  func(c *config.Config) { c.Projects[0].Agent.Options["image_cache_capacity_mib"] = 129 },
	} {
		t.Run(name, func(t *testing.T) {
			c, h := personalPartnerFixture(t)
			mutate(c)
			if validateCRMProjectSet(c, h) == nil {
				t.Fatal("unsafe personal topology accepted")
			}
		})
	}
}
