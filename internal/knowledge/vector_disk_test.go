package knowledge

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBinaryVectorStoragePreservesBitsAndRejectsDamage(t *testing.T) {
	config := testConfig()
	vector, err := normalize([]float32{1, 2, 3}, 3)
	if err != nil {
		t.Fatal(err)
	}
	key := strings.Repeat("a", 64)
	cache := vectorCache{ConfigID: config.ID(), Revision: strings.Repeat("a", 40), Vectors: map[string][]float32{key: vector}}
	file := filepath.Join(t.TempDir(), "cache", "semantic.bin")
	if err = writeVectorCache(file, cache, 3, syncKnowledgeDirectory); err != nil {
		t.Fatal(err)
	}
	loaded, _, err := readVectorCache(file, config)
	if err != nil || loaded.ConfigID != cache.ConfigID || loaded.Revision != cache.Revision {
		t.Fatal("binary roundtrip", err)
	}
	for i, v := range vector {
		if loaded.Vectors[key][i] != v {
			t.Fatal("binary roundtrip changed a vector")
		}
	}
	original, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("checksum", func(t *testing.T) {
		broken := bytes.Clone(original)
		firstVector := bytes.Index(broken, []byte(key)) + len(key)
		broken[firstVector] ^= 1
		if err = os.WriteFile(file, broken, 0600); err != nil {
			t.Fatal(err)
		}
		if _, _, err = readVectorCache(file, config); err == nil || !strings.Contains(err.Error(), "integrity") {
			t.Fatalf("finite altered vector escaped integrity check: %v", err)
		}
	})
	for _, damage := range []struct {
		name string
		raw  []byte
	}{
		{"truncated", original[:len(original)-1]},
		{"trailing", append(bytes.Clone(original), 1)},
		{"future", append([]byte("FMNVEC3\n"), original[len(vectorMagic):]...)},
	} {
		t.Run(damage.name, func(t *testing.T) {
			if err = os.WriteFile(file, damage.raw, 0600); err != nil {
				t.Fatal(err)
			}
			if _, _, err = readVectorCache(file, config); err == nil {
				t.Fatal("invalid binary cache accepted")
			}
		})
	}
	t.Run("bounded-count", func(t *testing.T) {
		// A checksummed hostile header must be rejected before its count is used
		// to allocate a map; it cannot claim rows absent from the actual file.
		header := []byte(`{"version":2,"configId":"` + config.ID() + `","revision":"","dimensions":3,"count":1000000}`)
		var malformed bytes.Buffer
		malformed.WriteString(vectorMagic)
		binary.Write(&malformed, binary.LittleEndian, uint32(len(header)))
		malformed.Write(header)
		digest := sha256.Sum256(malformed.Bytes())
		malformed.Write(digest[:])
		if err = os.WriteFile(file, malformed.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
		if _, _, err = readVectorCache(file, config); err == nil {
			t.Fatal("unbounded header count accepted")
		}
	})
}

func TestBinaryVectorBudgetRejectionPreservesPublishedCache(t *testing.T) {
	config := testConfig()
	cache := vectorCache{ConfigID: config.ID(), Vectors: map[string][]float32{strings.Repeat("b", 64): {1, 0, 0}}}
	file := filepath.Join(t.TempDir(), "semantic.bin")
	if err := writeVectorCache(file, cache, 3, syncKnowledgeDirectory); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("FMN_LIMIT_INDEX_BYTES", "128")
	if err = writeVectorCache(file, cache, 3, syncKnowledgeDirectory); err == nil {
		t.Fatal("oversized binary file published")
	}
	after, err := os.ReadFile(file)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("failed write replaced paid vectors", err)
	}
}

func TestCorruptStoredVectorsFailClosedAtOpen(t *testing.T) {
	engine, _ := indexFixture(t)
	if _, err := engine.Configure(t.Context(), testConfig(), false); err != nil {
		t.Fatal(err)
	}
	fillIndex(t, engine, "")
	engine.Close()
	if _, err := engine.db.Pool.Exec(t.Context(), `UPDATE knowledge_vectors SET vector = substring(vector from 2) WHERE org = $1 AND notebook = $2`, engine.db.Org, engine.notebook); err != nil {
		t.Fatal(err)
	}
	damaged := storedVectors(t, engine)
	reopened, err := Open(database(t, engine.Store), engine.Store)
	if err == nil {
		reopened.Close()
		t.Fatal("damaged paid cache silently became a fresh embedding request")
	}
	if !errors.Is(err, ErrVectorCache) {
		t.Fatalf("missing notebook-local cache error: %v", err)
	}
	after := storedVectors(t, engine)
	for hash, raw := range damaged {
		if !bytes.Equal(raw, after[hash]) {
			t.Fatal("failed open changed existing cache")
		}
	}
}
