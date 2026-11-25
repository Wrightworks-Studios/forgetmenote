package workspace

import (
	"context"
	"errors"
	"testing"
)

func TestFindingsReuseCommentsAndDetectChangedContent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	d, _ := s.Save(ctx, "a.md", []byte("# A\n"), "", "")
	input := FindingInput{Key: "review-1", Path: "a.md", Title: "Check evidence", Text: "The claim needs a source.", Revision: d.Revision, Hash: ContentHash(d.Content)}
	finding, err := s.AddFinding(ctx, input, "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.AddFinding(ctx, input, "reviewer")
	if err != nil || again.Comment.ID != finding.Comment.ID {
		t.Fatal(again, err)
	}
	comments, err := s.Comments(ctx, "a.md")
	if err != nil || len(comments.Comments) != 1 || comments.Comments[0].Text != input.Text {
		t.Fatal(comments, err)
	}
	s.Save(ctx, "a.md", []byte("# Revised\n"), d.Revision, "")
	inbox, err := s.Findings(ctx)
	if err != nil || len(inbox) != 1 || !inbox[0].Comment.Finding.Stale {
		t.Fatal(inbox, err)
	}
	if _, err = s.DecideFinding(ctx, "a.md", finding.Comment.ID, inbox[0].Revision, "other", "dismiss", false); err == nil {
		t.Fatal("unauthorized dismissal")
	}
	resolved, err := s.DecideFinding(ctx, "a.md", finding.Comment.ID, inbox[0].Revision, "editor", "resolve", true)
	if err != nil || !resolved.Comment.Resolved {
		t.Fatal(resolved, err)
	}
}

func TestFindingEvidencePinsAndMovesWithDiscussion(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	source, _ := s.Save(ctx, "source.md", []byte("Original evidence"), "", "")
	note, _ := s.Save(ctx, "note.md", []byte("A claim"), "", "")
	_, err := s.AddFinding(ctx, FindingInput{Key: "test", Path: "note.md", Title: "Check", Text: "Compare", Revision: note.Revision, Hash: ContentHash(note.Content), Evidence: []SourceEvidence{{Path: "source.md"}}}, "agent")
	if err != nil {
		t.Fatal(err)
	}
	s.Save(ctx, "source.md", []byte("Changed evidence"), source.Revision, "")
	inbox, err := s.Findings(ctx)
	if err != nil || len(inbox) != 1 || !inbox[0].Comment.Finding.Stale {
		t.Fatal("source was not pinned", inbox, err)
	}
	head, _ := s.Status(ctx)
	_, err = s.Move(ctx, "note.md", "moved.md", head.Revision, "")
	if err != nil {
		t.Fatal(err)
	}
	inbox, err = s.Findings(ctx)
	if err != nil || len(inbox) != 1 || inbox[0].Path != "moved.md" {
		t.Fatal("finding lost during move", inbox, err)
	}
}

// A scoped finding rechecks its sources when it is written: a source
// another gateway archived after the preflight is refused, and the
// discussion is left as it was.
func TestMutationScopeFindingsRecheckSources(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	target, err := s.Save(ctx, "target.md", []byte("# Target\n"), "", "author")
	if err != nil {
		t.Fatal(err)
	}
	source, err := s.Save(ctx, "source.md", []byte("# Source\n"), "", "author")
	if err != nil {
		t.Fatal(err)
	}
	scope := MutationScope{Version: 1, Content: ContentScope{Policy: ContainerPolicy{Version: 1, Archives: "exclude", Drafts: "exclude"}, Collection: "all"}, Sources: []MutationSource{{Path: source.Path, Revision: source.Revision, Hash: ContentHash(source.Content)}}}
	head := commitFixture(t, s, fixture(source.Path, "---\nfmn-container: archive\n---\n# Source\n"))
	finding := FindingInput{Key: "preflight-source-race", Path: target.Path, Title: "Finding", Text: "Must not publish out-of-scope source", Revision: head, Hash: ContentHash(target.Content), Evidence: []SourceEvidence{{Path: source.Path, Revision: source.Revision, Hash: ContentHash(source.Content)}}}
	if _, err = s.AddFinding(WithMutationScope(ctx, scope), finding, "agent"); !errors.Is(err, ErrForbidden) {
		t.Fatal("finding accepted a source excluded between preflight and owner effect", err)
	}
	discussion, _ := s.Comments(ctx, target.Path)
	if len(discussion.Comments) != 0 || headOf(t, s) != head {
		t.Fatal("failed finding left a discussion mutation")
	}
	scope.Sources = nil
	if _, err = s.AddFinding(WithMutationScope(ctx, scope), finding, "agent"); err != nil {
		t.Fatal("a finding in scope was refused", err)
	}
}
