package reposource

import (
	"fmt"
	"testing"
)

func BenchmarkRepositoryBM25Build(b *testing.B) {
	idx := sourceIndex{}
	for i := 0; i < 1000; i++ {
		idx.Chunks = append(idx.Chunks, SourceChunk{Path: fmt.Sprintf("internal/%04d.go", i), Symbol: "calibrateInstrument", Text: fmt.Sprintf("func calibrateInstrument%d() { verifyCalibration(); measurePulse() }", i)})
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := newSourceLexicon(b.Context(), idx); err != nil {
			b.Fatal(err)
		}
	}
}
