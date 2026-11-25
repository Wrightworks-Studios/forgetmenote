package knowledge

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
)

func TestTrashExcludedFromCurrentAndHistoricalSearch(t *testing.T) {
	e, doc := indexFixture(t)
	ctx := context.Background()
	state, _ := e.Store.Status(ctx)
	tr, err := e.Store.TrashNotes(ctx, []string{doc.Path}, state.Revision, "owner")
	if err != nil {
		t.Fatal(err)
	}
	for _, scope := range []string{"current", "history"} {
		query := HybridQuery{SearchQuery: SearchQuery{Text: "observation", Scope: scope}, Mode: "text"}
		result, err := e.HybridSearch(ctx, query)
		if err != nil || len(result.Matches) != 0 {
			t.Fatalf("%s search leaked trash: %+v %v", scope, result, err)
		}
		var included HybridQuery
		json.Unmarshal([]byte(`{"text":"observation","scope":"`+scope+`","mode":"text","includeTrash":true}`), &included)
		result, err = e.HybridSearch(ctx, included)
		if err != nil || len(result.Matches) == 0 {
			t.Fatalf("%s include trash missing: %+v %v", scope, result, err)
		}
	}
	// Reusing the old path must not resurrect its removed historical versions.
	if _, err = e.Store.Save(ctx, doc.Path, []byte("# New occupant\nDifferent content\n"), "", "owner"); err != nil {
		t.Fatal(err)
	}
	result, err := e.HybridSearch(ctx, historyQuery("observation"))
	if err != nil || len(result.Matches) != 0 {
		t.Fatal("old generation leaked", result, err)
	}
	listing, _ := e.Store.ListTrash(ctx)
	if _, err = e.Store.EmptyTrash(ctx, listing.Revision, listing.Confirmation); err != nil {
		t.Fatal(err)
	}
	var q HybridQuery
	json.Unmarshal([]byte(`{"text":"observation","scope":"history","mode":"text","includeTrash":true}`), &q)
	result, err = e.HybridSearch(ctx, q)
	if err != nil || len(result.Matches) != 0 {
		t.Fatal("emptied trash remains searchable", result, err)
	}
	_ = tr
}
func TestTrashHidesHistoryBeforeAnEarlierRename(t *testing.T) {
	e, doc := indexFixture(t)
	ctx := context.Background()
	moved, err := e.Store.Move(ctx, doc.Path, "renamed.md", doc.Revision, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.Store.TrashNotes(ctx, []string{"renamed.md"}, moved.Revision, "owner"); err != nil {
		t.Fatal(err)
	}
	result, err := e.HybridSearch(ctx, historyQuery("observation"))
	if err != nil || len(result.Matches) != 0 {
		t.Fatal("earlier path leaked", result, err)
	}
}
func TestTrashSemanticFiltersAndReusedIdenticalContent(t *testing.T) {
	e, doc := indexFixture(t)
	ctx := context.Background()
	if _, err := e.Configure(ctx, testConfig(), false); err != nil {
		t.Fatal(err)
	}
	state, _ := e.Store.Status(ctx)
	if _, err := e.Store.TrashNotes(ctx, []string{doc.Path}, state.Revision, "owner"); err != nil {
		t.Fatal(err)
	}
	fillIndex(t, e, "current")
	fillIndex(t, e, "history")
	for _, scope := range []string{"current", "history"} {
		q := SearchQuery{Scope: scope, Text: "observation", Vector: []float32{1, 0, 0}, ConfigID: testConfig().ID()}
		results, err := e.Search(ctx, q)
		if err != nil || len(results) != 0 {
			t.Fatal("semantic Trash leak", scope, results, err)
		}
		q.IncludeTrash = true
		results, err = e.Search(ctx, q)
		if err != nil || len(results) == 0 {
			t.Fatal("semantic Trash opt-in missing", scope, results, err)
		}
	}
	recreated, err := e.Store.Save(ctx, doc.Path, doc.Content, "", "owner")
	if err != nil {
		t.Fatal(err)
	}
	results, err := e.HybridSearch(ctx, historyQuery("observation"))
	if err != nil || len(results.Matches) != 1 {
		t.Fatal("reused content hidden", results, err)
	}
	for _, hit := range results.Matches {
		if hit.Revision != recreated.Revision || hit.TrashID != "" {
			t.Fatal("wrong generation", hit)
		}
		for _, occ := range hit.Occurrences {
			if occ.Revision == doc.Revision {
				t.Fatal("deleted generation in provenance")
			}
		}
	}
}
func TestTrashNeverResurrectsAnEmptiedPathGeneration(t *testing.T) {
	e, doc := indexFixture(t)
	ctx := context.Background()
	if _, err := e.Store.TrashNotes(ctx, []string{doc.Path}, doc.Revision, "owner"); err != nil {
		t.Fatal(err)
	}
	listing, _ := e.Store.ListTrash(ctx)
	if _, err := e.Store.EmptyTrash(ctx, listing.Revision, listing.Confirmation); err != nil {
		t.Fatal(err)
	}
	replacement, err := e.Store.Save(ctx, doc.Path, []byte("# Replacement\nNew generation\n"), "", "owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.Store.TrashNotes(ctx, []string{doc.Path}, replacement.Revision, "owner"); err != nil {
		t.Fatal(err)
	}
	query := historyQuery("observation")
	query.IncludeTrash = true
	result, err := e.HybridSearch(ctx, query)
	if err != nil || len(result.Matches) != 0 {
		t.Fatal("emptied generation resurrected", result, err)
	}
}
func TestTrashRestoreRenameProvenance(t *testing.T) {
	e, doc := indexFixture(t)
	ctx := context.Background()
	tr, err := e.Store.TrashNotes(ctx, []string{doc.Path}, doc.Revision, "owner")
	if err != nil {
		t.Fatal(err)
	}
	restored, err := e.Store.RestoreTrash(ctx, tr.Entries[0].ID, "renamed-restored.md", tr.Revision, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.Store.TrashNotes(ctx, []string{"renamed-restored.md"}, restored.Revision, "owner"); err != nil {
		t.Fatal(err)
	}
	r, err := e.HybridSearch(ctx, historyQuery("observation"))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Matches) != 0 {
		t.Fatalf("retrashed restored content leaked: %+v", r.Matches)
	}
}

func TestTrashTracksMoveWithRewrittenLinks(t *testing.T) {
	e, doc := indexFixture(t)
	ctx := context.Background()
	content := []byte("[SecretRenamePhrase](an-extremely-long-relative-attachment-name.png)\n")
	doc, err := e.Store.Save(ctx, doc.Path, content, doc.Revision, "owner")
	if err != nil {
		t.Fatal(err)
	}
	moved, err := e.Store.Move(ctx, doc.Path, "notes/moved.md", doc.Revision, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.Store.TrashNotes(ctx, []string{"notes/moved.md"}, moved.Revision, "owner"); err != nil {
		t.Fatal(err)
	}
	r, err := e.HybridSearch(ctx, historyQuery("SecretRenamePhrase"))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Matches) != 0 {
		t.Fatalf("rewritten move history leaked: %+v", r.Matches)
	}
}
func TestHistoryPreservesPassageCitation(t *testing.T) {
	e, _ := indexFixture(t)
	ctx := context.Background()
	r, err := e.HybridSearch(ctx, historyQuery("observation"))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Matches) != 1 {
		t.Fatal(r)
	}
	hit := r.Matches[0]
	u, err := url.Parse(hit.Citation)
	if err != nil {
		t.Fatal(err)
	}
	if u.Query().Get("fmn-passage") != hit.Hash || !strings.Contains(hit.Citation, "fmn-chars") || u.Fragment == "" {
		t.Fatalf("passage citation lost identity: %+v", hit)
	}
}

func TestTrashRestoreRewrittenLinksProvenance(t *testing.T) {
	e, doc := indexFixture(t)
	ctx := context.Background()
	content := []byte("[SecretRestorePhrase](an-extremely-long-relative-attachment-name.png)\n")
	doc, err := e.Store.Save(ctx, doc.Path, content, doc.Revision, "owner")
	if err != nil {
		t.Fatal(err)
	}
	tr, err := e.Store.TrashNotes(ctx, []string{doc.Path}, doc.Revision, "owner")
	if err != nil {
		t.Fatal(err)
	}
	restored, err := e.Store.RestoreTrash(ctx, tr.Entries[0].ID, "", tr.Revision, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.Store.TrashNotes(ctx, []string{doc.Path}, restored.Revision, "owner"); err != nil {
		t.Fatal(err)
	}
	listing, err := e.Store.ListTrash(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.Store.EmptyTrash(ctx, listing.Revision, listing.Confirmation); err != nil {
		t.Fatal(err)
	}
	query := historyQuery("SecretRestorePhrase")
	query.IncludeTrash = true
	r, err := e.HybridSearch(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Matches) != 0 {
		t.Fatalf("emptied restored history leaked: %+v", r.Matches)
	}
}

func TestTrashDoesNotHideReusedFormerNameAfterMove(t *testing.T) {
	e, doc := indexFixture(t)
	ctx := context.Background()
	_, err := e.Store.Move(ctx, doc.Path, "moved.md", doc.Revision, "owner")
	if err != nil {
		t.Fatal(err)
	}
	reused, err := e.Store.Save(ctx, doc.Path, []byte("# ReusedFormerName\n"), "", "owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.Store.TrashNotes(ctx, []string{"moved.md"}, reused.Revision, "owner"); err != nil {
		t.Fatal(err)
	}
	r, err := e.HybridSearch(ctx, historyQuery("ReusedFormerName"))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Matches) != 1 || r.Matches[0].Revision != reused.Revision {
		t.Fatalf("new name generation hidden: %+v", r.Matches)
	}
}
