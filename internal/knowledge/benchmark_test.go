package knowledge

import (
	"context"
	"fmt"
	"testing"

	"forgetmenote/internal/notestore"
)

func BenchmarkSemanticSearch(b *testing.B) {
	for _, count := range []int{1000, 5000} {
		b.Run(fmt.Sprintf("%d_passages_768_dimensions", count), func(b *testing.B) {
			store := newNotebook(b, nil)
			notes := make([]notestore.FileChange, count)
			for i := range notes {
				notes[i] = notestore.FileChange{Path: fmt.Sprintf("note-%05d.md", i), Content: []byte(fmt.Sprintf("# Note %d\nA unique observation %d.", i, i))}
			}
			commitFixture(b, store, "Synthetic benchmark", notes...)
			e, _ := Open(database(b, store), store)
			config := Config{Enabled: true, Model: "fixture-768", Dimensions: 768, PassageRunes: 256, Splitter: "markdown-heading-v1"}
			e.Configure(context.Background(), config, false)
			// Exclude population/serialization from the query measurement. Search still refreshes HEAD and applies filters.
			vector := make([]float32, 768)
			for i := range vector {
				vector[i] = 1
			}
			vector, _ = normalize(vector, 768)
			for _, p := range e.passages {
				e.cache.Vectors[p.Hash] = vector
			}
			e.cache.Revision = e.snapshot.Revision
			b.Run("query", func(b *testing.B) {
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if _, err := e.Search(context.Background(), SearchQuery{Vector: vector, ConfigID: config.ID(), Limit: 20}); err != nil {
						b.Fatal(err)
					}
				}
			})
			b.Run("rebuild", func(b *testing.B) {
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					status, err := e.Configure(context.Background(), config, true)
					if err != nil || status.Total != count {
						b.Fatalf("rebuild returned %d passages: %v", status.Total, err)
					}
				}
			})

		})
	}
}
