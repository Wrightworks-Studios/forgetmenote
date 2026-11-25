package workspace

import (
	"context"
	"fmt"
	"testing"
	"time"

	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
)

func BenchmarkBrowseAfterOneSave(b *testing.B) {
	for _, count := range []int{1000, 10000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			s := newTestStore(b)
			ctx := context.Background()
			notes := make([]notestore.FileChange, count)
			for i := range notes {
				notes[i] = fixture(fmt.Sprintf("n%d.md", i), "# Small note\nA measurement fixture.\n")
			}
			commitFixture(b, s, notes...)
			s = reopenTestStore(b, s)
			start := time.Now()
			if _, err := s.BrowseCollection(ctx, "", ""); err != nil {
				b.Fatal(err)
			}
			cold := time.Since(start)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				doc, _ := s.Read(ctx, "n0.md", "")
				if _, err := s.Save(ctx, "n0.md", []byte(fmt.Sprintf("# Revision %d", i)), doc.Revision, ""); err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
				if _, err := s.BrowseCollection(ctx, "", ""); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(cold.Microseconds())/1000, "cold-ms")
		})
	}
}

func TestIndependentExecutableMarkdownRetainsMetadata(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	commitFixture(t, s, notestore.FileChange{Path: "executable.md", Content: []byte("---\ntitle: Preserved title\ntags: [proof]\n---\nBody\n"), Mode: gitobj.ModeExecutable})
	entries, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Path == "executable.md" {
			if e.Metadata == nil || e.Metadata.Title != "Preserved title" {
				t.Fatalf("regular markdown lost metadata: %+v", e)
			}
			return
		}
	}
	t.Fatal("note not listed")
}

// Browsing lists folders, then notes, each by title regardless of case, and
// items whose titles differ only in case, or not at all, by path, so every
// listing of one revision comes back in one order.
func TestBrowseOrdersEqualTitlesByPath(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	files := map[string]string{"notes/a.md": "# a\n", "Notes/b.md": "# b\n", "NOTES/c.md": "# c\n", "Zeta.md": "# zeta\n", "alpha/d.md": "# d\n"}
	want := []string{"alpha", "NOTES", "Notes", "notes"}
	for i := range 20 {
		files[fmt.Sprintf("same-%02d.md", i)] = "# Same title\n"
		want = append(want, fmt.Sprintf("same-%02d.md", i))
	}
	want = append(want, "Zeta.md")
	commitFiles(t, s, files)
	for range 20 {
		browse, err := s.BrowseCollection(ctx, "", "")
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, item := range browse.Items {
			got = append(got, item.Path)
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("browse order:\n got %v\nwant %v", got, want)
		}
	}
}
