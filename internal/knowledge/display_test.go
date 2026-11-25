package knowledge

import (
	"strings"
	"testing"

	"forgetmenote/internal/workspace"
)

func TestLargeDisplayUsesOnlyUniqueVerifiedEvidenceRanges(t *testing.T) {
	for _, content := range []string{
		"| Field | Value |\n| --- | --- |\n" + strings.Repeat("| Filler | Earlier words |\n", 4000) + "| UniqueNeedle | Visible row |\n",
		"```\n" + strings.Repeat("earlier code\n", 8000) + "UniqueNeedle = a | b\n```\n",
	} {
		hash := workspace.ContentHash([]byte(content))
		passages, err := Split(workspace.Snapshot{Revision: strings.Repeat("a", 40), Files: []workspace.SnapshotFile{{Path: "large.md", Content: content, Hash: hash}}}, Config{PassageRunes: 4096})
		if err != nil {
			t.Fatal(err)
		}
		p := passages[len(passages)-1]
		out, err := DisplayExcerpts([]byte(content), []Passage{p}, "UniqueNeedle")
		if err != nil || len(out) != 1 || !strings.Contains(out[0], "UniqueNeedle") {
			t.Fatal(out, err)
		}
		if strings.HasPrefix(content, "```") {
			if !strings.Contains(out[0], "UniqueNeedle = a | b") {
				t.Fatal("code changed", out)
			}
		} else if strings.Contains(out[0], "|") {
			t.Fatal("table syntax leaked", out)
		}
		p.Hash = "invalid"
		if _, err = DisplayExcerpts([]byte(content), []Passage{p}, "UniqueNeedle"); err == nil {
			t.Fatal("unverified passage accepted")
		}
	}
	content := strings.Repeat("plain filler\n\n", 6000) + "same exact evidence\n\n```\nsame exact evidence\n```\n"
	p := Passage{NoteHash: workspace.ContentHash([]byte(content)), Text: "same exact evidence", Hash: workspace.ContentHash([]byte("same exact evidence"))}
	out, err := DisplayExcerpts([]byte(content), []Passage{p}, "evidence")
	if err != nil || len(out) != 1 || out[0] != workspace.UnavailableExcerpt {
		t.Fatalf("ambiguous context guessed: %v %v", out, err)
	}
}
