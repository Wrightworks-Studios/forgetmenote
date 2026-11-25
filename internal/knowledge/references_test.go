package knowledge

import (
	"context"
	"forgetmenote/internal/workspace"
	"strings"
	"testing"
)

func TestEvidenceBacklinksAndPinnedSections(t *testing.T) {
	e, d := indexFixture(t)
	ctx := context.Background()
	text := "# Summary\n[Evidence](a.md?fmn-rev=" + d.Revision + "#alpha) and [Outside](https://example.org/source).\n"
	e.Store.Save(ctx, "summary.md", []byte(text), "", "")
	evidence, err := e.Evidence(ctx, "summary.md")
	if err != nil || len(evidence.Sources) != 2 {
		t.Fatal(evidence, err)
	}
	if evidence.Sources[0].State != "Unchanged" || evidence.Sources[0].Scope != "heading" {
		t.Fatal(evidence.Sources)
	}
	refs, err := e.Evidence(ctx, "a.md")
	if err != nil || len(refs.Backlinks) != 1 {
		t.Fatal(refs, err)
	}
	e.Store.Save(ctx, "a.md", []byte("# Alpha\nChanged source.\n"), d.Revision, "")
	evidence, err = e.Evidence(ctx, "summary.md")
	if err != nil || evidence.Sources[0].State != "Source changed" {
		t.Fatal(evidence, err)
	}
	if !strings.Contains(evidence.Limitations, "HTML") {
		t.Fatal("syntax limitations missing")
	}
}

func TestHeadingCitationIncludesNestedContent(t *testing.T) {
	before := []byte("# Parent\nIntroduction\n## Detail\nOld measurement\n# Other\nUnrelated\n")
	after := []byte("# Parent\nIntroduction\n## Detail\nNew measurement\n# Other\nUnrelated\n")
	a, ok := sectionHash(before, "parent")
	b, ok2 := sectionHash(after, "parent")
	if !ok || !ok2 || a == b {
		t.Fatal("nested source change was missed")
	}
}

func TestArchivedExternalEvidence(t *testing.T) {
	e, _ := indexFixture(t)
	ctx := context.Background()
	capture := workspace.SourceCapture{Title: "Reference", Original: []byte("<p>amber evidence</p>"), Text: "amber evidence\n", Record: workspace.SourceRecord{URL: "https://example.org/evidence", MIME: "text/html", Method: "uploaded"}}
	doc, err := e.Store.ImportSource(ctx, capture, "", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.Store.Save(ctx, "citations.md", []byte("[Evidence](https://example.org/evidence)"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := e.Evidence(ctx, "citations.md")
	if err != nil || len(evidence.Sources) != 1 || evidence.Sources[0].Path != doc.Path || !strings.Contains(evidence.Sources[0].State, "Archived") {
		t.Fatalf("external archive unresolved: %+v %v", evidence, err)
	}
}

func TestArchivedTextParticipatesInSemanticIndex(t *testing.T) {
	e, _ := indexFixture(t)
	ctx := context.Background()
	capture := workspace.SourceCapture{Title: "Source", Original: []byte("<p>amber paradigm</p>"), Text: "## Page 1\n\namber paradigm\n", Record: workspace.SourceRecord{MIME: "text/html"}}
	doc, err := e.Store.ImportSource(ctx, capture, "", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.Configure(ctx, testConfig(), false); err != nil {
		t.Fatal(err)
	}
	pending, err := e.Pending(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	vectors := []VectorSubmission{}
	for _, p := range pending.Passages {
		if p.Path == doc.Path {
			vectors = append(vectors, VectorSubmission{ID: p.ID, Hash: p.Hash, NoteHash: p.NoteHash, Vector: []float32{1, 0, 0}})
		}
	}
	if len(vectors) == 0 {
		t.Fatal("archived source has no indexing passages")
	}
	status, err := e.Submit(ctx, Submission{Revision: pending.Status.Revision, ConfigID: pending.Status.ConfigID, Vectors: vectors})
	if err != nil {
		t.Fatal(err)
	}
	results, err := e.Search(ctx, SearchQuery{Vector: []float32{1, 0, 0}, ConfigID: status.ConfigID, Path: doc.Path})
	if err != nil || len(results) == 0 || results[0].Path != doc.Path {
		t.Fatal("archived text missing from semantic search", results, err)
	}
}
