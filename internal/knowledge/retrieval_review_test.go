package knowledge

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestReviewLongNoteDoesNotCrowdOutComparison(t *testing.T) {
	var body strings.Builder
	body.WriteString("# Saffron planning guide\n")
	for i := 0; i < 130; i++ {
		fmt.Fprintf(&body, "## Section %d\nSaffron planning option %d has separate evidence and implications.\n", i, i)
	}
	e := retrievalNotes(t, map[string]string{"a.md": body.String(), "b.md": "# Comparison\nSaffron is an alternative for the second independent plan with different risks."})
	got, err := e.Retrieve(context.Background(), RetrievalQuery{Text: "saffron", LocalOnly: true, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, p := range got.Passages {
		seen[p.Path] = true
	}
	if !seen["a.md"] || !seen["b.md"] {
		t.Fatalf("matching comparison source dropped before per-note packing: paths=%v, count=%d", seen, len(got.Passages))
	}
}
