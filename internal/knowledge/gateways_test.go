package knowledge

import (
	"errors"
	"testing"

	"forgetmenote/internal/workspace"
)

// Two gateways' engines for one notebook share its configuration, checkpoints
// and paid vectors through the database. Neither embeds what the other stored,
// overwrites the other's newer state, or keeps vectors the other rebuilt away.
func TestGatewaysShareKnowledgeStateAndVectors(t *testing.T) {
	ctx := t.Context()
	first, doc := indexFixture(t)
	second, err := Open(database(t, first.Store), first.Store)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if _, err = first.Configure(ctx, testConfig(), false); err != nil {
		t.Fatal(err)
	}
	if second.Config().ID() != testConfig().ID() {
		t.Fatal("another gateway's configuration was not adopted")
	}
	fillIndex(t, first, "")
	pending, err := second.Pending(ctx, 32)
	if err != nil || pending.Status.State != "ready" || len(pending.Passages) != 0 {
		t.Fatalf("a gateway asked to embed vectors another gateway stored: %+v %v", pending, err)
	}
	if _, err = first.Store.Save(ctx, "b.md", []byte("# Beta\nA different observation.\n"), "", ""); err != nil {
		t.Fatal(err)
	}
	if added := fillIndex(t, second, ""); added != 1 {
		t.Fatalf("second gateway embedded %d passages, want 1", added)
	}
	if stored := storedVectors(t, first); len(stored) != 2 {
		t.Fatalf("a gateway's write dropped another gateway's vectors: %d stored", len(stored))
	}
	cp, err := second.Complete(ctx, "agent", "review", "", doc.Revision, "Reviewed", false)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := first.Checkpoint(ctx, "agent", "review"); err != nil || got != cp {
		t.Fatalf("another gateway's checkpoint was not read: %+v %v", got, err)
	}
	stale := first.state
	if _, err = second.Complete(ctx, "agent", "other", "", doc.Revision, "Later", false); err != nil {
		t.Fatal(err)
	}
	if err = first.saveState(ctx, stale, nil, false); !errors.Is(err, workspace.ErrConflict) {
		t.Fatalf("a stale write overwrote another gateway's state: %v", err)
	}
	if got, err := first.Checkpoint(ctx, "agent", "other"); err != nil || got.Summary != "Later" {
		t.Fatalf("the newer checkpoint was lost: %+v %v", got, err)
	}
	if _, err = second.Configure(ctx, testConfig(), true); err != nil {
		t.Fatal(err)
	}
	pending, err = first.Pending(ctx, 32)
	if err != nil || len(pending.Passages) != 2 {
		t.Fatalf("a gateway kept vectors another gateway's rebuild discarded: %+v %v", pending, err)
	}
}
