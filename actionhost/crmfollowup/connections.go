package crmfollowup

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func (a *Adapter) ConnectionOperation(text string) string {
	switch text {
	case "连接我的飞书", "連接我的飛書", "connect my Feishu":
		return "connect"
	case "重新连接", "重新連接", "reconnect Feishu":
		return "reconnect"
	case "连接状态", "連接狀態", "connection status":
		return "status"
	case "断开连接", "斷開連接", "disconnect Feishu":
		return "disconnect"
	}
	return ""
}

type ConnectionRoute struct {
	Project, AppID, TenantKey, MemberID, UserID, ChatID, Audience string
}

func (a *Adapter) ConfigureConnections(ctx context.Context, route ConnectionRoute) (bool, error) {
	input := map[string]any{"source_app_id": route.AppID}
	if route.MemberID != "" {
		input["project"], input["source_tenant_key"] = route.Project, route.TenantKey
	}
	result, err := a.call(ctx, "host-connection-config", input, "")
	if err != nil {
		return false, err
	}
	if result["status"] == "disabled" {
		if route.MemberID != "" {
			return false, errors.New("personal member mapping requires configured connections")
		}
		return false, nil
	}
	if result["status"] != "enabled" {
		return false, errors.New("personal connection configuration rejected")
	}
	if route.MemberID != "" {
		bindings, ok := result["bindings"].([]any)
		if !ok || len(bindings) != 1 || result["project"] != route.Project || result["source_app_id"] != route.AppID || result["source_tenant_key"] != route.TenantKey {
			return false, errors.New("personal connection source binding rejected")
		}
		binding, ok := bindings[0].(map[string]any)
		if !ok || (route.Audience != "private" && route.Audience != "shared") || binding["audience"] != route.Audience || binding["platform"] != "feishu" || binding["project"] != route.Project || binding["member_id"] != route.MemberID || binding["user_id"] != route.UserID || binding["chat_id"] != route.ChatID {
			return false, errors.New("personal connection member binding rejected")
		}
	}
	users, ok := result["users"].([]any)
	if !ok || len(users) == 0 {
		return false, errors.New("personal connection users missing")
	}
	configured := []string{}
	seen := map[string]bool{}
	for _, raw := range users {
		user, ok := raw.(string)
		if !ok || user == "" || seen[user] {
			return false, errors.New("personal connection user invalid")
		}
		seen[user] = true
		configured = append(configured, user)
	}
	if route.MemberID != "" && !seen[route.MemberID] {
		return false, errors.New("personal connection member maintenance missing")
	}
	a.connectionUsers = configured
	a.connectionMembers = route.MemberID != ""
	return true, nil
}

func (a *Adapter) ConnectionCommand(ctx context.Context, operation string, p core.ActionPrincipal) (map[string]any, error) {
	return a.call(ctx, "host-connection", map[string]any{"command": operation, "principal": p}, "")
}

func (a *Adapter) ConnectionGuard(ctx context.Context, command string, p core.ActionPrincipal, token, workID, subject string) error {
	result, err := a.call(ctx, "host-connection-guard", map[string]any{"command": command, "principal": p, "work_id": workID, "knowledge_subject": subject}, token)
	if err != nil {
		if ctx.Err() != nil {
			return core.ErrConnectionTemporary
		}
		return err
	}
	if result["status"] == "blocked" && result["code"] == "connection_temporarily_unavailable" {
		return core.ErrConnectionTemporary
	}
	if result["status"] != "allowed" {
		return errors.New("personal connection work scope rejected")
	}
	return nil
}

// RunConnections is a non-model maintenance loop. The shared Python registry
// decides which identities are due and locks each one across host processes.
func (a *Adapter) RunConnections(ctx context.Context, changed func(string)) {
	timer := time.NewTicker(time.Minute)
	defer timer.Stop()
	for {
		for _, user := range a.connectionUsers {
			callCtx, cancel := context.WithTimeout(ctx, 50*time.Second)
			key := "user_id"
			if a.connectionMembers {
				key = "member_id"
			}
			result, err := a.call(callCtx, "host-connection-tick", map[string]any{key: user}, "")
			cancel()
			if err != nil && ctx.Err() == nil {
				slog.Warn("personal connection maintenance unavailable")
			} else if changes, ok := result["changes"].([]any); ok {
				for _, raw := range changes {
					if change, ok := raw.(map[string]any); ok {
						if user, ok := change[key].(string); ok && changed != nil {
							changed(user)
						}
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
	}
}
