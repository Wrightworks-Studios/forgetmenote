package knowledge

import (
	"fmt"
	"testing"
)

func BenchmarkNotebookBM25Build(b *testing.B) {
	passages := make([]Passage, 1000)
	titles := map[string]string{}
	for i := range passages {
		p := Passage{Path: fmt.Sprintf("notes/%04d.md", i), NoteHash: fmt.Sprint(i), Text: fmt.Sprintf("Observatory calibration %d pulsars evidence reliability and instrument measurements", i)}
		passages[i] = p
		titles[p.Path+"\x00"+p.NoteHash] = "Observatory measurements"
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := newBM25(b.Context(), "synthetic", passages, titles); err != nil {
			b.Fatal(err)
		}
	}
}
