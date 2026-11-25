package knowledge

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestRetrieveSemanticCoverageUsesRequiredHashes(t *testing.T) {
	notes := map[string]string{}
	for i := 0; i < 10; i++ {
		notes[fmt.Sprintf("note-%d.md", i)] = fmt.Sprintf("# Needle %d\nStable calibration evidence for needle %d.", min(i, 7), min(i, 7))
	}
	e := retrievalNotes(t, notes)
	syntheticRetrievalEndpoint(t, e, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"embeddings": [][]float32{{0, 1, 0}}})
	})
	input, err := e.retrievalInput(t.Context(), RetrievalQuery{Text: "needle"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(input.index.documents) != 10 || len(input.vectors) != 8 {
		t.Fatalf("wrong duplicate fixture: occurrences=%d vectors=%d", len(input.index.documents), len(input.vectors))
	}
	for _, d := range input.index.documents {
		if _, ok := input.vectors[d.passage.Hash]; !ok {
			t.Fatal("fixture omitted required vector")
		}
	}
	t.Run("all_required", func(t *testing.T) {
		got, err := e.Retrieve(t.Context(), RetrievalQuery{Text: "needle"})
		if err != nil {
			t.Fatal(err)
		}
		if got.Mode != "hybrid" || got.Semantic != "ready" || len(got.Passages) == 0 {
			t.Fatal("did not exercise live semantic query and committed evidence", got)
		}
		t.Logf("occurrences=%d uniqueVectors=%d retrievalMessage=%q", len(input.index.documents), len(input.vectors), got.Message)
		if strings.Contains(got.Message, "Semantic indexing is incomplete") {
			t.Error("fully indexed duplicate originals incorrectly report incomplete semantic coverage to chat")
		}
	})
	t.Run("missing_required_with_unrelated_extras", func(t *testing.T) {
		e.mu.Lock()
		delete(e.cache.Vectors, input.index.documents[0].passage.Hash)
		for i := 0; i < 10; i++ {
			e.cache.Vectors[fmt.Sprintf("unrelated-vector-%d", i)] = []float32{0, 1, 0}
		}
		e.mu.Unlock()
		got, err := e.Retrieve(t.Context(), RetrievalQuery{Text: "needle"})
		if err != nil {
			t.Fatal(err)
		}
		if got.Mode != "hybrid" || !strings.Contains(got.Message, "Semantic indexing is incomplete") {
			t.Fatal("missing required hash concealed by unrelated cached vectors", got)
		}
	})
}
