package retrieval

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestLexiconWeightsDuplicatesVisibilityAndCancellation(t *testing.T) {
	fields := [][4]string{{"saffron saffron mild", "Other", "a.md", ""}, {"saffron mild mild mild", "Saffron", "b.md", ""}}
	index, err := NewLexicon[uint32](t.Context(), len(fields), func(i int) [4]string { return fields[i] }, Tokens, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	weights := [4]float64{1, 3, 2, 0}
	scores, err := index.Scores(t.Context(), []string{"saffron"}, weights, nil)
	if err != nil || len(scores) != 2 || scores[1] <= scores[0] {
		t.Fatalf("title boost lost: %v %v", scores, err)
	}
	repeated, err := index.Scores(t.Context(), []string{"saffron", "saffron"}, weights, nil)
	if err != nil || !reflect.DeepEqual(scores, repeated) {
		t.Fatal("duplicate terms multiply evidence", repeated, err)
	}
	filtered, err := index.Scores(t.Context(), []string{"saffron"}, weights, func(doc int, _ string) bool { return doc == 0 })
	if err != nil || len(filtered) != 1 || filtered[0] != scores[0] {
		t.Fatal("visibility changes corpus weights or leaks a document", filtered, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = index.Scores(ctx, nil, weights, nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err = NewLexicon[uint32](ctx, 0, nil, Tokens, 1<<20); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err = NewLexicon[uint32](t.Context(), 100, func(int) [4]string { t.Fatal("unadmitted corpus allocated"); return [4]string{} }, Tokens, 1); !errors.Is(err, ErrLexiconLimit) {
		t.Fatal(err)
	}
}
