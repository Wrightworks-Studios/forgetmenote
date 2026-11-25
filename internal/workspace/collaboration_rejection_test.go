package workspace

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/reearth/ygo/crdt"
)

// A rejected update leaves no content or receipt, so the same operation ID
// can carry a corrected one up to exactly the live limit, which is the
// ordinary read limit of a note.
func TestLiveRejectionLeavesNoReceiptAndAcceptsUTF8Boundary(t *testing.T) {
	t.Setenv("FMN_LIMIT_FILE_BYTES", strconv.Itoa(256<<10))
	store := newTestStore(t)
	ctx := context.Background()
	limit := dbLiveLimit("boundary.md")
	content := strings.Repeat("é", limit/2-1)
	original, err := store.Save(ctx, "boundary.md", []byte(content), "", "")
	if err != nil {
		t.Fatal(err)
	}
	base, err := store.LiveRead(ctx, original.Path)
	if err != nil {
		t.Fatal(err)
	}
	before := liveStored(t, store, original.Path)
	update := func(suffix string) []byte {
		doc, err := browserDoc(base.Update)
		if err != nil {
			t.Fatal(err)
		}
		doc.Transact(func(tx *crdt.Transaction) { tx.GetText("markdown").Insert(tx, limit/2-1, suffix, nil) })
		return crdt.EncodeStateAsUpdateV1(doc, nil)
	}
	in := LiveUpdate{Path: original.Path, Generation: strings.Split(base.LiveRevision, ":")[0], OperationID: "rejected-then-corrected", Update: update("界")}
	for i := 0; i < 2; i++ {
		if _, err = store.LiveApply(ctx, in); !errors.Is(err, ErrLiveOperationRejected) {
			t.Fatalf("UTF-8 byte overflow lost its rejection type: %v", err)
		}
		if liveStored(t, store, original.Path) != before {
			t.Fatal("rejection persisted content or an operation receipt")
		}
	}
	committed, err := store.Read(ctx, original.Path, "")
	if err != nil || committed.Revision != original.Revision || string(committed.Content) != content {
		t.Fatalf("rejection changed Markdown or Git: %v", err)
	}
	in.Update = update("ok")
	accepted, err := store.LiveApply(ctx, in)
	if err != nil || accepted.Content != content+"ok" || len(accepted.Content) != limit {
		t.Fatalf("rejected ID cannot accept corrected UTF-8 boundary text: %v", err)
	}
	checkpoint, err := store.LiveCheckpoint(ctx, original.Path)
	if err != nil || checkpoint.Pending || checkpoint.Revision == original.Revision {
		t.Fatalf("corrected text did not checkpoint: %v", err)
	}
	replay, err := store.LiveApply(ctx, in)
	if err != nil || replay.Content != accepted.Content || replay.LiveRevision != accepted.LiveRevision {
		t.Fatalf("accepted correction lost its identical-retry receipt: %v", err)
	}
	committed, err = store.Read(ctx, original.Path, "")
	if err != nil || string(committed.Content) != accepted.Content || committed.Revision != checkpoint.Revision {
		t.Fatalf("corrected Markdown differs from its Git checkpoint: %v", err)
	}
}

// A live patch the store accepts rechecks its target's classification then,
// not at a gateway's preflight.
func TestMutationScopeRechecksLivePatchTargetAtOwner(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	note, err := store.Save(ctx, "eligible.md", []byte("# Eligible\n"), "", "author")
	if err != nil {
		t.Fatal(err)
	}
	live, err := store.LiveRead(ctx, note.Path)
	if err != nil {
		t.Fatal(err)
	}
	// Another editor archives the note.
	commitFixture(t, store, fixture(note.Path, "---\nfmn-container: archive\n---\n# Eligible\n"))
	scope := ContentScope{Policy: ContainerPolicy{Version: 1, Archives: "exclude", Drafts: "exclude"}, Collection: "all"}
	scoped := WithMutationScope(ctx, MutationScope{Version: 1, Content: scope, Target: note.Path})
	if _, err = store.LivePatch(scoped, TargetedPatch{Path: note.Path, Base: live.LiveRevision, OperationID: "scope-race", Edits: []TextEdit{{Start: 2, End: 10, Old: "Eligible", Text: "Mutated"}}}); err == nil {
		t.Error("accepted a live patch on an excluded target")
	}
	if doc, _ := store.Read(ctx, note.Path, ""); !strings.Contains(string(doc.Content), "# Eligible") {
		t.Error("rejected patch changed the note")
	}
}
