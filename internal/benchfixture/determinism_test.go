package benchfixture

import (
	"encoding/json"
	"testing"
)

// The generators promise the same bytes for a seed on every platform and Go
// release. These pins fail when an edit changes any fixture; update them only
// for an intended fixture change, and say so in the commit.
const (
	pinnedNovelHistoryHead = "11e0bb36efb4d22481370b9a86a34e5773259158"
	pinnedNotebookHead     = "828abd232ec256212d18e6dfaa6f4b1b3c5cecd9"
	pinnedTracesSHA256     = "060282c761b08935ecb2aa200cb464bd3cac3a2c889728a55050edea59a725e0"
)

func TestGeneratorsArePinned(t *testing.T) {
	seq, err := NovelHistory(testNovel(6), NovelHistoryOptions{Seed: 1, Edits: 50})
	if err != nil {
		t.Fatal(err)
	}
	revs := collect(seq)
	if head := revs[len(revs)-1].Commit; head != pinnedNovelHistoryHead {
		t.Errorf("F1h head %s, pinned %s", head, pinnedNovelHistoryHead)
	}
	_, notebook := runNotebook(t, smallNotebook)
	if head := notebook[len(notebook)-1].Commit; head != pinnedNotebookHead {
		t.Errorf("F2 head %s, pinned %s", head, pinnedNotebookHead)
	}
	traces, err := Traces(testNovel(40), 5)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(traces)
	if sum := sha256Hex(raw); sum != pinnedTracesSHA256 {
		t.Errorf("T1 SHA-256 %s, pinned %s", sum, pinnedTracesSHA256)
	}
}
