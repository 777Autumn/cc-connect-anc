package codex

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

type nativeThread struct {
	ID        string `json:"id"`
	Cwd       string `json:"cwd"`
	Preview   string `json:"preview"`
	UpdatedAt int64  `json:"updatedAt"`
	Turns     []struct {
		StartedAt int64  `json:"startedAt"`
		ItemsView string `json:"itemsView"`
		Items     []struct {
			Type    string `json:"type"`
			Text    string `json:"text"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"items"`
	} `json:"turns"`
}

func (a *Agent) nativeSessionAccess() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.spawnOpts.RunAsUser != "" || (a.backend == "app_server" && a.appServerURL == "stdio://")
}

func (a *Agent) withSessionAccess(ctx context.Context, fn func(codexRPC, string) error) error {
	a.mu.RLock()
	bin, args, work, home, spawn := a.cmd, append([]string(nil), a.cliExtraArgs...), a.workDir, a.codexHome, a.spawnOpts
	// Session capabilities are for model turns, never needed for history reads.
	env := append([]string(nil), a.configEnv...)
	a.mu.RUnlock()
	if home != "" {
		env = append(env, "CODEX_HOME="+home)
	}
	if spawn.RunAsUser != "" {
		spawn.EnvAllowlist = append(append([]string(nil), spawn.EnvAllowlist...), "CODEX_HOME")
	}
	work, err := filepath.Abs(work)
	if err != nil {
		return err
	}
	return withCodexAppServer(ctx, bin, args, work, env, spawn, func(call codexRPC) error { return fn(call, work) })
}

func (a *Agent) listNativeSessions(ctx context.Context) ([]core.AgentSessionInfo, error) {
	var sessions []core.AgentSessionInfo
	err := a.withSessionAccess(ctx, func(call codexRPC, work string) error {
		params := map[string]any{"cwd": work, "limit": 100, "sortKey": "updated_at", "modelProviders": []string{}, "sourceKinds": []string{"cli", "vscode", "exec", "appServer"}}
		seen := map[string]bool{}
		for {
			var response struct {
				Data       []nativeThread `json:"data"`
				NextCursor *string        `json:"nextCursor"`
			}
			if err := call("thread/list", params, &response); err != nil {
				return err
			}
			for _, thread := range response.Data {
				if thread.ID == "" || filepath.Clean(thread.Cwd) != work {
					return fmt.Errorf("codex: native session outside configured workspace")
				}
				summary := []rune(strings.TrimSpace(thread.Preview))
				if len(summary) > 60 {
					summary = append(summary[:60], []rune("...")...)
				}
				sessions = append(sessions, core.AgentSessionInfo{ID: thread.ID, Summary: string(summary), ModifiedAt: time.Unix(thread.UpdatedAt, 0)})
			}
			if response.NextCursor == nil {
				return nil
			}
			cursor := *response.NextCursor
			if cursor == "" || seen[cursor] {
				return fmt.Errorf("codex: invalid session pagination cursor")
			}
			seen[cursor] = true
			params["cursor"] = cursor
		}
	})
	if err != nil {
		return nil, err
	}
	return sessions, nil
}

func readNativeThread(call codexRPC, work, id string, includeTurns bool) (nativeThread, error) {
	var response struct {
		Thread nativeThread `json:"thread"`
	}
	if strings.TrimSpace(id) == "" {
		return response.Thread, fmt.Errorf("codex: session ID is required")
	}
	err := call("thread/read", map[string]any{"threadId": id, "includeTurns": includeTurns}, &response)
	if err != nil {
		return response.Thread, err
	}
	if response.Thread.ID != id || filepath.Clean(response.Thread.Cwd) != work {
		return nativeThread{}, fmt.Errorf("codex: session outside configured workspace")
	}
	return response.Thread, nil
}

func (a *Agent) nativeSessionHistory(ctx context.Context, id string, limit int) ([]core.HistoryEntry, error) {
	var entries []core.HistoryEntry
	err := a.withSessionAccess(ctx, func(call codexRPC, work string) error {
		// Check scope before requesting transcript contents.
		if _, err := readNativeThread(call, work, id, false); err != nil {
			return err
		}
		thread, err := readNativeThread(call, work, id, true)
		if err != nil {
			return err
		}
		for _, turn := range thread.Turns {
			if turn.ItemsView != "" && turn.ItemsView != "full" {
				return fmt.Errorf("codex: incomplete native session history")
			}
			for _, item := range turn.Items {
				switch item.Type {
				case "userMessage":
					for _, content := range item.Content {
						if content.Type == "text" && isUserPrompt(content.Text) {
							entries = append(entries, core.HistoryEntry{Role: "user", Content: content.Text, Timestamp: time.Unix(turn.StartedAt, 0)})
						}
					}
				case "agentMessage":
					if item.Text != "" {
						entries = append(entries, core.HistoryEntry{Role: "assistant", Content: item.Text, Timestamp: time.Unix(turn.StartedAt, 0)})
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if limit > 0 && len(entries) > limit {
		entries = entries[len(entries)-limit:]
	}
	return entries, nil
}

func (a *Agent) deleteNativeSession(ctx context.Context, id string) error {
	return a.withSessionAccess(ctx, func(call codexRPC, work string) error {
		if _, err := readNativeThread(call, work, id, false); err != nil {
			return err
		}
		// Native delete removes the session; archive would silently change the
		// existing delete contract. Unsupported servers must report the error.
		return call("thread/delete", map[string]any{"threadId": id}, nil)
	})
}
