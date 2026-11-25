package knowledge

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestWritingSharedProseFixtures(t *testing.T) {
	raw, err := os.ReadFile("testdata/writing.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []struct {
		Name, Markdown string
		Words          int
	}
	if err = json.Unmarshal(raw, &fixtures); err != nil {
		t.Fatal(err)
	}
	for _, fixture := range fixtures {
		t.Run(fixture.Name, func(t *testing.T) {
			if got := AnalyzeWriting([]byte(fixture.Markdown)).Stats.Words; got != fixture.Words {
				t.Fatalf("got %d words, want %d", got, fixture.Words)
			}
		})
	}
}

func TestWritingProseRangesAndSections(t *testing.T) {
	content := []byte("---\ntitle: Hidden words\n---\n# Act one\n\nCaf&eacute; sees **Lizzy**.\n\n## Later\n\nElizabeth returns. `Hidden code`\n\n***\n\nLast scene.\n\n```\nMore hidden code\n```\n<!-- hidden -->\n")
	result := AnalyzeWriting(content)
	if result.Stats.Words != 10 || len(result.Sections) != 3 || result.Sections[0].Words != 5 || result.Sections[0].SubtreeWords != 10 || result.Sections[1].SubtreeWords != 5 {
		t.Fatalf("analysis: %+v", result)
	}
	matches, more := result.Mentions(content, []WritingTerm{{Name: "Café"}, {Name: "Elizabeth", Aliases: []string{"Lizzy"}}}, 0, 100)
	if more || len(matches) != 3 {
		t.Fatalf("mentions %+v, more %v", matches, more)
	}
	if string(content[matches[0].StartByte:matches[0].EndByte]) != "Caf&eacute;" || matches[0].StartLine != 6 || matches[0].Text != "Café" {
		t.Fatalf("entity source mapping: %+v", matches[0])
	}
	if string(content[matches[1].StartByte:matches[1].EndByte]) != "Lizzy" {
		t.Fatalf("markup source mapping: %+v", matches[1])
	}
}
func TestWritingTermsBoundariesPaginationAndNoCode(t *testing.T) {
	content := []byte("Lizzy and Elizabeth; Elizabethan, café, caféine, C++.\n\n`Lizzy` [Lizzy](https://Lizzy.example) ![Lizzy](Lizzy.png)\n\n    Lizzy\n")
	terms := []WritingTerm{{Name: "Elizabeth", Aliases: []string{"Lizzy"}}, {Name: "Café"}, {Name: "C++"}}
	result := AnalyzeWriting(content)
	matches, more := result.Mentions(content, terms, 0, 2)
	if len(matches) != 2 || !more || matches[0].Text != "Lizzy" || matches[1].Text != "Elizabeth" {
		t.Fatalf("first page %+v %v", matches, more)
	}
	matches, more = result.Mentions(content, terms, 2, 3)
	if len(matches) != 3 || more || matches[0].Text != "café" || matches[1].Text != "C++" || matches[2].Text != "Lizzy" {
		t.Fatalf("second page %+v %v", matches, more)
	}
}
func BenchmarkWriting100KWords(b *testing.B) {
	content := []byte(strings.Repeat("# Chapter\n\n"+strings.Repeat("Elizabeth sees the quiet harbour and walks toward home. ", 11)+"\n\n", 1000))
	b.SetBytes(int64(len(content)))
	b.ReportAllocs()
	for b.Loop() {
		AnalyzeWriting(content)
	}
}

func TestWritingShortAliasBeforeLongerPrefix(t *testing.T) {
	content := []byte("Elizabeth Bennetton met Elizabeth Bennet.")
	matches, _ := AnalyzeWriting(content).Mentions(content, []WritingTerm{{Name: "Elizabeth Bennet", Aliases: []string{"Elizabeth"}}}, 0, 100)
	if len(matches) != 2 || matches[0].Text != "Elizabeth" || matches[1].Text != "Elizabeth Bennet" {
		t.Fatalf("aliases: %+v", matches)
	}
}

func TestWritingSceneRangesComeFromParsedBreaks(t *testing.T) {
	content := []byte("# Chapter\n\n```\n***\n```\n\n***\n\n***\n\n> ***\n>\n> Last scene.\n")
	sections := AnalyzeWriting(content).Sections
	if len(sections) != 4 {
		t.Fatalf("scenes: %+v", sections)
	}
	for i, line := range []int{1, 7, 9, 11} {
		if sections[i].StartLine != line || i > 0 && sections[i].StartByte <= sections[i-1].StartByte {
			t.Fatalf("scene %d has wrong source position: %+v", i, sections[i])
		}
	}
	if sections[3].Words != 2 || sections[0].SubtreeWords != 3 {
		t.Fatalf("scene counts: %+v", sections)
	}
}
