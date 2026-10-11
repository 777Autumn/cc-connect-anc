package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

const handoffStartKind = "banquet.handoff.start.v1"

type HandoffNotificationRoute struct{ AppID, TenantKey, MemberID, UserID, ChatID string }
type HandoffNotificationHost interface {
	HandoffCall(context.Context, string, map[string]any) (map[string]any, error)
}

// PendingActionChecker reads the domain's existing durable approval ledger.
type PendingActionChecker interface {
	PendingActions(context.Context, ActionPrincipal) (bool, error)
}

func pendingHostedActions(ctx context.Context, host ActionHost, principal ActionPrincipal) (bool, error) {
	if pair, ok := host.(*actionHostPair); ok {
		pending, err := pendingHostedActions(ctx, pair.ActionHost, principal)
		if pending || err != nil {
			return pending, err
		}
		return pendingHostedActions(ctx, pair.additional, principal)
	}
	if checker, ok := host.(PendingActionChecker); ok {
		return checker.PendingActions(ctx, principal)
	}
	return false, nil
}

type HandoffNotificationSender interface {
	HandoffPrincipal() ActionPrincipal
	HandoffReplyContext(ActionPrincipal) (any, error)
	SendHandoffCard(context.Context, ActionPrincipal, *Card, string) (string, error)
}
type handoffNotifications struct {
	platform  Platform
	host      HandoffNotificationHost
	route     HandoffNotificationRoute
	principal ActionPrincipal
	once      sync.Once
	wg        sync.WaitGroup
}
type handoffStartRequest struct {
	eventID  string
	accepted bool
}

// Only protected startup wiring can enable notices for a fixed personal route.
func (e *Engine) SetHandoffNotifications(p Platform, host HandoffNotificationHost, route HandoffNotificationRoute) error {
	sender, ok := p.(HandoffNotificationSender)
	_, canEdit := p.(CardMessageRefresher)
	if !ok || !canEdit || host == nil || route.AppID == "" || route.TenantKey == "" || route.MemberID == "" || route.UserID == "" || route.ChatID == "" || e.handoffNotifications != nil {
		return errors.New("invalid personal handoff route")
	}
	present := false
	for _, configured := range e.platforms {
		if configured == p {
			present = true
		}
	}
	principal := sender.HandoffPrincipal()
	if !present || principal.Platform != p.Name() || principal.UserID != route.UserID || principal.ChatID != route.ChatID || principal.SessionKey == "" {
		return errors.New("handoff route mismatch")
	}
	principal.Project = e.name
	e.handoffNotifications = &handoffNotifications{platform: p, host: host, route: route, principal: principal}
	return nil
}

func (e *Engine) startHandoffNotifications() {
	n := e.handoffNotifications
	if n == nil {
		return
	}
	if !e.HostedUserAuthorized(n.principal.UserID) {
		return
	}
	n.once.Do(func() {
		n.wg.Add(1)
		go func() {
			defer n.wg.Done()
			ticker := time.NewTicker(15 * time.Second)
			defer ticker.Stop()
			for {
				if e.ctx.Err() != nil {
					return
				}
				e.pollHandoffNotifications()
				select {
				case <-e.ctx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
	})
}

func (e *Engine) handoffRouteInput() map[string]any {
	n := e.handoffNotifications
	return map[string]any{"project": e.name, "source_app_id": n.route.AppID, "source_tenant_key": n.route.TenantKey}
}

func (e *Engine) validHandoffTarget(event map[string]any) bool {
	n := e.handoffNotifications
	target, ok := event["target"].(map[string]any)
	return ok && target["platform"] == n.principal.Platform && target["project"] == e.name && target["user_id"] == n.route.UserID && target["chat_id"] == n.route.ChatID && target["member_id"] == n.route.MemberID && target["source_app_id"] == n.route.AppID && target["source_tenant_key"] == n.route.TenantKey
}

func (e *Engine) pollHandoffNotifications() {
	n := e.handoffNotifications
	if n == nil {
		return
	}
	ctx, cancel := context.WithTimeout(e.ctx, 30*time.Second)
	defer cancel()
	result, err := n.host.HandoffCall(ctx, "host-handoff-events", e.handoffRouteInput())
	if err != nil || result["status"] != "ok" {
		slog.Warn("handoff notices unavailable", "project", e.name)
		return
	}
	events, _ := result["events"].([]any)
	for _, raw := range events {
		event, ok := raw.(map[string]any)
		if !ok || !e.validHandoffTarget(event) {
			slog.Warn("handoff notice route refused", "project", e.name)
			continue
		}
		eventID, ok := event["event_id"].(string)
		if !ok || eventID == "" {
			continue
		}
		input := e.handoffRouteInput()
		input["event_id"] = eventID
		claimed, err := n.host.HandoffCall(ctx, "host-handoff-notify-claim", input)
		if err != nil {
			slog.Warn("handoff notice claim unconfirmed", "project", e.name)
			continue
		}
		if claimed["status"] != "claimed" {
			continue
		}
		canonical, ok := claimed["event"].(map[string]any)
		dispatchID, _ := claimed["dispatch_id"].(string)
		if !ok || canonical["event_id"] != eventID || !e.validHandoffTarget(canonical) || dispatchID == "" {
			slog.Warn("handoff notice claim refused", "project", e.name)
			continue
		}
		e.sendHandoffNotification(ctx, canonical, dispatchID)
	}
}

func (e *Engine) handoffNoticeCard(event map[string]any, actionable bool) *Card {
	key := MsgHandoffNoticeSubmit
	switch event["kind"] {
	case "accept":
		key = MsgHandoffNoticeAccepted
	case "return":
		key = MsgHandoffNoticeReturned
	}
	b := NewCard().Title(e.i18n.T(MsgHandoffNoticeTitle), "blue").PlainText(e.i18n.T(key))
	if actionable && event["kind"] == "submit" {
		id, _ := event["event_id"].(string)
		b.Buttons(CardButton{Text: e.i18n.T(MsgHandoffStart), Type: "primary", Extra: map[string]string{"kind": handoffStartKind, "approval_id": id, "decision": "start"}})
	}
	return hostedSharedCard(b.Build())
}

func (e *Engine) sendHandoffNotification(ctx context.Context, event map[string]any, dispatchID string) {
	n := e.handoffNotifications
	if !e.HostedUserAuthorized(n.principal.UserID) {
		slog.Warn("handoff notice recipient no longer authorized", "project", e.name)
		return
	}
	id, sendErr := n.platform.(HandoffNotificationSender).SendHandoffCard(ctx, n.principal, e.handoffNoticeCard(event, false), dispatchID)
	status := "sent"
	if sendErr != nil || id == "" {
		status = "unknown"
		id = ""
	}
	ack, err := n.host.HandoffCall(ctx, "host-handoff-notify-ack", map[string]any{"event_id": event["event_id"], "dispatch_id": dispatchID, "status": status, "message_id": id})
	if err != nil || ack["status"] != "recorded" || status != "sent" {
		slog.Warn("handoff notification delivery unconfirmed; use pending handoffs", "project", e.name)
		return
	}
	if event["kind"] == "submit" {
		if err := n.platform.(CardMessageRefresher).RefreshCardMessage(ctx, id, n.principal.SessionKey, e.handoffNoticeCard(event, true)); err != nil {
			slog.Warn("handoff notification buttons unavailable; use pending handoffs", "project", e.name)
		}
	}
}

func (e *Engine) handleHandoffStart(p Platform, action TrustedCardAction) TrustedCardActionResponse {
	n := e.handoffNotifications
	invalid := TrustedCardActionResponse{Toast: e.i18n.T(MsgHandoffUnavailable), ToastType: "error"}
	if n == nil || p != n.platform || action.Decision != "start" || action.ApprovalID == "" {
		return invalid
	}
	if !e.HostedUserAuthorized(action.Principal.UserID) {
		return invalid
	}
	action.Principal.Platform = p.Name()
	action.Principal.Project = e.name
	if !sameFilePrincipal(n.principal, action.Principal) || action.Principal.MessageID == "" {
		return invalid
	}
	rc, err := p.(HandoffNotificationSender).HandoffReplyContext(action.Principal)
	if err != nil {
		return invalid
	}
	request := &handoffStartRequest{eventID: action.ApprovalID}
	msg := &Message{Platform: p.Name(), UserID: action.Principal.UserID, ChannelID: action.Principal.ChatID, SessionKey: action.Principal.SessionKey, MessageID: action.Principal.MessageID, ReplyCtx: rc, ControlledFileWork: true, FileWorkPrivate: true, handoffStart: request}
	// The file intake lock covers idle check, fresh-work creation and durable claim.
	e.handleFileWorkMessage(p, msg)
	if request.accepted {
		return TrustedCardActionResponse{Toast: e.i18n.T(MsgHandoffStarted), ToastType: "info"}
	}
	return TrustedCardActionResponse{Toast: e.i18n.T(MsgHandoffCheckReply), ToastType: "info"}
}

func (e *Engine) prepareHandoffStart(msg *Message) MsgKey {
	n := e.handoffNotifications
	if n == nil {
		return MsgHandoffUnavailable
	}
	ctx, cancel := context.WithTimeout(e.ctx, 30*time.Second)
	defer cancel()
	e.actionMu.RLock()
	host := e.actionHost
	e.actionMu.RUnlock()
	if pending, err := pendingHostedActions(ctx, host, e.actionPrincipalForMessage(msg)); err != nil {
		return MsgHandoffUnavailable
	} else if pending {
		return MsgHandoffBusy
	}
	for _, session := range e.sessions.ListSessions(msg.SessionKey) {
		if q := session.caseQuestion(); q != nil && (q.Status == "executing" || (q.Status == "awaiting" && time.Now().Before(q.Expires))) {
			return MsgHandoffBusy
		}
	}
	result, err := n.host.HandoffCall(ctx, "host-handoff-start", map[string]any{"event_id": msg.handoffStart.eventID, "principal": e.actionPrincipalForMessage(msg)})
	if err != nil {
		return MsgHandoffUnavailable
	}
	if result["status"] == "replayed" {
		return MsgHandoffAlreadyStarted
	}
	if result["status"] == "busy" {
		return MsgHandoffBusy
	}
	if result["status"] != "ready" {
		return MsgHandoffUnavailable
	}
	var check BanquetFileCheck
	raw, err := json.Marshal(result["file_check"])
	if err != nil || json.Unmarshal(raw, &check) != nil || !check.Published || len(check.Files) == 0 || CheckBanquetFiles(e.banquetCheckContext(ctx), check) != nil {
		return MsgHandoffUnavailable
	}
	banquetID, ok := result["banquet_id"].(string)
	if !ok || banquetID == "" {
		return MsgHandoffUnavailable
	}
	encoded, _ := json.Marshal(banquetID)
	version, _ := json.Marshal(result["version"])
	msg.Content = fmt.Sprintf("[Host: the recipient clicked Organize handoff. First read the installed team-crm Skill and its referenced banquet tool protocol. Use the documented crm-tool handoff CLI with banquet_id %s to read the current approved handoff, expected version %s. The CLI need not appear as a named model/MCP tool; an empty tool search does not establish that it is unavailable. After the authorized handoff read imports materials, refresh work-context and read the returned files. Empty work-context before that read does not establish that the publication has no materials. Summarize confirmed facts and missing items. Do not accept or return the handoff until this person explicitly requests and approves the existing action. Never load the author's conversation. If the publication or version changed, stop and ask the person to reopen the current handoff.]", encoded, version)
	return ""
}

func (e *Engine) claimHandoffStart(msg *Message, session *Session, workID string) bool {
	ctx, cancel := context.WithTimeout(e.ctx, 30*time.Second)
	defer cancel()
	result, err := e.handoffNotifications.host.HandoffCall(ctx, "host-handoff-start", map[string]any{"event_id": msg.handoffStart.eventID, "principal": e.actionPrincipalForMessage(msg), "work_id": workID, "session_id": fileNativeSessionID(session), "handoff_files_verified": true})
	return err == nil && result["status"] == "started"
}
