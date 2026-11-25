package knowledge

import (
	"context"
	"fmt"
	"forgetmenote/internal/attribution"
	"forgetmenote/internal/workspace"
	"testing"
)

func TestProposalAndIndependentConsumerCheckpoints(t *testing.T) {
	e, d := indexFixture(t)
	ctx := attribution.With(context.Background(), attribution.Identity{ID: "alice", Name: "Alice", Email: "alice@users.gitnotes.invalid", Source: "mcp", Client: "client-a"})
	group := workspace.EditGroup{ID: "proposal-1", Summary: "Clarify the research note", Edits: []workspace.NoteEdit{{Path: d.Path, Revision: d.Revision, Hash: workspace.ContentHash(d.Content), Content: "# Alpha\nA clarified observation.\n"}}}
	p, err := e.Propose(ctx, "alice", group)
	if err != nil {
		t.Fatal(err)
	}
	again, err := e.Propose(ctx, "alice", group)
	if err != nil || again.ID != p.ID {
		t.Fatal(again, err)
	}
	doc, _ := e.Store.Read(ctx, d.Path, "")
	if string(doc.Content) != string(d.Content) {
		t.Fatal("proposal edited content")
	}
	reviewer := attribution.With(context.Background(), attribution.Identity{ID: "bob", Name: "Bob", Email: "bob@users.gitnotes.invalid", Source: "browser"})
	applied, err := e.Decide(reviewer, "bob", p.ID, "accept")
	if err != nil || applied.AcceptedBy != "bob" || applied.Status != "applied" {
		t.Fatal(applied, err)
	}
	repeated, err := e.Decide(reviewer, "bob", p.ID, "accept")
	if err != nil || repeated.Revision != applied.Revision {
		t.Fatal(repeated, err)
	}
	cp, err := e.Complete(ctx, "alice", "review", "", applied.Revision, "Reviewed snapshot", false)
	if err != nil {
		t.Fatal(err)
	}
	independent, err := e.Checkpoint(ctx, "alice", "ingest")
	if err != nil || independent.Revision != "" {
		t.Fatal("shared checkpoint", independent, err)
	}
	reopened, err := Open(database(t, e.Store), e.Store)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := reopened.Checkpoint(ctx, "alice", "review")
	if err != nil || restored.Revision != cp.Revision {
		t.Fatal(restored, err)
	}
	if _, err = e.Complete(ctx, "alice", "review", "wrong", cp.Revision, "stale writer", false); err == nil {
		t.Fatal("checkpoint CAS missing")
	}
}

func TestCheckpointHistoryRewriteRequiresExplicitFreshReview(t *testing.T) {
	e, doc := indexFixture(t)
	ctx := context.Background()
	e.Complete(ctx, "agent", "review", "", doc.Revision, "Reviewed", false)
	// A purge, the one rewrite of a notebook's history, replaces the
	// reviewed revision.
	plan, err := e.Store.PreparePurge(ctx, workspace.PurgeScope{Kind: workspace.PurgeText, Text: "An observation."}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.Store.ExecutePurge(ctx, plan, plan.Confirmation, "owner", "Synthetic redaction", nil); err != nil {
		t.Fatal(err)
	}
	if _, err = e.Store.Read(ctx, doc.Path, doc.Revision); err == nil {
		t.Fatal("the purge kept the reviewed revision")
	}
	next, err := e.Store.Save(ctx, "replacement.md", []byte("New history"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	changes, err := e.Store.ChangesSince(ctx, doc.Revision, next.Revision)
	if err != nil || !changes.FreshReview {
		t.Fatal(changes, err)
	}
	if _, err = e.Complete(ctx, "agent", "review", doc.Revision, next.Revision, "Reviewed replacement", false); err == nil {
		t.Fatal("rewritten history silently accepted")
	}
	completed, err := e.Complete(ctx, "agent", "review", doc.Revision, next.Revision, "Reviewed replacement", true)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := e.Complete(ctx, "agent", "review", doc.Revision, next.Revision, "Reviewed replacement", true)
	if err != nil || replay != completed {
		t.Fatal("completion retry was not idempotent", replay, err)
	}
}

func TestCompletedProposalArchivePreservesRetry(t *testing.T) {
	e, d := indexFixture(t)
	ctx := context.Background()
	group := workspace.EditGroup{ID: "archive-test", Summary: "Propose", Edits: []workspace.NoteEdit{{Path: d.Path, Revision: d.Revision, Hash: workspace.ContentHash(d.Content), Content: "new"}}}
	p, err := e.Propose(ctx, "alice", group)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.Decide(ctx, "owner", p.ID, "archive"); err == nil {
		t.Fatal("archived active proposal")
	}
	if _, err = e.Decide(ctx, "alice", p.ID, "dismiss"); err != nil {
		t.Fatal(err)
	}
	if _, err = e.Decide(ctx, "owner", p.ID, "archive"); err != nil {
		t.Fatal(err)
	}
	listed, err := e.Proposals(ctx)
	if err != nil || len(listed) != 0 {
		t.Fatal(listed, err)
	}
	reopened, err := Open(database(t, e.Store), e.Store)
	if err != nil {
		t.Fatal(err)
	}
	again, err := reopened.Propose(ctx, "alice", group)
	if err != nil || again.ID != p.ID || again.Status != "dismissed" {
		t.Fatal(again, err)
	}
	group.Summary = "changed"
	if _, err = reopened.Propose(ctx, "alice", group); err == nil {
		t.Fatal("archival lost request identity")
	}
}

func TestMoreThanTwoHundredCompletedProposals(t *testing.T) {
	e, d := indexFixture(t)
	ctx := context.Background()
	for i := 0; i < 205; i++ {
		group := workspace.EditGroup{ID: fmt.Sprintf("completed-%d", i), Summary: "Review", Edits: []workspace.NoteEdit{{Path: d.Path, Revision: d.Revision, Hash: workspace.ContentHash(d.Content), Content: "proposed"}}}
		p, err := e.Propose(ctx, "alice", group)
		if err != nil {
			t.Fatal(i, err)
		}
		if _, err = e.Decide(ctx, "alice", p.ID, "dismiss"); err != nil {
			t.Fatal(err)
		}
		if _, err = e.Decide(ctx, "owner", p.ID, "archive"); err != nil {
			t.Fatal(err)
		}
	}
	list, err := e.Proposals(ctx)
	if err != nil || len(list) != 0 {
		t.Fatal(list, err)
	}
}
