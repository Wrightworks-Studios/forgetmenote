package automationrun

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"forgetmenote/internal/automation"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/connectors"
	"forgetmenote/internal/workspace"
)

// PinPublication publishes to path only while it still holds old: a note
// changed, or created, after old was read wins over the run's output.
func PinPublication(path string, old workspace.Document) *automation.Publication {
	p := &automation.Publication{Path: path, Revision: old.Revision}
	if old.Revision != "" {
		p.Hash = workspace.ContentHash(old.Content)
	}
	return p
}

// Publish writes a run's publication to its output note, or for a
// preview only shows it. The run keeps the publication as its result only once
// its final check passes: a run refused there keeps no result, including a
// report set before, so it shows nothing its actor may no longer publish.
func (s *Service) Publish(ctx context.Context, r *automation.Run) error {
	if e := s.hooks.CheckEvidence(ctx, r, true); e != nil {
		r.Result = ""
		return e
	}
	if r.DryRun {
		r.Result = r.Publication.Content
		return nil
	}
	a, ok := automation.IdentityFrom(ctx)
	if !ok {
		return workspace.ErrForbidden
	}
	p := r.Publication
	r.Result = p.Content
	id := "automation-" + r.ID
	if p.LearningKey != "" {
		id += "-" + p.LearningKey
	}
	return s.catalog.WithProjectAuthority(a.Spec.Actor, a.Spec.Project, a.Account, "edit", a.Epoch, a.ProjectEpoch, func(store *workspace.Store) error {
		_, e := store.ApplyGroup(ctx, workspace.EditGroup{ID: id, Summary: "Automation: " + r.Spec.ID, Edits: []workspace.NoteEdit{{Path: p.Path, Content: p.Content, Revision: p.Revision, Hash: p.Hash}}})
		return e
	})
}

// Notify delivers a run's answer or publication to its Slack channel or
// direct message, after rechecking the run and, for a direct message, its
// recipient.
func (s *Service) Notify(ctx context.Context, r *automation.Run, save func() error) error {
	if r.DryRun || r.Answer == "" && r.Publication == nil {
		return nil
	}
	if r.Spec.NotifySlackUser == "" && r.Spec.NotifyChannel == "" {
		return nil
	}
	if !s.hooks.SlackConfigured() {
		return errors.New("Slack is not configured")
	}
	if e := s.hooks.CheckEvidence(ctx, r, true); e != nil {
		return e
	}
	if r.Publication != nil {
		r.Answer = r.Publication.Content
		if len(r.Answer) > 9000 {
			r.Answer = "# " + r.Spec.ID + "\n\nSaved " + r.Publication.Path + " in " + r.Spec.Project + ".\n\n" + ReceiptSummary(r.Receipts)
		}
		if e := save(); e != nil {
			return e
		}
	}
	if r.Spec.NotifyChannel != "" {
		return s.hooks.Messaging().DeliverSlack(ctx, r, save, automation.Delivery{Channel: r.Spec.NotifyChannel, Shared: true})
	}
	policy, _, enabled := s.hooks.Messaging().MessageClient("slack")
	if !enabled {
		return workspace.ErrForbidden
	}
	c, ok := policy.DirectMessages[r.Spec.NotifySlackUser]
	actor := policy.Users[r.Spec.NotifySlackUser]
	if !ok || actor == "" || c.Project != r.Spec.Project || !s.catalog.Interactive(actor) {
		return workspace.ErrForbidden
	}
	for _, project := range r.Spec.ReadProjects {
		if project != c.Project && !slices.Contains(c.ReadProjects, project) {
			return workspace.ErrForbidden
		}
	}
	// Recheck the recipient independently: the worker's service account may see
	// more data than the person receiving its message.
	recipient := r.Spec
	recipient.Actor = actor
	recipient.Connections = c.Connections
	recipient.ReadProjects = c.ReadProjects
	recipientCtx, e := s.hooks.RunContext(chat.WithoutRunScope(ctx), recipient)
	if e != nil {
		return e
	}
	recipientCtx, e = s.SourceContext(recipientCtx, &automation.Run{Spec: recipient})
	if e != nil {
		return e
	}
	if e = s.hooks.CheckEvidence(recipientCtx, r, true); e != nil {
		return e
	}
	return s.hooks.Messaging().DeliverSlack(ctx, r, save, automation.Delivery{Channel: r.Spec.NotifySlackUser, SlackUser: r.Spec.NotifySlackUser, Direct: true})
}

// ReceiptSummary lists a run's data receipts as Markdown.
func ReceiptSummary(receipts []connectors.Receipt) string {
	var out strings.Builder
	out.WriteString("Live evidence:\n")
	for _, r := range receipts {
		fmt.Fprintf(&out, "- `%s/%s` at %s; receipt `%s`; complete: %t.\n", r.Connection, r.Operation, r.Time, r.ID, r.Complete)
	}
	return out.String()
}
