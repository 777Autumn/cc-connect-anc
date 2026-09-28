package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// Synthetic native protocol: the gateway never receives a transcript path.
func TestCodexSessionCLIHelper(t *testing.T) {
	if !slices.Contains(os.Args, "anc-session-cli") {
		return
	}
	cwd := os.Getenv("ANC_SESSION_CWD")
	if expected := os.Getenv("ANC_SESSION_UID"); expected != "" {
		actual, err := user.Current()
		if err != nil || actual.Username != expected || actual.Uid == "0" {
			os.Exit(8)
		}
		if home := os.Getenv("CODEX_HOME"); home == "" || home != os.Getenv("ANC_SESSION_HOME") {
			os.Exit(10)
		}
		if err := os.WriteFile(filepath.Join(os.Getenv("CODEX_HOME"), "synthetic-session-access"), []byte("model-only"), 0600); err != nil {
			os.Exit(9)
		}
	}
	scanner := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var req struct {
			ID     any            `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if json.Unmarshal(scanner.Bytes(), &req) != nil {
			os.Exit(2)
		}
		if req.ID == nil {
			continue
		}
		thread := map[string]any{"id": "own", "cwd": cwd, "preview": "synthetic prompt", "updatedAt": 1700000000}
		result := any(map[string]any{})
		var failure any
		switch req.Method {
		case "thread/list":
			if os.Getenv("ANC_SESSION_HANG") != "" {
				time.Sleep(time.Minute)
			}
			if req.Params["cwd"] != cwd {
				os.Exit(3)
			}
			if os.Getenv("ANC_SESSION_ERROR") != "" {
				failure = map[string]any{"code": -32000, "message": "synthetic permission denied"}
				break
			}
			if req.Params["cursor"] == nil {
				result = map[string]any{"data": []any{thread}, "nextCursor": "page2"}
			} else {
				result = map[string]any{"data": []any{}, "nextCursor": nil}
			}
		case "thread/read":
			id, _ := req.Params["threadId"].(string)
			thread["id"] = id
			if id == "foreign" {
				thread["cwd"] = filepath.Join(cwd, "other")
			}
			if req.Params["includeTurns"] == true {
				if id != "own" {
					os.Exit(4)
				}
				thread["turns"] = []any{map[string]any{"startedAt": 1700000000, "itemsView": "full", "items": []any{
					map[string]any{"type": "userMessage", "content": []any{map[string]any{"type": "text", "text": "first"}}},
					map[string]any{"type": "reasoning", "text": "hidden"},
					map[string]any{"type": "agentMessage", "text": "second"},
				}}}
			}
			result = map[string]any{"thread": thread}
		case "thread/delete":
			if req.Params["threadId"] != "own" {
				os.Exit(5)
			}
			if err := os.WriteFile(os.Getenv("ANC_SESSION_DELETE"), []byte("own"), 0600); err != nil {
				os.Exit(6)
			}
		}
		response := map[string]any{"id": req.ID, "result": result}
		if failure != nil {
			delete(response, "result")
			response["error"] = failure
		}
		if encoder.Encode(response) != nil {
			os.Exit(7)
		}
	}
	os.Exit(0)
}

func TestCodexNativeSessionAccess(t *testing.T) {
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	deleted := filepath.Join(work, "deleted")
	a := &Agent{cmd: bin, cliExtraArgs: []string{"-test.run=^TestCodexSessionCLIHelper$", "--", "anc-session-cli"},
		workDir: work, codexHome: filepath.Join(work, "no-local-transcripts"), backend: "app_server", appServerURL: "stdio://",
		configEnv: []string{"ANC_SESSION_CWD=" + work, "ANC_SESSION_DELETE=" + deleted}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sessions, err := a.ListSessions(ctx)
	if err != nil || len(sessions) != 1 || sessions[0].ID != "own" {
		t.Fatalf("native list: %+v / %v", sessions, err)
	}
	history, err := a.GetSessionHistory(ctx, "own", 1)
	if err != nil || len(history) != 1 || history[0].Content != "second" {
		t.Fatalf("native history: %+v / %v", history, err)
	}
	if _, err := a.GetSessionHistory(ctx, "foreign", 0); err == nil {
		t.Fatal("foreign history accepted")
	}
	if err := a.DeleteSession(ctx, "foreign"); err == nil {
		t.Fatal("foreign deletion accepted")
	}
	if _, err := os.Stat(deleted); !os.IsNotExist(err) {
		t.Fatal("rejected deletion reached CLI")
	}
	if err := a.DeleteSession(ctx, "own"); err != nil {
		t.Fatal(err)
	}
	if content, err := os.ReadFile(deleted); err != nil || string(content) != "own" {
		t.Fatal("native deletion not used", err)
	}
	a.configEnv = append(a.configEnv, "ANC_SESSION_ERROR=1")
	if sessions, err := a.ListSessions(ctx); err == nil || len(sessions) != 0 {
		t.Fatal("permission failure disguised as empty list")
	}
	a.configEnv = append(a.configEnv, "ANC_SESSION_HANG=1")
	short, cancelShort := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancelShort()
	if _, err := a.ListSessions(short); err == nil {
		t.Fatal("native read timeout ignored")
	}
}
