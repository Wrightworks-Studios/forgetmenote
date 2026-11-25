package knowledge

import (
	"forgetmenote/internal/workspace"
	"strings"
	"testing"
)

func testConfig() Config {
	return Config{Enabled: true, Model: "fixture-v1", Dimensions: 3, PassageRunes: 128, Splitter: "markdown-heading-v1"}
}
func TestPassagesBoundedHeadingAwareAndReusable(t *testing.T) {
	content := "---\ntitle: Hidden front matter\n---\n# Research\n" + strings.Repeat("Observation. ", 30) + "\n```\n# not a heading\n```\n\nNext steps\n----------\nA conclusion.\n"
	snap := workspace.Snapshot{Revision: strings.Repeat("a", 40), Files: []workspace.SnapshotFile{{Path: "a.md", Content: content, Hash: workspace.ContentHash([]byte(content))}}}
	passages, err := Split(snap, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if len(passages) < 3 {
		t.Fatal(passages)
	}
	next := false
	for _, p := range passages {
		if len([]rune(p.Text)) > 128 || p.Hash == "" || p.NoteHash == "" || strings.Contains(p.Text, "Hidden front matter") {
			t.Fatalf("invalid passage %+v", p)
		}
		if p.Heading == "not a heading" {
			t.Fatal("fence parsed as heading")
		}
		if p.Heading == "Next steps" && p.Anchor != "" {
			next = true
		}
	}
	if !next {
		t.Fatal("missing setext heading")
	}
	snap.Revision = strings.Repeat("b", 40)
	again, _ := Split(snap, testConfig())
	if passages[0].Hash != again[0].Hash || passages[0].ID != again[0].ID {
		t.Fatal("unchanged passage not reusable")
	}
}

func TestPassagesLeaveOutViewCodeAndStayInTheNote(t *testing.T) {
	content := "# Dashboard\n\nCounts the notes about burrows.\n\n```view\n<script>fmn.notes.search('Wombat')</script>\n```\n\nRefreshed when opened.\n\n## Later\n\n~~~view\n<p>Wombat</p>\n~~~\n"
	snap := workspace.Snapshot{Revision: strings.Repeat("a", 40), Files: []workspace.SnapshotFile{{Path: "dashboard.md", Content: content, Hash: workspace.ContentHash([]byte(content))}}}
	texts := []string{}
	_, err := splitPassages(snap, testConfig(), func(p Passage, at [2]int) {
		if content[at[0]:at[1]] != p.Text {
			t.Fatalf("passage %q is not the note text at %v: %q", p.Text, at, content[at[0]:at[1]])
		}
		texts = append(texts, p.Text)
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"# Dashboard\n\nCounts the notes about burrows.", "Refreshed when opened.", "## Later"}; strings.Join(texts, "|") != strings.Join(want, "|") {
		t.Fatalf("passages: got %q, want %q", texts, want)
	}
	passages, err := Split(snap, testConfig())
	if err != nil || len(passages) != 3 || passages[0].ID == passages[1].ID {
		t.Fatalf("view-split passages need distinct identities: %+v %v", passages, err)
	}
}
