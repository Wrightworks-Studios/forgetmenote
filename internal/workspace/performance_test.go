package workspace

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"forgetmenote/internal/benchfixture"
	"forgetmenote/internal/notestore"
)

// Use committed synthetic notebooks so measurements include the real store
// and metadata boundaries. Fixture construction is excluded from each measurement.
func BenchmarkWorkspaceOperations(b *testing.B) {
	for _, count := range []int{1000, 10000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			store := newTestStore(b)
			ctx := context.Background()
			notes := make([]notestore.FileChange, count)
			for i := range notes {
				body := "# Measurement\nOrdinary synthetic note.\n"
				if i == 0 {
					body += "Rare amber evidence.\n"
				}
				notes[i] = fixture(fmt.Sprintf("n%d.md", i), body)
			}
			commitFixture(b, store, notes...)
			if _, err := store.BrowseCollection(ctx, "", ""); err != nil {
				b.Fatal(err)
			}
			b.Run("search", func(b *testing.B) {
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					entries, err := store.Search(ctx, "rare amber")
					if err != nil || len(entries) != 1 {
						b.Fatalf("search returned %d entries: %v", len(entries), err)
					}
				}
			})
			b.Run("snapshot", func(b *testing.B) {
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					snapshot, err := store.Snapshot(ctx, "")
					if err != nil || len(snapshot.Files) != count {
						b.Fatalf("snapshot returned %d files: %v", len(snapshot.Files), err)
					}
				}
			})
			b.Run("history-batch", func(b *testing.B) {
				status, err := store.Status(ctx)
				if err != nil {
					b.Fatal(err)
				}
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					batch, err := store.HistoryNotes(ctx, status.Revision, 0, nil)
					if err != nil || len(batch.Files) != 32 {
						b.Fatalf("history returned %d files: %v", len(batch.Files), err)
					}
				}
			})
			b.Run("save", func(b *testing.B) {
				doc, err := store.Read(ctx, "n0.md", "")
				if err != nil {
					b.Fatal(err)
				}
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					doc, err = store.Save(ctx, "n0.md", []byte(fmt.Sprintf("# Revision %d\nRare amber evidence.\n", i)), doc.Revision, "")
					if err != nil {
						b.Fatal(err)
					}
				}
			})
		})
	}
}

// BenchmarkReadsF2 measures the reads of an F2-shaped notebook: the final
// files of F2's 2,000-note notebook, notes of 2 to 20 KiB in nested
// folders, committed as one revision, with small images so the fixture stays
// quick to build. The cold reads follow a restart; the others are warm.
func BenchmarkReadsF2(b *testing.B) {
	revs, err := benchfixture.Notebook(benchfixture.NotebookOptions{Revisions: 4400, MinAssetBytes: 4 << 10, MaxAssetBytes: 16 << 10})
	if err != nil {
		b.Fatal(err)
	}
	files := map[string][]byte{}
	for r := range revs {
		for _, c := range r.Changes {
			if c.OldPath != "" {
				delete(files, c.OldPath)
			}
			if c.Content == nil {
				delete(files, c.Path)
			} else {
				files[c.Path] = c.Content
			}
		}
	}
	var changes []notestore.FileChange
	notes, noteBytes := 0, 0
	for _, p := range slices.Sorted(maps.Keys(files)) {
		changes = append(changes, notestore.FileChange{Path: p, Content: files[p]})
		if strings.HasSuffix(p, ".md") && !IsTrash(p) {
			notes, noteBytes = notes+1, noteBytes+len(files[p])
		}
	}
	store := newTestStore(b)
	commitFixture(b, store, changes...)
	ctx := context.Background()
	listed, err := store.List(ctx)
	if err != nil {
		b.Fatal(err)
	}
	b.Logf("%d files, %d notes of %d bytes, %d listed", len(files), notes, noteBytes, len(listed))
	cold := func(name string, read func(*Store) error) {
		b.Run(name, func(b *testing.B) {
			for b.Loop() {
				b.StopTimer()
				store = reopenTestStore(b, store)
				b.StartTimer()
				if err := read(store); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
	cold("cold-list", func(s *Store) error { _, err := s.List(ctx); return err })
	cold("cold-find", func(s *Store) error { _, err := s.Find(ctx, "Harbor", "", "", ""); return err })
	// Warm the caches a search adds to a listing's.
	if _, err := store.Find(ctx, "Harbor", "", "", ""); err != nil {
		b.Fatal(err)
	}
	for _, read := range []struct {
		name string
		read func() error
	}{
		{"list", func() error { _, err := store.List(ctx); return err }},
		{"browse", func() error { _, err := store.BrowseCollection(ctx, "", ""); return err }},
		{"browse-folder", func() error { _, err := store.BrowseCollection(ctx, "Projects", ""); return err }},
		{"collections", func() error { _, err := store.Collections(ctx); return err }},
		{"find-tag", func() error { _, err := store.Find(ctx, "", "research", "", ""); return err }},
		{"find-rare", func() error { _, err := store.Find(ctx, "Harbor", "", "", ""); return err }},
		{"find-common", func() error { _, err := store.Find(ctx, "the", "", "", ""); return err }},
		{"find-none", func() error { _, err := store.Find(ctx, "zzqx", "", "", ""); return err }},
		{"find-accented", func() error { _, err := store.Find(ctx, "café", "", "", ""); return err }},
	} {
		b.Run(read.name, func(b *testing.B) {
			for b.Loop() {
				if err := read.read(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
	b.Run("browse-after-save", func(b *testing.B) {
		target := ""
		for _, e := range listed {
			if strings.HasSuffix(e.Path, ".md") && e.Editable {
				target = e.Path
				break
			}
		}
		doc, err := store.Read(ctx, target, "")
		if err != nil {
			b.Fatal(err)
		}
		for i := 0; b.Loop(); i++ {
			b.StopTimer()
			if doc, err = store.Save(ctx, target, slices.Concat(doc.Content, []byte(fmt.Sprintf("\nEdit %d.\n", i))), doc.Revision, ""); err != nil {
				b.Fatal(err)
			}
			b.StartTimer()
			if _, err := store.BrowseCollection(ctx, "", ""); err != nil {
				b.Fatal(err)
			}
		}
	})
}
