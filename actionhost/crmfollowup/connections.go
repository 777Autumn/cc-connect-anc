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

func (a *Adapter) ConfigureConnections(ctx context.Context, sourceAppID string) (bool, error) {
	result, err := a.call(ctx, "host-connection-config", map[string]any{"source_app_id": sourceAppID}, "")
	if err != nil {
		return false, err
	}
	if result["status"] == "disabled" {
		return false, nil
	}
	if result["status"] != "enabled" {
		return false, errors.New("personal connection configuration rejected")
	}
	users, ok := result["users"].([]any)
	if !ok || len(users) == 0 {
		return false, errors.New("personal connection users missing")
	}
	for _, raw := range users {
		user, ok := raw.(string)
		if !ok || user == "" {
			return false, errors.New("personal connection user invalid")
		}
		a.connectionUsers = append(a.connectionUsers, user)
	}
	return true, nil
}

func (a *Adapter) ConnectionCommand(ctx context.Context, operation string, p core.ActionPrincipal) (map[string]any, error) {
	return a.call(ctx, "host-connection", map[string]any{"command": operation, "principal": p}, "")
}

func (a *Adapter) ConnectionGuard(ctx context.Context, command string, p core.ActionPrincipal, token, workID, subject string) error {
	result, err := a.call(ctx, "host-connection-guard", map[string]any{"command": command, "principal": p, "work_id": workID, "knowledge_subject": subject}, token)
	if err != nil {
		return err
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
			result, err := a.call(callCtx, "host-connection-tick", map[string]any{"user_id": user}, "")
			cancel()
			if err != nil && ctx.Err() == nil {
				slog.Warn("personal connection maintenance unavailable")
			} else if changes, ok := result["changes"].([]any); ok {
				for _, raw := range changes {
					if change, ok := raw.(map[string]any); ok {
						if user, ok := change["user_id"].(string); ok && changed != nil {
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
