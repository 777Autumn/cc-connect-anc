package main

import (
	"errors"
	"strings"

	"github.com/chenhg5/cc-connect/config"
)

func routeMember(options map[string]any) string {
	if member, _ := options["member_id"].(string); member != "" {
		return member
	}
	user, _ := options["allow_from"].(string)
	return user
}

func connectionAudience(options map[string]any) string {
	if legacy, _ := options["legacy_private_chat"].(string); legacy != "" {
		return "shared"
	}
	return "private"
}

func personalRoutesConfigured(cfg *config.Config) bool {
	for _, p := range cfg.Projects {
		for _, platform := range p.Platforms {
			for _, key := range []string{"member_id", "source_tenant_key", "legacy_private_chat", "legacy_private_redirect"} {
				if _, exists := platform.Options[key]; exists {
					return true
				}
			}
		}
	}
	return false
}

func fixedChatKey(options map[string]any) string {
	app, _ := options["app_id"].(string)
	tenant, _ := options["source_tenant_key"].(string)
	chat, _ := options["allow_chat"].(string)
	return app + "\x00" + tenant + "\x00" + chat
}

// Each member has one personal app and one route through the common group app.
func validatePersonalRoutes(cfg *config.Config, sharedOwner string) error {
	if sharedOwner == "" || len(cfg.Projects) < 4 || len(cfg.Projects) > 10 || len(cfg.Projects)%2 != 0 {
		return errors.New("personal assistants require one shared Codex backend and paired member routes")
	}
	counts := map[string]int{}
	for _, p := range cfg.Projects {
		if len(p.Platforms) != 1 {
			return errors.New("personal assistants require one fixed platform per project")
		}
		counts[fixedChatKey(p.Platforms[0].Options)]++
	}
	private, groups, apps := map[string]bool{}, map[string]bool{}, map[string]bool{}
	legacyChats := map[string]bool{}
	sharedChat, groupApp, tenant := "", "", ""
	for _, p := range cfg.Projects {
		o := p.Platforms[0].Options
		member, _ := o["member_id"].(string)
		sourceTenant, _ := o["source_tenant_key"].(string)
		app, _ := o["app_id"].(string)
		for _, value := range []string{member, sourceTenant} {
			if value == "" || strings.ContainsAny(value, "*, \t\r\n\x00") {
				return errors.New("personal routes require exact member_id and source_tenant_key")
			}
		}
		if tenant != "" && tenant != sourceTenant {
			return errors.New("personal routes must belong to the same tenant")
		}
		tenant = sourceTenant
		chat := fixedChatKey(o)
		if counts[chat] == 1 {
			if private[member] || apps[app] {
				return errors.New("each member requires one distinct personal app")
			}
			private[member], apps[app] = true, true
			if _, exists := o["legacy_private_chat"]; exists {
				return errors.New("legacy private redirect belongs only to the public app")
			}
			if _, exists := o["legacy_private_redirect"]; exists {
				return errors.New("legacy private redirect belongs only to the public app")
			}
		} else {
			if counts[chat] != len(cfg.Projects)/2 || groups[member] || (sharedChat != "" && sharedChat != chat) {
				return errors.New("members require one common public group route each")
			}
			groups[member], sharedChat, groupApp = true, chat, app
			legacy, _ := o["legacy_private_chat"].(string)
			redirect, _ := o["legacy_private_redirect"].(string)
			if legacy == "" || legacyChats[legacy] || strings.ContainsAny(legacy, "*, \t\r\n\x00") || strings.TrimSpace(redirect) == "" || legacy == o["allow_chat"] {
				return errors.New("public routes require an exact legacy private chat and fixed redirect")
			}
			legacyChats[legacy] = true
		}
	}
	if len(private) != len(cfg.Projects)/2 || len(groups) != len(private) || apps[groupApp] {
		return errors.New("personal apps and public group app must be distinct")
	}
	for member := range private {
		if !groups[member] {
			return errors.New("missing member public group route")
		}
	}
	return nil
}
