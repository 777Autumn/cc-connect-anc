package core

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"
)

// ConnectionHost owns deterministic enrollment and work audience checks. Model
// tools cannot call enrollment or select the identity used for a business tool.
type ConnectionHost interface {
	ConnectionOperation(string) string
	ConnectionCommand(context.Context, string, ActionPrincipal) (map[string]any, error)
	ConnectionGuard(context.Context, string, ActionPrincipal, string, string, string) error
}

// ErrConnectionTemporary blocks a protected operation without expiring its work.
var ErrConnectionTemporary = errors.New("personal connection temporarily unavailable")

func (e *Engine) SetConnectionHost(host ConnectionHost) { e.connectionHost = host }

func (e *Engine) guardConnectionWork(ctx context.Context, command string, principal ActionPrincipal, token, workID string, input ...json.RawMessage) error {
	if e.connectionHost == nil {
		return nil
	}
	subject := ""
	if len(input) == 1 && strings.HasPrefix(command, "knowledge_") {
		var value struct {
			Subject string `json:"subject"`
		}
		if json.Unmarshal(input[0], &value) != nil {
			return errConnectionWorkUnavailable
		}
		subject = value.Subject
	}
	return e.connectionHost.ConnectionGuard(ctx, command, principal, token, workID, subject)
}

// Check before a native context is loaded or another queued prompt is sent.
func (e *Engine) guardConnectionSession(sessions *SessionManager, session *Session, principal ActionPrincipal, workID string) error {
	if session.connectionExpired() {
		return errConnectionWorkUnavailable
	}
	if workID == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(e.ctx, 15*time.Second)
	defer cancel()
	err := e.guardConnectionWork(ctx, "work-resume", principal, "", workID)
	if err != nil && !errors.Is(err, ErrConnectionTemporary) {
		if saveErr := sessions.expireConnection(session); saveErr != nil {
			slog.Error("expired connection session could not be persisted", "error", saveErr)
		}
	}
	return err
}

func connectionFailureMessage(err error) MsgKey {
	if errors.Is(err, ErrConnectionTemporary) {
		return MsgConnectionUnavailable
	}
	return MsgConnectionWorkExpired
}

func (e *Engine) handleConnectionCommand(p Platform, msg *Message) bool {
	host := e.connectionHost
	if host == nil {
		return false
	}
	operation := host.ConnectionOperation(strings.TrimSpace(msg.Content))
	if operation == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(e.ctx, 60*time.Second)
	defer cancel()
	if operation == "reconnect" || operation == "disconnect" {
		check, err := host.ConnectionCommand(ctx, "check", e.actionPrincipalForMessage(msg))
		if err == nil && check["audience"] == "private" {
			e.cleanupInteractiveState(msg.SessionKey)
			e.sessions.NewSession(msg.SessionKey, "")
		}
	}
	result, err := host.ConnectionCommand(ctx, operation, e.actionPrincipalForMessage(msg))
	if err != nil {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgConnectionUnavailable))
		return true
	}
	if result["audience"] == "private" && result["reset"] == true {
		// New authorization must never resume an old work's model context.
		e.cleanupInteractiveState(msg.SessionKey)
		e.sessions.NewSession(msg.SessionKey, "")
	}
	key := MsgConnectionUnavailable
	switch result["status"] {
	case "pending":
		key = MsgConnectionPending
	case "connected":
		key = MsgConnectionReady
	case "disconnected", "reconnect_required", "wrong_account":
		key = MsgConnectionRequired
	}
	if operation == "disconnect" && result["status"] == "disconnected" {
		key = MsgConnectionDisconnected
	}
	if result["code"] == "connection_private_chat_required" {
		key = MsgConnectionPrivate
	}
	message := e.i18n.T(key)
	if uri, ok := result["verification_url"].(string); ok && result["audience"] == "private" && result["status"] == "pending" {
		message = uri + "\n\n" + message
	}
	e.reply(p, msg.ReplyCtx, message)
	if result["audience"] == "private" && result["status"] == "pending" {
		if encoded, ok := result["qrcode_png"].(string); ok && len(encoded) < 140000 {
			if sender, supported := p.(ImageSender); supported {
				data, err := base64.StdEncoding.DecodeString(encoded)
				if err == nil && len(data) > 8 {
					err = sender.SendImage(ctx, msg.ReplyCtx, ImageAttachment{MimeType: "image/png", Data: data, FileName: "feishu-connection.png"})
				}
				if err != nil {
					slog.Warn("personal connection QR delivery unavailable")
				}
			}
		}
	}
	return true
}

// InvalidateConnection cancels only this person's private model work. Group work
// does not use this person's credentials and continues independently.
func (e *Engine) InvalidateConnection(userID string) {
	type pending struct {
		key   string
		state *interactiveState
	}
	var affected []pending
	e.interactiveMu.Lock()
	for key, state := range e.interactiveStates {
		state.mu.Lock()
		matches := state.currentPrincipal.UserID == userID && state.filePrivate
		state.mu.Unlock()
		if matches {
			affected = append(affected, pending{key, state})
		}
	}
	e.interactiveMu.Unlock()
	for _, item := range affected {
		item.state.mu.Lock()
		principal, workID := item.state.currentPrincipal, item.state.fileWorkID
		session := item.state.fileSession
		platform, reply := item.state.platform, item.state.replyCtx
		item.state.mu.Unlock()
		ctx, cancel := context.WithTimeout(e.ctx, 15*time.Second)
		err := e.guardConnectionWork(ctx, "work-resume", principal, "", workID)
		cancel()
		if errors.Is(err, ErrConnectionTemporary) {
			slog.Warn("private connection verification temporarily unavailable")
			continue
		}
		if err != nil {
			if session != nil {
				if saveErr := e.sessions.expireConnection(session); saveErr != nil {
					slog.Error("expired connection session could not be persisted", "error", saveErr)
				}
			}
			e.cleanupInteractiveState(item.key, item.state)
			if platform != nil {
				e.reply(platform, reply, e.i18n.T(MsgConnectionWorkExpired))
			}
		} else if session != nil {
			item.state.mu.Lock()
			waiting := len(item.state.pendingMessages) > 0 && !item.state.stopped && item.state.agentSession != nil && item.state.agentSession.Alive()
			item.state.mu.Unlock()
			if waiting && session.TryLock() {
				go e.drainOrphanedQueue(session, e.sessions, item.key, nil, "")
			}
		}
	}
}

var errConnectionWorkUnavailable = errors.New("personal connection or work scope unavailable")
