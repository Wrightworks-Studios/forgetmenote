package workspace

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSearchExcerptDisplaysMarkdownWithoutChangingItsMeaning(t *testing.T) {
	cases := []struct{ name, content, query, want string }{
		{"table syntax", "# Fields\n\n| Field | Value |\n| --- | --- |\n| Name | Readable value |\n", "Name", "Field Value Name Readable value"},
		{"metadata only", "---\ntitle: Calendar\n---\n", "Calendar", ""},
		{"body key is real prose", "# Calendar\n\ntitle: a meaningful body label", "title", "title: a meaningful body label"},
		{"links and emphasis", "# Calendar\n\nRead [the schedule](https://example.invalid) and **confirm** dates.", "schedule", "Read the schedule and confirm dates."},
		{"reference links", "# Calendar\n\nRead [the schedule][dates].\n\n[dates]: https://example.invalid", "schedule", "Read the schedule."},
		{"bare URL", "Visit https://docs.example.invalid for setup.", "example", "Visit https://docs.example.invalid for setup."},
		{"bare email", "Email helper@example.invalid for setup.", "example", "Email helper@example.invalid for setup."},
		{"automatic protocol", "Visit www.example.invalid for setup.", "example", "Visit www.example.invalid for setup."},
		{"angle URL", "Visit <https://docs.example.invalid> for setup.", "example", "Visit https://docs.example.invalid for setup."},
		{"HTML not displayed", "# Calendar\n\n<script>secret script</script>\n\nVisible dates.", "dates", "Visible dates."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := SearchExcerpt([]byte(c.content), c.query); got != c.want {
				t.Fatalf("got %q; want %q", got, c.want)
			}
		})
	}
	for _, padding := range []string{strings.Repeat("Ísland details. ", 40), strings.Repeat("Background paragraph.\n\n", 4000)} {
		body := "---\ntitle: not an excerpt\n---\n# Heading\n\n" + padding + "\nThe [Autumn schedule](https://example.invalid) is **confirmed**.\n"
		got := SearchExcerpt([]byte(body), "Autumn")
		if !utf8.ValidString(got) || !strings.Contains(got, "Autumn schedule is confirmed.") || strings.Contains(got, "https://") || len([]rune(got)) > 242 {
			t.Fatalf("invalid bounded late-match preview: %q", got)
		}
	}
}

func TestSearchExcerptKeepsLateMatchesAcrossUnicodeWidths(t *testing.T) {
	for _, c := range []struct{ name, padding string }{
		{"single long paragraph", strings.Repeat("a", 100000)},
		{"lowercase contracts bytes", strings.Repeat("K", 30000)},
		{"lowercase expands bytes", strings.Repeat("Ⱥ", 40000)},
	} {
		t.Run(c.name, func(t *testing.T) {
			content := "# Preview boundary\n\n" + c.padding + " needle: [match](https://example.invalid) remains **readable**.\n"
			got := SearchExcerpt([]byte(content), "NEEDLE")
			if !utf8.ValidString(got) || strings.ContainsRune(got, '�') || len([]rune(got)) > 242 || !strings.Contains(got, "needle: match remains readable.") || strings.Contains(got, "](") || strings.Contains(got, "**") {
				t.Fatalf("invalid bounded late-match preview: %q", got)
			}
		})
	}
}

func TestMarkdownRangeExcerptsUseOriginalTableAndCodeContext(t *testing.T) {
	for _, content := range []string{
		"# Table\n\n| Field | Value |\n| --- | --- |\n" + strings.Repeat("| Earlier | Filler words |\n", 4000) + "| SelectedNeedle | Visible row |\n",
		"# Literal\n\n```\n" + strings.Repeat("earlier code\n", 4000) + "SelectedNeedle = a | b\n```\n",
	} {
		start := strings.Index(content, "SelectedNeedle")
		end := strings.IndexByte(content[start:], '\n') + start
		got := MarkdownRangeExcerpts([]byte(content), [][2]int{{start, end}}, "SelectedNeedle")[0]
		if strings.Contains(content, "# Table") {
			if strings.Contains(got, "|") || got != "SelectedNeedle Visible row" {
				t.Fatalf("table excerpt: %q", got)
			}
		} else if got != "SelectedNeedle = a | b" {
			t.Fatalf("literal code changed: %q", got)
		}
	}
}

func TestSearchExcerptKeepsLongTableContext(t *testing.T) {
	content := "# Table\n\n| Field | Value |\n| --- | --- |\n" + strings.Repeat("| Earlier | Filler context |\n", 4000) + "| LongTableNeedle | Visible final row |\n"
	got := SearchExcerpt([]byte(content), "LongTableNeedle")
	if strings.Contains(got, "|") || !strings.Contains(got, "LongTableNeedle Visible final row") {
		t.Fatal("table context lost", got)
	}
}

func TestDisplayExcerptKeepsEvidenceWhenInlineContextExceedsBudget(t *testing.T) {
	content := "[" + strings.Repeat("label ", 18000) + "UnboundedNeedle](https://example.invalid)\n"
	start := strings.Index(content, "UnboundedNeedle")
	got := MarkdownRangeExcerpts([]byte(content), [][2]int{{start, start + len("UnboundedNeedle")}}, "UnboundedNeedle")[0]
	if got != "Snippet unavailable; open the note to inspect this match." {
		t.Fatalf("unbounded inline context must have an explicit display-only fallback, got %q", got)
	}
	if !strings.Contains(content, "UnboundedNeedle](https://example.invalid)") {
		t.Fatal("original evidence changed")
	}
}

func TestMarkdownRangeExcerptPreservesGrammarContext(t *testing.T) {
	for _, tc := range []struct{ name, content, selected, want string }{
		{"quoted table", "> | Field | Value |\n> | --- | --- |\n> | Needle | Display |\n", "Needle | Display", "Needle Display"},
		{"list table", "- | Field | Value |\n  | --- | --- |\n  | Needle | Display |\n", "Needle | Display", "Needle Display"},
		{"escaped pipe code", "| Field |\n| --- |\n| `Needle \\| value` |\n", "Needle \\| value", "Needle | value"},
		{"mismatched columns", "a | b\n| --- |\nNeedle | value\n| --- | --- |\n", "Needle | value", "Needle | value"},
		{"setext and list", "0\n-:\n-\n", "0\n-:\n-", "0"},
		{"HTML continuation", "<div>\nNeedle hidden\n</div>\n\nVisible\n", "Needle hidden", ""},
		{"reference outside range", "[Needle][link]\n\n[link]: https://example.invalid\n", "[Needle][link]", "Needle"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start := strings.Index(tc.content, tc.selected)
			got := MarkdownRangeExcerpts([]byte(tc.content), [][2]int{{start, start + len(tc.selected)}}, "Needle")[0]
			if got != tc.want {
				t.Fatalf("got %q; want %q", got, tc.want)
			}
		})
	}
}

func TestWideSparseTableUsesExplicitDisplayFallback(t *testing.T) {
	content := "|" + strings.Repeat("x|", 1000) + "\n|" + strings.Repeat("-|", 1000) + "\n" + strings.Repeat("|\n", 300) + "|Needle|\n"
	got := MarkdownRangeExcerpts([]byte(content), [][2]int{{4004, len(content)}}, "Needle")[0]
	if got != unavailableExcerpt {
		t.Fatalf("sparse table amplification must stay display-only: %q", got)
	}
	title, summary := NoteSummary([]byte("# Sparse table\n\n" + content))
	if title != "Sparse table" || summary != UnavailableExcerpt {
		t.Fatalf("ordinary note summary bypassed presentation limits: %q %q", title, summary)
	}
}

func TestExcessiveReferenceContextUsesExplicitDisplayFallback(t *testing.T) {
	content := strings.Repeat("[reference]: https://example.invalid\n", 10000) + "\nNeedle target\n"
	if got := SearchExcerpt([]byte(content), "Needle"); got != unavailableExcerpt {
		t.Fatalf("incomplete reference context must be explicit: %q", got)
	}
}
