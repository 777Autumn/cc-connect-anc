package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// The file host is an external boundary in core. Its publication authorization
// and activation reauthorization are covered by actionhost/files tests.
type slowHandoffFiles struct {
	fileHostStub
	mu        sync.Mutex
	delay     time.Duration
	inputs    []FileWorkInput
	entered   chan struct{}
	cancelled chan struct{}
}

func (h *slowHandoffFiles) Tool(ctx context.Context, command string, input json.RawMessage, _ ActionPrincipal, work string) (map[string]any, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if command == "host-case-import" {
		var request struct {
			PublicationID string `json:"publication_id"`
		}
		if err := json.Unmarshal(input, &request); err != nil {
			return nil, err
		}
		if len(h.inputs) == 1 {
			if h.entered != nil {
				close(h.entered)
			}
			timer := time.NewTimer(h.delay)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				if h.cancelled != nil {
					close(h.cancelled)
				}
				return nil, ctx.Err()
			case <-timer.C:
			}
		}
		index := len(h.inputs)
		h.inputs = append(h.inputs, FileWorkInput{Name: fmt.Sprintf("material-%d.pdf", index), SHA256: strings.Repeat("a", 64),
			Source: &FileWorkSource{PublicationID: request.PublicationID, Purpose: "current", PublicationBasisID: "chg_fixture"}})
	}
	return publicationResult(FileWorkContext{Enabled: true, WorkID: work, Inputs: append([]FileWorkInput(nil), h.inputs...)}), nil
}

func handoffTimeoutResult(count int) map[string]any {
	files := make([]any, count)
	for i := range files {
		files[i] = map[string]any{"publication_id": fmt.Sprintf("bh_chg_fixture_%d", i), "name": fmt.Sprintf("material-%d.pdf", i),
			"sha256": strings.Repeat("a", 64), "purpose": "current", "reference_only": false, "publication_basis_id": "chg_fixture"}
	}
	return map[string]any{"status": "found", "handoff": map[string]any{"files": files}}
}

func actionToolSocket(t *testing.T, network string, handler http.Handler, writeTimeout time.Duration) (*ActionToolServer, *http.Client, string) {
	t.Helper()
	var server *ActionToolServer
	var err error
	transport := &http.Transport{Proxy: nil}
	address := ""
	if network == "unix" {
		if runtime.GOOS == "windows" {
			t.Skip("protected Unix-domain socket is verified on Unix")
		}
		// Keep the pathname below the Unix socket length limit, including on Mac.
		dir, mkdirErr := os.MkdirTemp("", "handoff-")
		if mkdirErr != nil {
			t.Fatal(mkdirErr)
		}
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
		address = filepath.Join(dir, "tool.sock")
		server, err = ListenActionToolsUnix(address, handler)
		socketPath := address
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
		}
	} else {
		server, err = ListenActionTools("127.0.0.1:0", handler)
		if err == nil {
			address = server.listener.Addr().String()
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	if writeTimeout > 0 {
		server.server.WriteTimeout = writeTimeout
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve() }()
	t.Cleanup(func() {
		transport.CloseIdleConnections()
		_ = server.Close()
		<-done
	})
	if network == "unix" {
		address = "localhost"
	}
	return server, &http.Client{Transport: transport, Timeout: 55 * time.Second}, "http://" + address + "/tool"
}

func socketTool(ctx context.Context, client *http.Client, url, token, command string) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(`{"command":"`+command+`","input":{}}`))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	response, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	body, err := io.ReadAll(response.Body)
	if closeErr := response.Body.Close(); err == nil {
		err = closeErr
	}
	return response.StatusCode, string(body), err
}

func TestHandoffTwoFilesSurviveToolAndSocketDeadlines(t *testing.T) {
	for _, network := range []string{"tcp", "unix"} {
		t.Run(network, func(t *testing.T) {
			t.Parallel()
			e, host, _, state := actionToolFixture(t)
			t.Cleanup(e.cancel)
			host.toolResult = handoffTimeoutResult(2)
			state.fileWorkID = "fixture-work"
			// Exceed the old tool (30s), TCP (35s), and Unix (40s) deadlines.
			e.fileWorkHost = &slowHandoffFiles{delay: 41 * time.Second}
			_, client, url := actionToolSocket(t, network, e.ActionToolHandler(), 0)
			status, body, err := socketTool(context.Background(), client, url, state.actionToken, "handoff")
			if err != nil || status != http.StatusOK || !strings.Contains(body, `"materials_imported":true`) || !strings.Contains(body, "material-1.pdf") {
				t.Fatalf("second material unavailable after slow import: status=%d body=%s error=%v", status, body, err)
			}
		})
	}
}

func TestHandoffSlowImportStillCancels(t *testing.T) {
	for _, stop := range []string{"request", "engine", "listener"} {
		t.Run(stop, func(t *testing.T) {
			e, host, _, state := actionToolFixture(t)
			t.Cleanup(e.cancel)
			host.toolResult = handoffTimeoutResult(2)
			state.fileWorkID = "fixture-work"
			files := &slowHandoffFiles{delay: time.Hour, entered: make(chan struct{}), cancelled: make(chan struct{})}
			e.fileWorkHost = files
			server, client, url := actionToolSocket(t, "tcp", e.ActionToolHandler(), 0)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan struct{})
			go func() { defer close(done); _, _, _ = socketTool(ctx, client, url, state.actionToken, "handoff") }()
			select {
			case <-files.entered:
			case <-time.After(3 * time.Second):
				t.Fatal("second import did not start")
			}
			switch stop {
			case "request":
				cancel()
			case "engine":
				_ = e.Stop()
			case "listener":
				_ = server.Close()
			}
			select {
			case <-files.cancelled:
			case <-time.After(3 * time.Second):
				t.Fatal("shutdown/cancellation left import running")
			}
			<-done
		})
	}
}

type deadlineToolRecorder struct {
	*httptest.ResponseRecorder
	calls int
	err   error
}

func (w *deadlineToolRecorder) SetWriteDeadline(time.Time) error {
	w.calls++
	return w.err
}

func TestHandoffWriteDeadlineRequiresAuthenticatedSupportedTransport(t *testing.T) {
	for _, mode := range []string{"handoff", "ordinary", "invalid-token", "invalid-input", "deadline-error", "unsupported"} {
		t.Run(mode, func(t *testing.T) {
			e, host, _, _ := actionToolFixture(t)
			t.Cleanup(e.cancel)
			token, body := "test-session-token", `{"command":"handoff","input":{}}`
			wantStatus, wantHost, wantDeadline := 200, 1, 1
			switch mode {
			case "ordinary":
				body, wantDeadline = `{"command":"customer","input":{}}`, 0
			case "invalid-token":
				token, wantStatus, wantHost, wantDeadline = "wrong", 401, 0, 0
			case "invalid-input":
				body, wantStatus, wantHost, wantDeadline = `{"command":"handoff","input":{},"actor":"forged"}`, 400, 0, 0
			case "deadline-error", "unsupported":
				wantStatus, wantHost = 503, 0
			}
			w := &deadlineToolRecorder{ResponseRecorder: httptest.NewRecorder()}
			if mode == "deadline-error" {
				w.err = errors.New("deadline not writable")
			}
			r, _ := http.NewRequest("POST", "/tool", strings.NewReader(body))
			r.Header.Set("Authorization", "Bearer "+token)
			var writer http.ResponseWriter = w
			if mode == "unsupported" {
				writer, wantDeadline = w.ResponseRecorder, 0
			}
			e.ActionToolHandler().ServeHTTP(writer, r)
			if w.Code != wantStatus || host.toolCalls != wantHost || w.calls != wantDeadline {
				t.Fatalf("status=%d host=%d deadline=%d body=%s", w.Code, host.toolCalls, w.calls, w.Body.String())
			}
		})
	}
}

func TestHandoffImportRetainsFourFileLimit(t *testing.T) {
	for _, count := range []int{0, 4, 5} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			e, host, _, state := actionToolFixture(t)
			t.Cleanup(e.cancel)
			host.toolResult = handoffTimeoutResult(count)
			state.fileWorkID = "fixture-work"
			e.fileWorkHost = &slowHandoffFiles{}
			_, client, url := actionToolSocket(t, "tcp", e.ActionToolHandler(), 0)
			status, body, err := socketTool(context.Background(), client, url, state.actionToken, "handoff")
			if err != nil || (status == http.StatusOK) != (count == 4) {
				t.Fatalf("file limit changed: status=%d body=%s error=%v", status, body, err)
			}
		})
	}
}

type deadlineBudgetHost struct {
	actionToolHostStub
	budgets chan time.Duration
}

func (h *deadlineBudgetHost) Tool(ctx context.Context, command string, _ json.RawMessage, _ ActionPrincipal, _ string, _ Language) (map[string]any, *ActionHostResult, error) {
	deadline, _ := ctx.Deadline()
	h.budgets <- time.Until(deadline)
	if command == "customer" {
		select {
		case <-time.After(150 * time.Millisecond):
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	}
	return map[string]any{"status": "ok"}, nil, nil
}

func TestHandoffDoesNotExtendFollowingOrdinaryRequest(t *testing.T) {
	e, _, _, state := actionToolFixture(t)
	t.Cleanup(e.cancel)
	host := &deadlineBudgetHost{budgets: make(chan time.Duration, 2)}
	e.SetActionHost(host)
	_, client, url := actionToolSocket(t, "tcp", e.ActionToolHandler(), 50*time.Millisecond)
	status, body, err := socketTool(context.Background(), client, url, state.actionToken, "handoff")
	if err != nil || status != http.StatusOK {
		t.Fatalf("handoff unavailable: %d %s %v", status, body, err)
	}
	if budget := <-host.budgets; budget < 120*time.Second || budget > 150*time.Second {
		t.Fatalf("handoff processing budget = %s", budget)
	}
	// Reuse the keepalive transport. An ordinary response must still hit its
	// listener deadline; the preceding handoff must not lengthen this request.
	if _, _, err := socketTool(context.Background(), client, url, state.actionToken, "customer"); err == nil {
		t.Fatal("ordinary tool inherited the handoff socket deadline")
	}
	if budget := <-host.budgets; budget < 25*time.Second || budget > 30*time.Second {
		t.Fatalf("ordinary processing budget changed: %s", budget)
	}
}
