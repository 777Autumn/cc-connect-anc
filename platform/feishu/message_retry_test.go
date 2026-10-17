package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
	lark "github.com/larksuite/oapi-sdk-go/v3"
)

type savedRetryMessage struct {
	UUID    string `json:"uuid"`
	Type    string `json:"msg_type"`
	Content string `json:"content"`
	ID      string
	Path    string
}

// This fixture models the documented UUID window, not an undocumented promise
// about the response shape of a real duplicate request.
type retryMessageLedger struct {
	mu         sync.Mutex
	saved      []savedRetryMessage
	requests   []savedRetryMessage
	deadlines  []time.Time
	mode       string
	cancel     context.CancelFunc
	tokenCalls int
}

func (l *retryMessageLedger) snapshot() ([]savedRetryMessage, []savedRetryMessage) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]savedRetryMessage(nil), l.saved...), append([]savedRetryMessage(nil), l.requests...)
}

func retryMessageFixture(t *testing.T) (*Platform, *retryMessageLedger) {
	t.Helper()
	ledger := &retryMessageLedger{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "tenant_access_token") {
			ledger.mu.Lock()
			ledger.tokenCalls++
			calls, mode := ledger.tokenCalls, ledger.mode
			ledger.mu.Unlock()
			if calls > 1 && mode == "refresh_failure" {
				writeJSON(t, w, map[string]any{"code": 99991661, "msg": "fixture refresh rejected"})
				return
			}
			token := "fixture-token"
			if calls > 1 {
				token = "fresh-token"
			}
			writeJSON(t, w, map[string]any{"code": 0, "expire": 7200, "tenant_access_token": token})
			return
		}
		if r.URL.Path == "/open-apis/im/v1/images" || r.URL.Path == "/open-apis/im/v1/files" {
			writeJSON(t, w, map[string]any{"code": 0, "data": map[string]any{"image_key": "fixture-image", "file_key": "fixture-file"}})
			return
		}
		if r.URL.Path != "/open-apis/im/v1/messages" && !strings.HasSuffix(r.URL.Path, "/reply") {
			t.Errorf("unexpected API %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var message savedRetryMessage
		if err := json.NewDecoder(r.Body).Decode(&message); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		message.Path = r.URL.Path
		ledger.mu.Lock()
		ledger.requests = append(ledger.requests, message)
		mode := ledger.mode
		code := 0
		if mode == "reject" || (mode == "repeat_reject" && len(ledger.saved) > 0) {
			code = 230020
		}
		if (mode == "token_refresh" || mode == "refresh_failure") && len(ledger.saved) > 0 && r.Header.Get("Authorization") != "Bearer fresh-token" {
			code = 99991663
		}
		if code != 0 {
			ledger.mu.Unlock()
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(t, w, map[string]any{"code": code, "msg": "fixture rejected"})
			return
		}
		duplicate := false
		for _, saved := range ledger.saved {
			if message.UUID != "" && message.UUID == saved.UUID {
				message.ID = saved.ID
				duplicate = true
				break
			}
		}
		if message.ID == "" {
			message.ID = fmt.Sprintf("saved-%d", len(ledger.saved)+1)
			ledger.saved = append(ledger.saved, message)
		}
		ledger.mu.Unlock()
		if mode == "wait_response" {
			<-r.Context().Done()
			return
		}
		if mode == "server_failure" {
			w.WriteHeader(http.StatusInternalServerError)
			writeJSON(t, w, map[string]any{"code": 230024, "msg": "fixture server failure"})
			return
		}
		if mode == "bad_response" {
			_, _ = io.WriteString(w, `{"code":0`)
			return
		}
		if duplicate && mode == "repeat_inflight" {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(t, w, map[string]any{"code": 230049, "msg": "The message is being sent."})
			return
		}
		if mode == "missing_receipt" {
			writeJSON(t, w, map[string]any{"code": 0, "data": map[string]any{}})
			return
		}
		writeJSON(t, w, map[string]any{"code": 0, "data": map[string]any{"message_id": message.ID}})
	}))
	t.Cleanup(srv.Close)
	client := srv.Client()
	base := client.Transport
	var mu sync.Mutex
	lost := false
	client.Transport = imageRoundTrip(func(r *http.Request) (*http.Response, error) {
		messageRequest := r.URL.Path == "/open-apis/im/v1/messages" || strings.HasSuffix(r.URL.Path, "/reply")
		if messageRequest {
			deadline, _ := r.Context().Deadline()
			ledger.mu.Lock()
			ledger.deadlines = append(ledger.deadlines, deadline)
			ledger.mu.Unlock()
			mu.Lock()
			failBefore := ledger.mode == "before_once" && !lost
			if failBefore {
				lost = true
			}
			mu.Unlock()
			if failBefore {
				return nil, io.ErrUnexpectedEOF
			}
		}
		resp, err := base.RoundTrip(r)
		if err == nil && messageRequest && resp.StatusCode == http.StatusOK {
			mu.Lock()
			drop := ledger.mode != "bad_response" && (!lost || ledger.mode == "drop_all")
			lost = true
			mu.Unlock()
			if drop {
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				if ledger.cancel != nil {
					ledger.cancel()
				}
				return nil, io.ErrUnexpectedEOF
			}
		}
		return resp, err
	})
	return &Platform{
		platformName: "feishu", appID: t.Name(), appSecret: "synthetic-secret", domain: srv.URL,
		client:       lark.NewClient(t.Name(), "synthetic-secret", lark.WithOpenBaseUrl(srv.URL), lark.WithHttpClient(client)),
		replayClient: lark.NewClient(t.Name(), "synthetic-secret", lark.WithEnableTokenCache(false), lark.WithOpenBaseUrl(srv.URL), lark.WithHttpClient(client)),
	}, ledger
}

func TestMessageRetryServerFailureRetainsDiagnostics(t *testing.T) {
	for _, route := range []string{"reply", "create", "preview_reply", "preview_create"} {
		t.Run(route, func(t *testing.T) {
			p, ledger := retryMessageFixture(t)
			ledger.mode = "server_failure"
			id, err := retrySendWithReceipt(p, context.Background(), route)
			if id != "" || !errors.Is(err, core.ErrMessageDeliveryUnconfirmed) {
				t.Fatalf("receipt=%q err=%v", id, err)
			}
			for _, detail := range []string{"status=500", "code=230024", "msg=fixture server failure"} {
				if !strings.Contains(err.Error(), detail) {
					t.Errorf("error lost %q: %v", detail, err)
				}
			}
			saved, requests := ledger.snapshot()
			if len(saved) != 1 || len(requests) != 1 {
				t.Fatalf("saved=%+v requests=%+v", saved, requests)
			}
		})
	}
}

func TestMessageRetryRecoveryAndIndependentIdenticalSends(t *testing.T) {
	for _, route := range []string{"reply", "create"} {
		for _, mode := range []string{"before_once", "token_refresh", "sequential", "concurrent"} {
			t.Run(route+"/"+mode, func(t *testing.T) {
				p, ledger := retryMessageFixture(t)
				ledger.mode = mode
				type result struct {
					id  string
					err error
				}
				results := make(chan result, 2)
				send := func() { id, err := retrySendWithReceipt(p, context.Background(), route); results <- result{id, err} }
				want := 1
				if mode == "concurrent" {
					want = 2
					go send()
					go send()
				} else {
					send()
					if mode == "sequential" {
						want = 2
						send()
					}
				}
				ids := map[string]bool{}
				for range want {
					result := <-results
					if result.err != nil || result.id == "" {
						t.Fatalf("send=%+v", result)
					}
					ids[result.id] = true
				}
				saved, requests := ledger.snapshot()
				if len(saved) != want || len(ids) != want {
					t.Fatalf("saved=%+v receipts=%v want=%d", saved, ids, want)
				}
				seen := map[string]bool{}
				for _, record := range saved {
					if !ids[record.ID] || record.UUID == "" || seen[record.UUID] {
						t.Fatalf("independent identity lost: %+v", saved)
					}
					seen[record.UUID] = true
				}
				for _, request := range requests {
					if !seen[request.UUID] {
						t.Fatalf("retry UUID changed: %+v", requests)
					}
				}
				if mode == "token_refresh" && ledger.tokenCalls < 2 {
					t.Fatal("token refresh not exercised")
				}
			})
		}
	}
}

func TestMessageRetryPrecancelDeadlineAndExplicitRejection(t *testing.T) {
	for _, route := range []string{"reply", "create", "preview_reply", "preview_create"} {
		for _, mode := range []string{"precancel", "wait_response", "reject", "server_failure", "bad_response"} {
			t.Run(route+"/"+mode, func(t *testing.T) {
				p, ledger := retryMessageFixture(t)
				ledger.mode = mode
				ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
				defer cancel()
				if mode == "precancel" {
					cancel()
				}
				start := time.Now()
				id, err := retrySendWithReceipt(p, ctx, route)
				saved, requests := ledger.snapshot()
				if err == nil || id != "" {
					t.Fatalf("unconfirmed send accepted: id=%q err=%v", id, err)
				}
				switch mode {
				case "precancel":
					if len(saved) != 0 || len(requests) != 0 || !errors.Is(err, context.Canceled) || errors.Is(err, core.ErrMessageDeliveryUnconfirmed) {
						t.Fatalf("pre-cancel submitted: saved=%v err=%v", saved, err)
					}
				case "reject":
					if len(saved) != 0 || len(requests) != 1 || errors.Is(err, core.ErrMessageDeliveryUnconfirmed) || !strings.Contains(err.Error(), "230020") {
						t.Fatalf("rejection changed: saved=%v requests=%v err=%v", saved, requests, err)
					}
				default:
					if len(saved) != 1 || !errors.Is(err, core.ErrMessageDeliveryUnconfirmed) {
						t.Fatalf("lost unknown: saved=%v err=%v", saved, err)
					}
					if mode == "wait_response" && (!errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second) {
						t.Fatalf("deadline lost: elapsed=%v err=%v", time.Since(start), err)
					}
				}
			})
		}
	}
}

func TestMessageRetryPublicCallersAndReceiptAssociation(t *testing.T) {
	for _, create := range []bool{false, true} {
		for _, caller := range []string{"reply", "send", "footer", "card", "image", "file"} {
			t.Run(fmt.Sprintf("%s/create=%v", caller, create), func(t *testing.T) {
				p, ledger := retryMessageFixture(t)
				p.noReplyToTrigger, p.strictRoutes, p.allowFrom, p.allowChat = create, true, "fixture-user", "fixture-chat"
				p.sharedGroup = &sharedWSGroup{platforms: []*Platform{p}}
				rc := replyContext{messageID: "fixture-parent", chatID: p.allowChat, sessionKey: "feishu:fixture-chat:fixture-user", controlledFileWork: true}
				if err := p.SetFileWorkEnabled(true, func(core.Message, string) error { return nil }); err != nil {
					t.Fatal(err)
				}
				var receipts []string
				p.SetFileWorkReplyObserver(func(source core.Message, id string) error {
					if source.MessageID != rc.messageID || source.SessionKey != rc.sessionKey || source.UserID != p.allowFrom {
						t.Error("receipt rebound to wrong source")
					}
					receipts = append(receipts, id)
					return nil
				})
				var err error
				switch caller {
				case "reply":
					err = p.Reply(context.Background(), rc, "same answer")
				case "send":
					err = p.Send(context.Background(), rc, "same answer")
				case "footer":
					err = p.SendWithStatusFooter(context.Background(), rc, "same answer", "status")
				case "card":
					err = (&interactivePlatform{Platform: p}).ReplyCard(context.Background(), rc, core.NewCard().Markdown("same answer").Build())
				case "image":
					err = p.SendImage(context.Background(), rc, core.ImageAttachment{Data: []byte("synthetic-image")})
				case "file":
					err = p.SendFile(context.Background(), rc, core.FileAttachment{Data: []byte("synthetic-file"), FileName: "fixture.txt"})
				}
				saved, _ := ledger.snapshot()
				if err != nil || len(saved) != 1 {
					t.Fatalf("saved=%+v err=%v", saved, err)
				}
				// Legacy standalone media does not register a file-work association.
				wantReceipt := !(create && (caller == "image" || caller == "file"))
				if wantReceipt && (len(receipts) != 1 || receipts[0] != saved[0].ID) {
					t.Fatalf("receipts=%v saved=%+v", receipts, saved)
				}
			})
		}
	}
}

func TestMessageRetryPreservesExplicitUUID(t *testing.T) {
	p, ledger := retryMessageFixture(t)
	id := hostedNoticeUUID("fixture-parent")
	receipt, err := p.replyMessageResultWithUUID(context.Background(), replyContext{messageID: "fixture-parent", chatID: "fixture-chat"}, "text", `{"text":"notice"}`, true, id)
	saved, requests := ledger.snapshot()
	if err != nil || len(saved) != 1 || saved[0].ID != receipt || saved[0].UUID != id {
		t.Fatalf("saved=%+v receipt=%q err=%v", saved, receipt, err)
	}
	for _, request := range requests {
		if request.UUID != id {
			t.Fatalf("replaced explicit UUID: %+v", requests)
		}
	}
}

func TestMessageRetryUnknownAcceptanceStaysUnknown(t *testing.T) {
	for _, route := range []string{"reply", "create", "preview_reply", "preview_create"} {
		for _, mode := range []string{"drop_all", "repeat_reject", "refresh_failure", "repeat_inflight", "missing_receipt", "cancel"} {
			t.Run(route+"/"+mode, func(t *testing.T) {
				p, ledger := retryMessageFixture(t)
				ledger.mode = mode
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if mode == "cancel" {
					ledger.cancel = cancel
				}
				receipt, err := retrySendWithReceipt(p, ctx, route)
				saved, _ := ledger.snapshot()
				if len(saved) != 1 || receipt != "" || !errors.Is(err, core.ErrMessageDeliveryUnconfirmed) || !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Fatalf("saved=%+v receipt=%q err=%v; want one message with sticky unknown and original EOF", saved, receipt, err)
				}
				if mode == "cancel" && !errors.Is(err, context.Canceled) {
					t.Fatalf("lost cancellation cause: %v", err)
				}
			})
		}
	}
}

func retrySendWithReceipt(p *Platform, ctx context.Context, route string) (string, error) {
	rc := replyContext{messageID: "fixture-parent", chatID: "fixture-chat"}
	switch route {
	case "reply":
		return p.replyMessageWithID(ctx, rc, "text", `{"text":"same answer"}`)
	case "create":
		return p.createMessageWithID(ctx, rc.chatID, "text", `{"text":"same answer"}`, "send")
	default:
		p.useInteractiveCard, p.noReplyToTrigger = true, route == "preview_create"
		handle, err := p.SendPreviewStart(ctx, rc, "same answer")
		if err != nil {
			return "", err
		}
		return handle.(*feishuPreviewHandle).messageID, nil
	}
}

func TestMessageRetryKeepsFixedDeadline(t *testing.T) {
	for _, route := range []string{"reply", "create", "preview_reply", "preview_create"} {
		for _, earlier := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/earlier=%v", route, earlier), func(t *testing.T) {
				p, ledger := retryMessageFixture(t)
				ctx := context.Background()
				start := time.Now()
				if earlier {
					var cancel context.CancelFunc
					ctx, cancel = context.WithDeadline(ctx, start.Add(10*time.Second))
					defer cancel()
				}
				if _, err := retrySendWithReceipt(p, ctx, route); err != nil {
					t.Fatal(err)
				}
				ledger.mu.Lock()
				defer ledger.mu.Unlock()
				if len(ledger.deadlines) < 2 {
					t.Fatal("retry did not occur")
				}
				for _, deadline := range ledger.deadlines {
					if !deadline.Equal(ledger.deadlines[0]) {
						t.Fatal("retry reset the deadline")
					}
					if earlier {
						if !deadline.Equal(start.Add(10 * time.Second)) {
							t.Fatal("caller deadline changed")
						}
					} else if deadline.Before(start.Add(55*time.Minute)) || deadline.After(start.Add(55*time.Minute+time.Second)) {
						t.Fatalf("missing fixed 55-minute budget: %s", deadline)
					}
				}
			})
		}
	}
}

func TestMessageRetryLostAcceptedResponseSavesOneMessage(t *testing.T) {
	for _, create := range []bool{false, true} {
		t.Run(map[bool]string{false: "reply", true: "create"}[create], func(t *testing.T) {
			p, ledger := retryMessageFixture(t)
			var receipt string
			var err error
			if create {
				receipt, err = p.createMessageWithID(context.Background(), "fixture-chat", "text", `{"text":"same answer"}`, "send")
			} else {
				receipt, err = p.replyMessageWithID(context.Background(), replyContext{messageID: "fixture-parent", chatID: "fixture-chat"}, "text", `{"text":"same answer"}`)
			}
			saved, requests := ledger.snapshot()
			if err != nil || len(saved) != 1 || receipt != saved[0].ID {
				t.Fatalf("saved=%+v receipt=%q err=%v; want one accepted message and its receipt", saved, receipt, err)
			}
			if len(requests) < 2 || saved[0].UUID == "" || len(saved[0].UUID) > 50 {
				t.Fatalf("missing valid retry identity: %+v", requests)
			}
			for _, request := range requests {
				if request.UUID != saved[0].UUID || request.Content != saved[0].Content {
					t.Fatalf("retry changed identity or content: %+v", requests)
				}
			}
		})
	}
}

func TestMessageRetryPreviewLostAcceptedResponseSavesOneMessage(t *testing.T) {
	for _, create := range []bool{false, true} {
		t.Run(map[bool]string{false: "reply", true: "create"}[create], func(t *testing.T) {
			p, ledger := retryMessageFixture(t)
			p.useInteractiveCard, p.noReplyToTrigger = true, create
			handle, err := p.SendPreviewStart(context.Background(), replyContext{messageID: "fixture-parent", chatID: "fixture-chat"}, "same answer")
			saved, _ := ledger.snapshot()
			if err != nil || len(saved) != 1 {
				t.Fatalf("saved=%+v handle=%v err=%v; want one accepted preview", saved, handle, err)
			}
			if preview, ok := handle.(*feishuPreviewHandle); !ok || preview.messageID != saved[0].ID {
				t.Fatalf("preview receipt=%v does not identify saved=%+v", handle, saved)
			}
		})
	}
}
