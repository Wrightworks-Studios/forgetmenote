package personal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"forgetmenote/internal/automation"
	"forgetmenote/internal/messaging"
	"forgetmenote/internal/messaginghttp"
	"forgetmenote/internal/workspace"
)

func personalQuiet(now time.Time, zone, start, end string) bool {
	if start == "" {
		return false
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return true
	}
	clock := now.In(loc).Format("15:04")
	if start == end {
		return true
	}
	if start < end {
		return clock >= start && clock < end
	}
	return clock >= start || clock < end
}
func (s *Service) notifyPersonal(ctx context.Context, r *automation.Run, state *State, persist func() error) error {
	settings, err := s.catalog.Assistant(r.Spec.Actor)
	if err != nil {
		return err
	}
	_, _, chatEnabled := s.messaging.MessageClient("slack")
	team, slack, members, mounted := s.hooks.Slack()
	if !chatEnabled || !settings.Notifications || !mounted || s.hooks.PersonalConnections() == nil || personalQuiet(time.Now(), settings.TimeZone, settings.QuietStart, settings.QuietEnd) {
		return nil
	}
	connections, err := s.hooks.PersonalConnections().List(r.Spec.Personal.Account)
	if err != nil {
		return err
	}
	user := ""
	generation := uint64(0)
	for _, c := range connections {
		if c.Provider == "slack" && c.State == "connected" && c.Workspace == team {
			user = c.Subject
			generation = c.Generation
		}
	}
	if user == "" {
		return nil
	}
	scope := automation.Hash([]string{"personal-notification", r.Spec.Personal.Account})
	effects, err := s.automations.Queue.Effects(scope)
	if err != nil {
		return err
	}
	changed := false
	for _, effect := range effects {
		if effect.State != "accepted" {
			continue
		}
		var ack struct {
			Items []string `json:"items"`
		}
		if json.Unmarshal(effect.Result, &ack) != nil {
			continue
		}
		for _, id := range ack.Items {
			key, reminder := strings.CutSuffix(id, ":due")
			d, ok := state.Drafts[key]
			if !ok {
				continue
			}
			if reminder && !d.ReminderNotified {
				d.ReminderNotified = true
				changed = true
			}
			if !reminder && !d.Notified {
				d.Notified = true
				changed = true
			}
			state.Drafts[key] = d
		}
	}
	if changed {
		if err = persist(); err != nil {
			return err
		}
	}
	ids := []string{}
	for id, draft := range state.Drafts {
		if draft.State == "ready" && (!draft.Notified || draft.Due != "" && !draft.ReminderNotified) {
			if err = s.hooks.Current(ctx, draft.Source); err != nil {
				if errors.Is(err, ErrSourceChanged) {
					supersedePersonal(state, draft.Source)
					if err = persist(); err != nil {
						return err
					}
					continue
				}
				return err
			}
		}
		if draft.State == "ready" && draft.NoteRevision != "" && !draft.Notified {
			ids = append(ids, id)
		}
		if draft.State == "ready" && draft.Notified && !draft.ReminderNotified && draft.Due != "" && (draft.Source.Kind == "calendar" || slices.Contains(settings.Features, "followups")) {
			if due, e := time.Parse(time.RFC3339, draft.Due); e == nil && !time.Now().Before(due) {
				ids = append(ids, id+":due")
			}
		}
	}
	sort.Strings(ids)
	if len(ids) == 0 {
		return nil
	}
	// Give the employee an actionable title and a link to their private draft.
	// Full message bodies stay in the authenticated notebook or original app.
	var message strings.Builder
	fmt.Fprintf(&message, "You have %d assistant suggestions or due reminders.\n", len(ids))
	for _, id := range ids {
		key, reminder := strings.CutSuffix(id, ":due")
		draft := state.Drafts[key]
		label := "Suggestion"
		if reminder {
			label = "Reminder"
		} else if draft.MailboxID != "" {
			label = "Gmail draft ready"
		}
		fmt.Fprintf(&message, "• %s: %s\n%s\n", label, strings.ReplaceAll(draft.Title, "\n", " "), assistantNoteURL(s.hooks.Origin(), r.Spec.Project, draft.Path))
	}
	fmt.Fprintf(&message, "Assistant: %s/account/assistant", s.hooks.Origin())
	text := message.String()
	address := messaging.Address{Client: "slack", Workspace: team, User: user, Channel: user, Direct: true}
	if err = members.VerifyMember(ctx, address); err != nil {
		return err
	}
	current, _ := s.catalog.Assistant(r.Spec.Actor)
	if !current.Notifications || current.Revision != settings.Revision {
		return workspace.ErrForbidden
	}
	if err = s.hooks.CheckAutomationAuth(ctx); err != nil {
		return err
	}
	if err = s.hooks.PersonalConnections().Check(r.Spec.Personal.Account, "slack", generation); err != nil {
		return err
	}
	// Sent now or by an earlier run, the items are notified.
	sent, err := s.messaging.SendOnce(ctx, automation.Effect{Scope: scope, Action: automation.Hash(ids), Actor: r.Spec.Actor}, slack, address, messaging.Reply{Text: text}, messaginghttp.DMSendTimeout, map[string]any{"items": ids})
	if err != nil && !sent {
		return err
	}
	for _, id := range ids {
		key, reminder := strings.CutSuffix(id, ":due")
		d := state.Drafts[key]
		if reminder {
			d.ReminderNotified = true
		} else {
			d.Notified = true
		}
		state.Drafts[key] = d
	}
	return errors.Join(err, persist())
}
