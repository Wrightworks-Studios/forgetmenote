package knowledge

import (
	"context"
	"encoding/json"
	"errors"
	"forgetmenote/internal/attribution"
	"forgetmenote/internal/workspace"
	"maps"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

type Proposal struct {
	ID               string               `json:"id"`
	RequestHash      string               `json:"requestHash"`
	Group            workspace.EditGroup  `json:"group"`
	Author           string               `json:"author"`
	Provenance       attribution.Identity `json:"provenance"`
	Created          string               `json:"created"`
	Status           string               `json:"status"`
	AcceptedBy       string               `json:"acceptedBy,omitempty"`
	AcceptedIdentity attribution.Identity `json:"acceptedIdentity"`
	Revision         string               `json:"revision,omitempty"`
	Updated          string               `json:"updated,omitempty"`
	Stale            bool                 `json:"stale"`
	Before           map[string]string    `json:"before"`
}

var consumerName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,79}$`)

func (e *Engine) putProposal(p Proposal) error {
	next := e.state
	next.Proposals = map[string]Proposal{}
	for id, existing := range e.state.Proposals {
		next.Proposals[id] = existing
	}
	next.Proposals[p.ID] = p
	raw, _ := json.Marshal(next)
	if len(raw) > 32<<20 {
		return errors.New("knowledge state exceeds 32 MiB; archive completed proposals from Review before adding more")
	}
	return e.saveState(context.Background(), next, nil, false)
}
func (e *Engine) Propose(ctx context.Context, actor string, group workspace.EditGroup) (Proposal, error) {
	if err := e.Store.ValidateMutationGroup(ctx, group); err != nil {
		return Proposal{}, err
	}
	raw, _ := json.Marshal(group)
	if len(group.ID) < 1 || len(group.ID) > 128 || strings.TrimSpace(group.Summary) == "" || len(group.Summary) > 300 || len(group.Edits) < 1 || len(group.Edits) > 50 || len(raw) > 4<<20 {
		return Proposal{}, errors.New("proposal needs an ID, summary and 1–50 edits within 4 MiB")
	}
	id := workspace.ContentHash([]byte(actor + "\x00" + group.ID))
	requestHash := workspace.ContentHash(raw)
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.refresh(ctx); err != nil {
		return Proposal{}, err
	}
	old, exists, lookupErr := e.proposal(ctx, id)
	if lookupErr != nil {
		return Proposal{}, lookupErr
	}
	if exists {
		if old.RequestHash != requestHash {
			return Proposal{}, workspace.ErrConflict
		}
		return old, nil
	}
	if len(e.state.Proposals) >= 200 {
		return Proposal{}, errors.New("project proposal limit reached (200)")
	}
	before := map[string]string{}
	for _, edit := range group.Edits {
		if edit.Path == "AGENTS.md" {
			return Proposal{}, workspace.ErrForbidden
		}
		if _, ok := before[edit.Path]; ok {
			return Proposal{}, errors.New("duplicate note path")
		}
		doc, err := e.Store.KnowledgeRead(ctx, edit.Path, edit.Revision)
		if edit.Revision == "" {
			if err == nil || !errors.Is(err, os.ErrNotExist) || edit.Hash != "" || edit.Delete {
				return Proposal{}, workspace.ErrConflict
			}
			before[edit.Path] = ""
		} else {
			if err != nil {
				return Proposal{}, err
			}
			if workspace.ContentHash(doc.Content) != edit.Hash {
				return Proposal{}, workspace.ErrConflict
			}
			before[edit.Path] = string(doc.Content)
		}
	}
	group.ID = "proposal-" + id
	proposal := Proposal{ID: id, RequestHash: requestHash, Group: group, Author: actor, Provenance: attribution.From(ctx), Created: time.Now().UTC().Format(time.RFC3339), Status: "proposed", Before: before}
	return proposal, e.putProposal(proposal)
}
func (e *Engine) reconcile(ctx context.Context, p Proposal) (Proposal, error) {
	if p.Status != "applying" {
		return p, nil
	}
	receipt, err := e.Store.GroupReceipt(ctx, p.Group, p.AcceptedIdentity.ID)
	if err != nil {
		return p, err
	}
	if receipt.Replayed {
		p.Status = "applied"
		p.Revision = receipt.Revision
	} else {
		p.Status = "proposed"
		p.AcceptedBy = ""
		p.AcceptedIdentity = attribution.Identity{}
	}
	return p, e.putProposal(p)
}
func (e *Engine) Decide(ctx context.Context, actor, id, decision string) (Proposal, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.refresh(ctx); err != nil {
		return Proposal{}, err
	}
	p, ok, lookupErr := e.proposal(ctx, id)
	if lookupErr != nil {
		return Proposal{}, lookupErr
	}
	if !ok {
		return Proposal{}, os.ErrNotExist
	}
	var err error
	p, err = e.reconcile(ctx, p)
	if err != nil {
		return p, err
	}
	if decision == "archive" {
		if p.Status != "applied" && p.Status != "dismissed" {
			return p, workspace.ErrConflict
		}
		if _, active := e.state.Proposals[id]; !active {
			return p, nil
		}
		next := e.state
		next.Proposals = maps.Clone(e.state.Proposals)
		delete(next.Proposals, id)
		if err := e.saveState(ctx, next, &p, false); err != nil {
			return p, err
		}
		return p, nil
	}
	if p.Status == "applied" && decision == "accept" {
		return p, nil
	}
	if p.Status != "proposed" {
		return p, workspace.ErrConflict
	}
	if decision == "dismiss" {
		p.Status = "dismissed"
		p.AcceptedBy = actor
		p.Updated = time.Now().UTC().Format(time.RFC3339)
		return p, e.putProposal(p)
	}
	if decision != "accept" {
		return p, errors.New("choose accept or dismiss")
	}
	p.Status = "applying"
	p.AcceptedBy = actor
	p.AcceptedIdentity = attribution.From(ctx)
	p.Updated = time.Now().UTC().Format(time.RFC3339)
	if err = e.putProposal(p); err != nil {
		return p, err
	}
	result, err := e.Store.ApplyGroup(ctx, p.Group)
	if err != nil {
		var persistErr error
		p, persistErr = e.reconcile(context.WithoutCancel(ctx), p)
		return p, errors.Join(err, persistErr)
	}
	p.Status = "applied"
	p.Revision = result.Revision
	return p, e.putProposal(p)
}
func (e *Engine) Proposals(ctx context.Context) ([]Proposal, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.refresh(ctx); err != nil {
		return nil, err
	}
	out := []Proposal{}
	for _, p := range e.state.Proposals {
		var err error
		p, err = e.reconcile(ctx, p)
		if err != nil {
			return nil, err
		}
		allowed := true
		for _, edit := range p.Group.Edits {
			doc, err := e.Store.KnowledgeRead(ctx, edit.Path, "")
			if errors.Is(err, workspace.ErrForbidden) {
				allowed = false
				break
			}
			if edit.Revision == "" {
				p.Stale = p.Stale || err == nil
			} else {
				p.Stale = p.Stale || err != nil || workspace.ContentHash(doc.Content) != edit.Hash
			}
		}
		if p.Status != "proposed" {
			p.Stale = false
		}
		if allowed {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created > out[j].Created })
	return out, nil
}
func (e *Engine) Checkpoint(ctx context.Context, actor, consumer string) (Checkpoint, error) {
	if !consumerName.MatchString(consumer) {
		return Checkpoint{}, errors.New("consumer names use 1–80 letters, numbers, dots, underscores or hyphens")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.refresh(ctx); err != nil {
		return Checkpoint{}, err
	}
	return e.state.Checkpoints[actor+"/"+consumer], nil
}
func (e *Engine) Complete(ctx context.Context, actor, consumer, previous, revision, summary string, reset bool) (Checkpoint, error) {
	if !consumerName.MatchString(consumer) || len(summary) > 1000 {
		return Checkpoint{}, errors.New("invalid consumer name or completion summary")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.refresh(ctx); err != nil {
		return Checkpoint{}, err
	}
	key := actor + "/" + consumer
	old := e.state.Checkpoints[key]
	if old.Revision == revision && old.Summary == summary {
		return old, nil
	}
	if old.Revision != previous {
		return Checkpoint{}, workspace.ErrConflict
	}
	if err := e.Store.AssertSnapshot(ctx, revision); err != nil {
		return Checkpoint{}, err
	}
	if old.Revision != "" && !reset {
		changes, err := e.Store.ChangesSince(ctx, old.Revision, revision)
		if err != nil {
			return Checkpoint{}, err
		}
		if changes.FreshReview {
			return Checkpoint{}, errors.New("checkpoint history changed; explicitly record a fresh review with reset")
		}
	}
	if len(e.state.Checkpoints) >= 1000 && old.Consumer == "" {
		return Checkpoint{}, errors.New("consumer checkpoint limit reached")
	}
	cp := Checkpoint{Consumer: consumer, User: actor, Revision: revision, CompletedAt: time.Now().UTC().Format(time.RFC3339), Summary: summary}
	next := e.state
	next.Checkpoints = map[string]Checkpoint{}
	for k, v := range e.state.Checkpoints {
		next.Checkpoints[k] = v
	}
	next.Checkpoints[key] = cp
	if err := e.saveState(ctx, next, nil, false); err != nil {
		return Checkpoint{}, err
	}
	return cp, nil
}

// Checkpoints lists the checkpoints this engine last read, rereading them first
// when the database answers.
func (e *Engine) Checkpoints() []Checkpoint {
	e.mu.Lock()
	defer e.mu.Unlock()
	_ = e.refresh(context.Background())
	out := []Checkpoint{}
	for _, cp := range e.state.Checkpoints {
		out = append(out, cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CompletedAt > out[j].CompletedAt })
	return out
}

// Completed records remain outside the active state, preserving full audit and
// idempotency across restarts.
func (e *Engine) proposal(ctx context.Context, id string) (Proposal, bool, error) {
	if p, ok := e.state.Proposals[id]; ok {
		return p, true, nil
	}
	if len(id) != 64 {
		return Proposal{}, false, nil
	}
	for _, r := range id {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return Proposal{}, false, nil
		}
	}
	return e.archivedProposal(ctx, id)
}
