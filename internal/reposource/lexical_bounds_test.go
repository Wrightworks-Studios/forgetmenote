package reposource

import (
	"errors"
	"strings"
	"testing"
)

func TestSourceLexiconRefusesFrequencyOverflow(t *testing.T) {
	// A malformed persisted chunk must not wrap its frequency counter to zero.
	idx := sourceIndex{Chunks: []SourceChunk{{Path: "guide.md", Text: strings.Repeat("saffron ", 1<<16)}}}
	if _, err := newSourceLexicon(t.Context(), idx); !errors.Is(err, ErrLimit) {
		t.Fatalf("wrapped lexical frequency admitted: %v", err)
	}
}
