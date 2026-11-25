package workspace

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
)

// The committed lock policy is parsed once per content, every committed
// change to it is seen, and an invalid one is refused.
func TestCommittedLockPolicyIsReusedUntilItChanges(t *testing.T) {
	s := newTestStore(t)
	backend := s.backend
	ctx := t.Context()
	load := func() ([]string, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		err := backend.loadLocks(ctx)
		return slices.Clone(s.projectLocks), err
	}
	if rules, err := load(); err != nil || rules != nil {
		t.Fatal("no policy:", rules, err)
	}
	commitFixture(t, s, fixture(locksPath, `{"version":1,"paths":["protected.md"]}`))
	if rules, err := load(); err != nil || !slices.Equal(rules, []string{"protected.md"}) {
		t.Fatal("committed lock lost", rules, err)
	}
	parsed := backend.locksBlob
	head := commitFixture(t, s, fixture("other.md", "unrelated"))
	if rules, err := load(); err != nil || !slices.Equal(rules, []string{"protected.md"}) || backend.locksBlob != parsed || s.locksRevision != head {
		t.Fatal("an unrelated commit:", rules, err)
	}
	if state, err := backend.lockState(ctx, "protected.md"); err != nil || state.Revision != head || len(state.Rules) != 1 {
		t.Fatal("lock state:", state, err)
	}
	commitFixture(t, s, fixture(locksPath, `{"version":1,"paths":[]}`))
	if rules, err := load(); err != nil || len(rules) != 0 {
		t.Fatal("cached policy ignored committed unlock", rules, err)
	}
	for name, bad := range map[string]notestore.FileChange{
		"version":   fixture(locksPath, `{"version":2,"paths":[]}`),
		"syntax":    fixture(locksPath, `not json`),
		"duplicate": fixture(locksPath, `{"version":1,"paths":["a.md","a.md"]}`),
		"outside":   fixture(locksPath, `{"version":1,"paths":["../a.md"]}`),
		"size":      fixture(locksPath, `{"version":1,"paths":[]}`+strings.Repeat(" ", 65536)),
		"link":      {Path: locksPath, Content: []byte("elsewhere"), Mode: gitobj.ModeSymlink},
	} {
		commitFixture(t, s, bad)
		if _, err := load(); err == nil {
			t.Errorf("%s: an invalid policy was accepted", name)
		}
	}
}

// The cached Trash index is the committed one: callers get copies, and a
// missing or invalid index is refused while Trash content exists.
func TestCachedTrashIndexIsPrivateAndChecked(t *testing.T) {
	s := newTestStore(t)
	backend := s.backend
	ctx := t.Context()
	load := func() (TrashIndex, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		return backend.loadTrash(ctx)
	}
	doc, err := s.Save(ctx, "discard.md", []byte("# Retained\n"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("ab", 16)
	index := TrashIndex{Version: 1, Records: []TrashRecord{{TrashEntry: TrashEntry{ID: id, Path: "Trash/" + id + ".md", OriginalPath: doc.Path, Title: "Retained"}, History: map[string][]string{doc.Path: {doc.Revision}}}}}
	raw, _ := json.MarshalIndent(index, "", "  ")
	commitFixture(t, s, notestore.FileChange{Path: "Trash/" + id + ".md", From: doc.Path}, fixture(trashIndexPath, string(raw)+"\n"))
	policy, err := load()
	if err != nil || len(policy.Records) != 1 || policy.Records[0].History[doc.Path][0] != doc.Revision {
		t.Fatal("trash policy missing", policy, err)
	}
	policy.Records[0].History[doc.Path][0] = "mutated by caller"
	policy.Records[0].History["injected.md"] = []string{doc.Revision}
	policy.Records[0].Title = "mutated title"
	commitFixture(t, s, fixture("other.md", "unrelated"))
	for range 2 {
		again, err := load()
		if err != nil || again.Records[0].History[doc.Path][0] != doc.Revision || again.Records[0].Title != "Retained" || again.Records[0].History["injected.md"] != nil {
			t.Fatal("cached trash authority was mutable through its reader", err)
		}
	}
	commitFixture(t, s, fixture(trashIndexPath, `{"version":1,"records":[{"id":"bad"}]}`))
	if _, err := load(); err == nil {
		t.Fatal("an invalid Trash index was accepted")
	}
	commitFixture(t, s, notestore.FileChange{Path: trashIndexPath})
	if _, err := load(); err == nil || !strings.Contains(err.Error(), "not system managed") {
		t.Fatal("Trash content without an index:", err)
	}
	commitFixture(t, s, notestore.FileChange{Path: "Trash/" + id + ".md"})
	if index, err := load(); err != nil || len(index.Records) != 0 {
		t.Fatal("no Trash:", index, err)
	}
}
