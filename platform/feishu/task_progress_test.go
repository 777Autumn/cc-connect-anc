package feishu

import (
	"context"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func TestTaskProgressSingleAttemptAfterLostCreateResponse(t *testing.T) {
	for _, create := range []bool{false, true} {
		for _, outcome := range []string{"lost_response", "server_failure", "missing_receipt", "success"} {
			t.Run(outcome+map[bool]string{false: "/reply", true: "/create"}[create], func(t *testing.T) {
				p, observed := fileDeliveryFixture(t, outcome)
				p.useInteractiveCard, p.noReplyToTrigger = true, create
				if err := p.SetFileWorkEnabled(true, func(core.Message, string) error { return nil }); err != nil {
					t.Fatal(err)
				}
				rc := fileDeliverySource()
				rc.controlledFileWork = true
				callbacks := 0
				p.SetFileWorkReplyObserver(func(msg core.Message, receipt string) error {
					callbacks++
					if msg.MessageID != rc.messageID || receipt != "accepted-file" {
						t.Error("wrong original work association")
					}
					return nil
				})
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				handle, err := p.StartTaskProgress(ctx, rc, "任务处理中。")
				observed.mu.Lock()
				calls := len(observed.paths)
				observed.mu.Unlock()
				if calls != 1 {
					t.Fatalf("uncertain create retried: %d API calls", calls)
				}
				if outcome == "success" {
					if err != nil || handle == nil || callbacks != 1 {
						t.Fatalf("success: handle=%v err=%v callbacks=%d", handle, err, callbacks)
					}
				} else if err == nil || handle != nil || callbacks != 0 {
					t.Fatalf("unconfirmed result accepted: %v %v callbacks=%d", handle, err, callbacks)
				}
			})
		}
	}
}

func TestTaskProgressSingleAttemptPatchFailure(t *testing.T) {
	p, observed := fileDeliveryFixture(t, "server_failure")
	p.useInteractiveCard = true
	err := p.UpdateTaskProgress(context.Background(), &feishuPreviewHandle{messageID: "accepted-file"}, "已停止。")
	observed.mu.Lock()
	defer observed.mu.Unlock()
	if err == nil || len(observed.paths) != 1 {
		t.Fatalf("patch retried or accepted failure: calls=%d err=%v", len(observed.paths), err)
	}
}
