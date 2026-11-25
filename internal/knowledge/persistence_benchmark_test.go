package knowledge

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"forgetmenote/internal/capacity"
	"math"
	"path/filepath"
	"testing"
)

// Compare the current portable JSON with a float32 streaming representation
// before choosing a disk migration. Corpus construction is outside measurement.
func BenchmarkVectorPersistenceRepresentation(b *testing.B) {
	cache := vectorCache{ConfigID: "synthetic-768", Revision: "synthetic-revision", Vectors: map[string][]float32{}}
	for row := 0; row < 1000; row++ {
		vector := make([]float32, 768)
		for i := range vector {
			vector[i] = float32(math.Sin(float64(row*768 + i + 1)))
		}
		vector, _ = normalize(vector, 768)
		cache.Vectors[fmt.Sprintf("%064x", row)] = vector
	}
	b.Run("JSON", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			raw, err := json.Marshal(cache)
			if err != nil {
				b.Fatal(err)
			}
			b.ReportMetric(float64(len(raw)), "bytes/output")
		}
	})
	b.Run("float32_prototype", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			var output bytes.Buffer
			for key, vector := range cache.Vectors {
				output.WriteString(key)
				if err := binary.Write(&output, binary.LittleEndian, vector); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(output.Len()), "bytes/output")
		}
	})
}

func BenchmarkVectorPersistenceDisk(b *testing.B) {
	config := Config{Enabled: true, Model: "synthetic-768", Dimensions: 768, PassageRunes: 256, Splitter: "markdown-heading-v1"}
	cache := vectorCache{ConfigID: config.ID(), Vectors: map[string][]float32{}}
	for row := 0; row < 1000; row++ {
		vector := make([]float32, 768)
		for i := range vector {
			vector[i] = float32(math.Sin(float64(row*768 + i + 1)))
		}
		vector, _ = normalize(vector, 768)
		cache.Vectors[fmt.Sprintf("%064x", row)] = vector
	}
	dir := b.TempDir()
	if err := persist(filepath.Join(dir, "semantic.json"), cache); err != nil {
		b.Fatal(err)
	}
	if err := writeVectorCache(filepath.Join(dir, "semantic.bin"), cache, 768, syncKnowledgeDirectory); err != nil {
		b.Fatal(err)
	}
	b.Run("JSON_publish", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if err := persist(filepath.Join(dir, "semantic.json"), cache); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("binary_publish", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if err := writeVectorCache(filepath.Join(dir, "semantic.bin"), cache, 768, syncKnowledgeDirectory); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("JSON_load", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			raw, _, err := readKnowledgeFile(filepath.Join(dir, "semantic.json"), capacity.IndexBytes.Limit())
			var loaded vectorCache
			if err != nil || json.Unmarshal(raw, &loaded) != nil {
				b.Fatal("JSON load", err)
			}
		}
	})
	b.Run("binary_load", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, _, err := readVectorCache(filepath.Join(dir, "semantic.bin"), config); err != nil {
				b.Fatal(err)
			}
		}
	})
}
