package knowledge

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestRetrieveQuestionTermsExcludeFramingOnlyEvidence(t *testing.T) {
	e := retrievalNotes(t, map[string]string{
		"bicycle.md":    "# Bicycle workshop\nBicycle maintenance requires chain lubrication and brake inspection.",
		"office.md":     "# Office\nTell me about the attendance roster. The office can show me the options for badge colors.",
		"travel.md":     "# Travel\nPlease tell me about the arrival gate. I can tell you where the lounge is.",
		"solar.md":      "# Solar\nSolar output is 19 kWh daily.",
		"wind.md":       "# Wind\nWind output is 14 kWh daily.",
		"café.md":       "# Café Reykjavík\nNaïve sketches decorate this quiet café.",
		"me.md":         "# Me\nA synthetic project deliberately named with a common word.",
		"about-time.md": "# About Time\nA synthetic clock project.",
		"who.md":        "# The Who\nA synthetic group name for this matching fixture.",
	})
	for _, tc := range []struct {
		query string
		paths []string
	}{
		{"Tell me about the bicycle maintenance", []string{"bicycle.md"}},
		{"Could you tell me about quantum chromodynamics?", nil},
		{"Please compare solar and wind output", []string{"solar.md", "wind.md"}},
		{"Tell me about Café Reykjavík", []string{"café.md"}},
		{"Me", []string{"me.md"}},
		{"About Time", []string{"about-time.md"}},
		{`Tell me about "The Who"`, []string{"who.md"}},
	} {
		t.Run(tc.query, func(t *testing.T) {
			got, err := e.Retrieve(t.Context(), RetrievalQuery{Text: tc.query, LocalOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]bool{}
			for _, p := range tc.paths {
				want[p] = true
			}
			seen := map[string]bool{}
			for _, p := range got.Passages {
				if !want[p.Path] {
					t.Errorf("framing-only unrelated evidence selected: %s", p.Path)
				}
				seen[p.Path] = true
			}
			for p := range want {
				if !seen[p] {
					t.Errorf("missing topical/entity evidence: %s", p)
				}
			}
		})
	}
	sidebar, err := e.HybridSearch(t.Context(), HybridQuery{Mode: "text", SearchQuery: SearchQuery{Text: "tell me about"}})
	if err != nil || len(sidebar.Matches) != 2 {
		t.Fatalf("sidebar literal phrase behavior changed: matches=%d error=%v", len(sidebar.Matches), err)
	}
}

func TestRetrieveColdQueryEmbeddingWaitsWithinCallerBudget(t *testing.T) {
	e := retrievalNotes(t, map[string]string{"bicycle.md": "# Bicycle workshop\nChain inspection and brake repairs.", "office.md": "# Office\nThe attendance roster is available."})
	calls := syntheticRetrievalEndpoint(t, e, func(w http.ResponseWriter, r *http.Request) {
		timer := time.NewTimer(2300 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-r.Context().Done():
			return
		case <-timer.C:
		}
		json.NewEncoder(w).Encode(map[string]any{"embeddings": [][]float32{{1, 0, 0}}})
	})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	got, err := e.Retrieve(ctx, RetrievalQuery{Text: "two-wheeler upkeep"})
	if err != nil || got.Mode != "hybrid" || len(got.Passages) == 0 || calls.Load() != 1 {
		t.Fatalf("cold model was abandoned before caller budget: mode=%s passages=%d calls=%d err=%v status=%s", got.Mode, len(got.Passages), calls.Load(), err, got.Message)
	}
	for _, p := range got.Passages {
		if p.Path != "bicycle.md" {
			t.Error("unrelated semantic original selected")
		}
	}
}

func TestRetrievePartialTopicEvidenceIsExplicitlyWeak(t *testing.T) {
	e := retrievalNotes(t, map[string]string{"tortoise.md": "# Desert tortoise\nTortoises shelter in burrows during hot afternoons."})
	got, err := e.Retrieve(t.Context(), RetrievalQuery{Text: "Tell me about tortoise vaccination schedule", LocalOnly: true})
	if err != nil || len(got.Passages) != 1 {
		t.Fatal(got, err)
	}
	if !strings.Contains(got.Message, "Weak") || !strings.Contains(got.Message, "absence") {
		t.Fatalf("partial topic match could certify absence: %s", got.Message)
	}
}
