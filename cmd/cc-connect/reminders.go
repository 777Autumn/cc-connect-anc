package main

import (
	"context"
	"errors"
	"github.com/chenhg5/cc-connect/actionhost/opsalerts"
	"github.com/chenhg5/cc-connect/actionhost/reminders"
	"github.com/chenhg5/cc-connect/config"
	"github.com/chenhg5/cc-connect/core"
)

type authorizedReminderSender struct {
	engine *core.Engine
	user   string
	sender core.ReminderSender
}

// An administrator selects the already verified Owner private project, never an
// arbitrary recipient. Frozen batch bindings also include the sender and chat.
func operationsRecipient(project string, routes []reminders.Route, senders map[string]reminders.Sender) func() opsalerts.Recipient {
	var chosen reminders.Route
	matches := 0
	for _, r := range routes {
		if r.Project == project && r.Chat == r.PrivateChat {
			chosen = r
			matches++
		}
	}
	return func() opsalerts.Recipient {
		if matches != 1 {
			return opsalerts.Recipient{}
		}
		sender, ok := senders[chosen.MemberID()].(authorizedReminderSender)
		if !ok {
			return opsalerts.Recipient{}
		}
		binding := chosen.Project + "\x00" + chosen.User + "\x00" + chosen.PrivateChat
		if chosen.Member != "" {
			binding += "\x00" + chosen.Member + "\x00" + chosen.AppID
		}
		result := opsalerts.Recipient{Binding: opsalerts.Scope(binding), Chat: chosen.PrivateChat}
		if sender.ReminderAuthorized() {
			result.Sender = sender
		}
		return result
	}
}

func (s authorizedReminderSender) ReminderAuthorized() bool {
	return s.engine.HostedUserAuthorized(s.user)
}
func (s authorizedReminderSender) SendReminder(ctx context.Context, chat, text, id string) (string, error) {
	if !s.ReminderAuthorized() {
		return "", core.ErrReminderPermission
	}
	return s.sender.SendReminder(ctx, chat, text, id)
}

// The CRM project-set validator has already checked exact member identities,
// two fixed environments per member, private chats and a shared group. This only
// derives the private mapping; no second identity configuration is introduced.
func reminderRoutes(cfg *config.Config, projects map[string]bool) ([]reminders.Route, error) {
	if len(projects) < 4 || len(projects) > 10 || len(projects)%2 != 0 {
		return nil, errors.New("reminders require paired verified environments for two to five members")
	}
	counts := map[string]int{}
	for _, p := range cfg.Projects {
		if projects[p.Name] {
			counts[fixedChatKey(p.Platforms[0].Options)]++
		}
	}
	private := map[string]reminders.Route{}
	for _, p := range cfg.Projects {
		if projects[p.Name] {
			o := p.Platforms[0].Options
			chat, _ := o["allow_chat"].(string)
			user, _ := o["allow_from"].(string)
			member := routeMember(o)
			if counts[fixedChatKey(o)] == 1 {
				if private[member].Project != "" {
					return nil, errors.New("ambiguous private route")
				}
				app, _ := o["app_id"].(string)
				private[member] = reminders.Route{User: user, Chat: chat, Project: p.Name, AppID: app}
			}
		}
	}
	if len(private) != len(projects)/2 {
		return nil, errors.New("missing private route")
	}
	result := []reminders.Route{}
	for _, p := range cfg.Projects {
		if projects[p.Name] {
			o := p.Platforms[0].Options
			chat, _ := o["allow_chat"].(string)
			user, _ := o["allow_from"].(string)
			target := private[routeMember(o)]
			if target.Project == "" {
				return nil, errors.New("missing private route")
			}
			member, _ := o["member_id"].(string)
			app, _ := o["app_id"].(string)
			route := reminders.Route{User: user, Chat: chat, Project: p.Name, PrivateChat: target.Chat, Member: member, AppID: app}
			if member != "" {
				route.PrivateProject, route.PrivateUser, route.PrivateAppID = target.Project, target.User, target.AppID
			}
			result = append(result, route)
		}
	}
	if len(result) != len(projects) {
		return nil, errors.New("missing configured reminder environment")
	}
	return result, nil
}
