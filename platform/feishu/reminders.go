package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/chenhg5/cc-connect/core"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

// SendReminder only uses this platform's fixed destination. It never starts
// another receiver, invokes an agent, or falls back to a different chat.
func (p *Platform) SendReminder(ctx context.Context, chatID, text, id string) (string, error) {
	if !p.strictRoutes || chatID == "" || chatID != p.allowChat || id == "" || strings.ContainsAny(p.allowChat, "*, \t\n") {
		return "", core.ErrReminderPermission
	}
	content, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return "", errors.New("invalid reminder text")
	}
	return p.sendFixedNotification(ctx, chatID, larkim.MsgTypeText, string(content), id)
}

func (p *Platform) HandoffPrincipal() core.ActionPrincipal {
	if !p.strictRoutes || p.memberID == "" || p.legacyPrivateChat != "" {
		return core.ActionPrincipal{}
	}
	return core.ActionPrincipal{Platform: p.platformName, UserID: p.allowFrom, ChatID: p.allowChat, SessionKey: p.sessionKeyFromTrustedCardAction(p.allowChat, p.allowFrom)}
}

func (p *Platform) HandoffReplyContext(principal core.ActionPrincipal) (any, error) {
	expected := p.HandoffPrincipal()
	if expected.UserID == "" || principal.Platform != expected.Platform || principal.UserID != expected.UserID || principal.ChatID != expected.ChatID || principal.SessionKey != expected.SessionKey || !fileRouteID(principal.MessageID, 256) {
		return nil, core.ErrFileNotSubmitted
	}
	return replyContext{chatID: expected.ChatID, sessionKey: expected.SessionKey, messageID: principal.MessageID}, nil
}

func (p *Platform) SendHandoffCard(ctx context.Context, principal core.ActionPrincipal, card *core.Card, id string) (string, error) {
	expected := p.HandoffPrincipal()
	if expected.UserID == "" || principal.Platform != expected.Platform || principal.UserID != expected.UserID || principal.ChatID != expected.ChatID || principal.SessionKey != expected.SessionKey || id == "" || card == nil {
		return "", core.ErrReminderPermission
	}
	if err := validateCardSize(card, expected.SessionKey); err != nil {
		return "", err
	}
	content := renderCard(card, expected.SessionKey)
	return p.sendFixedNotification(ctx, expected.ChatID, larkim.MsgTypeInteractive, content, id)
}

func (p *Platform) sendFixedNotification(ctx context.Context, chatID, kind, content, id string) (string, error) {
	req := larkim.NewCreateMessageReqBuilder().ReceiveIdType(larkim.ReceiveIdTypeChatId).Body(larkim.NewCreateMessageReqBodyBuilder().ReceiveId(chatID).MsgType(kind).Content(content).Uuid(id).Build()).Build()
	resp, err := p.client.Im.Message.Create(ctx, req)
	if err != nil {
		return "", errors.New("reminder acceptance unconfirmed")
	}
	if !resp.Success() {
		// Permission failures are terminal. Do not log API messages, which can
		// contain destination details. Other errors use the durable backoff.
		message := strings.ToLower(resp.Msg)
		if resp.StatusCode == 403 || strings.Contains(message, "permission") || strings.Contains(message, "stopped receiving") || strings.Contains(message, "forbidden") {
			return "", core.ErrReminderPermission
		}
		switch resp.Code {
		case 99991672, 99991679, 230002, 230006, 230013, 230027:
			return "", core.ErrReminderPermission
		}
		return "", errors.New("reminder acceptance unconfirmed")
	}
	if resp.Data == nil || resp.Data.MessageId == nil || *resp.Data.MessageId == "" {
		return "", errors.New("reminder receipt missing")
	}
	return *resp.Data.MessageId, nil
}
