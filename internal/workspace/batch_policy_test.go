package workspace

import (
	"fmt"
	"testing"
)

func TestScopedHistoryBatchesEligiblePostimages(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	revision := ""
	var revisions []string
	for i := 0; i < 16; i++ {
		doc, err := s.Save(ctx, "versions.md", []byte(fmt.Sprintf("# Version %d\n", i)), revision, "")
		if err != nil {
			t.Fatal(err)
		}
		revision = doc.Revision
		revisions = append(revisions, revision)
	}
	scope := ContentScope{Policy: ContainerPolicy{Version: 1, Archives: "exclude", Drafts: "exclude"}, Collection: "all"}
	history, err := s.HistoryScoped(ctx, "versions.md", scope)
	if err != nil || len(history) != 16 {
		t.Fatalf("history=%d, error=%v", len(history), err)
	}
	for i, version := range history {
		if version.Message != "Committed eligible note version" || version.Path != "versions.md" {
			t.Fatal("unfiltered subject", version.Message, version.Path)
		}
		if version.Revision != revisions[len(revisions)-1-i] {
			t.Fatal("versions out of order:", i, version.Revision)
		}
	}
}

func TestScopedHistoryFiltersBeforeLimitAcrossBatchBoundary(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	revision := ""
	first := ""
	for i := 0; i < 70; i++ {
		content := fmt.Sprintf("# Version %d\n", i)
		if i >= 3 {
			content = "---\nfmn-container: archive\n---\n" + content
		}
		doc, err := s.Save(ctx, "versions.md", []byte(content), revision, "")
		if err != nil {
			t.Fatal(err)
		}
		revision = doc.Revision
		if i == 0 {
			first = revision
		}
	}
	scope := ContentScope{Policy: ContainerPolicy{Version: 1, Archives: "exclude", Drafts: "exclude"}, Collection: "all"}
	history, err := s.HistoryScoped(ctx, "versions.md", scope)
	if err != nil || len(history) != 3 || history[2].Revision != first {
		t.Fatalf("eligible historical versions were truncated: count=%d error=%v", len(history), err)
	}
}
