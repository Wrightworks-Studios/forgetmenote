package web

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"forgetmenote/internal/automation"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/messaging"
)

// emailPairing returns the messaging client an email-pairing task pairs members
// for and the transport that lists them. The task runs as the service account
// the client admits workspace members as, with the domains named beside it, so
// the administrator who configured the client decides who may be paired.
func (s *Server) emailPairing(spec automation.Spec) (messaging.Client, messaging.MemberLister, error) {
	c, transport, ok := s.messaging.MessageClient(spec.Native.Client)
	if !ok {
		return c, nil, errors.New("the messaging client is not configured or is disabled")
	}
	if c.Members == nil || c.Members.Actor != spec.Actor || len(c.Members.PairByEmail) == 0 {
		return c, nil, errors.New("email pairing runs as the client's members actor and needs members.pairByEmail")
	}
	lister, ok := transport.(messaging.MemberLister)
	if !ok {
		return c, nil, errors.New("this messaging client cannot list its members")
	}
	return c, lister, nil
}

// runEmailPairing pairs the client's members with accounts by verified email.
// A dry run reports what a run would change. The report names members and
// accounts by ID and never carries an address.
func (s *Server) runEmailPairing(ctx context.Context, r *automation.Run) (map[string]any, automation.Incident, error) {
	incident := automation.Incident{Severity: "warning"}
	c, lister, err := s.emailPairing(r.Spec)
	if err != nil {
		return nil, incident, err
	}
	members, err := lister.Members(ctx)
	if err != nil {
		return nil, incident, err
	}
	mapped := map[string]bool{}
	for user := range c.Users {
		mapped[user] = true
	}
	if !r.DryRun {
		if err = s.checkAutomationAuth(ctx); err != nil {
			return nil, incident, err
		}
	}
	p, err := s.catalog.PairMessageUsersByEmail(r.Spec.Actor, r.Spec.Native.Client, c.Workspace, c.Members.PairByEmail, members, mapped, r.DryRun)
	if err != nil {
		return nil, incident, err
	}
	// Keep the first pairing of a large workspace within the report limit.
	const shown = 200
	data := map[string]any{"members": p.Members, "paired": len(p.Paired), "unpaired": len(p.Unpaired), "current": p.Current, "declined": p.Declined, "conflicts": len(p.Conflicts)}
	data["pairedMembers"] = firstMessageLinks(p.Paired, shown)
	data["unpairedMembers"] = firstMessageLinks(p.Unpaired, shown)
	data["conflictMembers"] = p.Conflicts[:min(len(p.Conflicts), shown)]
	incident.Active = len(p.Conflicts) > 0
	incident.Summary = fmt.Sprintf("Email pairing for %s: %d members need review", r.Spec.Native.Client, len(p.Conflicts))
	return data, incident, nil
}

func firstMessageLinks(links []catalog.MessageLink, n int) []map[string]string {
	out := []map[string]string{}
	for _, l := range links[:min(len(links), n)] {
		out = append(out, map[string]string{"user": l.User, "account": l.Account})
	}
	return out
}

// observeMemberChange queues each email-pairing job for the client when a
// member joined or changed in a way that could change their pairing. A
// redelivered event queues nothing more, and the job's own schedule catches up
// on anything an event missed.
func (s *Server) observeMemberChange(client, eventID string, member messaging.Member, active bool) {
	if eventID == "" || len(eventID) > 128 {
		return
	}
	for _, spec := range s.automations.Definitions.Specs() {
		if spec.Disabled || spec.Native == nil || spec.Native.Task != "email-pairing" || spec.Native.Client != client || s.automationPolicy(spec) != nil {
			continue
		}
		c, _, err := s.emailPairing(spec)
		if err != nil || !s.catalog.MessagePairingAffected(client, c.Workspace, c.Members.PairByEmail, member, active) {
			continue
		}
		if _, err = s.automations.Queue.Enqueue(spec, "member-event:"+eventID); err != nil {
			slog.Warn("email pairing not queued for a member event", "job", spec.ID, "error", err)
		}
	}
}
