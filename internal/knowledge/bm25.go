package knowledge

import (
	"context"
	"forgetmenote/internal/capacity"
	"forgetmenote/internal/retrieval"
	"path"
	"sort"
	"strings"
)

// BM25 uses k1=1.2 and b=0.75 independently for body, title and path.
// Field weights are 1, 3 and 2. IDF counts a passage once across all fields.
// The index is derived exclusively from currently authorized committed input.

type bm25Document struct {
	passage Passage
	title   string
}
type bm25Index struct {
	key       string
	documents []bm25Document
	terms     *retrieval.Lexicon[uint32]
}
type passageRank struct {
	index int
	score float64
	exact bool
}

// Letter/digit words retain combining marks, use Unicode case folding, and do
// not stem words or interpret note text as query syntax.
func retrievalTokens(text string) []string { return retrieval.Tokens(text) }
func newBM25(ctx context.Context, key string, passages []Passage, titles map[string]string) (*bm25Index, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	index := &bm25Index{key: key, documents: make([]bm25Document, len(passages))}
	for i, p := range passages {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		index.documents[i] = bm25Document{passage: p, title: titles[p.Path+"\x00"+p.NoteHash]}
	}
	terms, err := retrieval.NewLexicon[uint32](ctx, len(passages), func(i int) [4]string {
		doc := index.documents[i]
		return [4]string{doc.passage.Text, doc.title, doc.passage.Path, ""}
	}, retrievalTokens, int64(capacity.IndexBytes.Limit()))
	if err != nil {
		return nil, err
	}
	index.terms = terms
	return index, nil
}

func (index *bm25Index) rank(ctx context.Context, query string) ([]passageRank, error) {
	scores, err := index.terms.Scores(ctx, retrievalQueryTerms(query), [4]float64{1, 3, 2, 0}, func(doc int, term string) bool {
		return !retrievalFraming(term) || retrievalQuotedMatch(index.documents[doc], query)
	})
	if err != nil {
		return nil, err
	}
	for i, doc := range index.documents {
		if retrievalExactName(doc, query) && scores[i] == 0 {
			scores[i] = 1
		}
	}
	ranked := make([]passageRank, 0, len(scores))
	for i, score := range scores {
		doc := index.documents[i]
		exact := retrievalExactName(doc, query) || retrievalQuotedMatch(doc, query)
		ranked = append(ranked, passageRank{i, score, exact})
	}
	index.sort(ranked)
	return ranked, nil
}
func (index *bm25Index) sort(ranked []passageRank) {
	sort.Slice(ranked, func(i, j int) bool {
		a, b := ranked[i], ranked[j]
		pa, pb := index.documents[a.index].passage, index.documents[b.index].passage
		if pa.Priority != pb.Priority {
			return pa.Priority < pb.Priority
		}
		if a.exact != b.exact {
			return a.exact
		}
		if a.score != b.score {
			return a.score > b.score
		}
		if pa.Path != pb.Path {
			return pa.Path < pb.Path
		}
		if pa.Revision != pb.Revision {
			return pa.Revision < pb.Revision
		}
		return a.index < b.index
	})
}

// Question framing is not topical evidence. Keep indexing all original words;
// only the chat BM25 query drops this small English function/request vocabulary.
// Exact names and explicit quoted phrases remain searchable even when their
// words would ordinarily be framing (for example a project named "Me").
func retrievalFraming(term string) bool { return retrieval.Framing(term) }
func retrievalQuotedPhrases(query string) []string {
	phrases := []string{}
	var quoted strings.Builder
	inside := false
	for _, r := range query {
		if r == '"' || r == '“' || r == '”' {
			if inside && quoted.Len() > 0 {
				phrases = append(phrases, strings.Join(retrievalTokens(quoted.String()), " "))
			}
			quoted.Reset()
			inside = !inside
		} else if inside {
			quoted.WriteRune(r)
		}
	}
	return phrases
}
func retrievalQueryTerms(query string) []string {
	protected := map[string]bool{}
	for _, phrase := range retrievalQuotedPhrases(query) {
		for _, term := range retrievalTokens(phrase) {
			protected[term] = true
		}
	}
	terms := []string{}
	seen := map[string]bool{}
	for _, term := range retrievalTokens(query) {
		if !seen[term] && (!retrievalFraming(term) || protected[term]) {
			terms = append(terms, term)
			seen[term] = true
		}
	}
	return terms
}

// FocusedRetrievalQuery preserves explicit quoted names. A caller may use this
// deterministic reformulation for one bounded semantic recovery, never a loop.
func FocusedRetrievalQuery(query string) string {
	if len(retrievalQuotedPhrases(query)) > 0 {
		return strings.TrimSpace(query)
	}
	return strings.Join(retrievalQueryTerms(query), " ")
}
func retrievalExactName(doc bm25Document, query string) bool {
	query = strings.Trim(strings.TrimSpace(query), "\"“”")
	return strings.EqualFold(doc.title, query) || strings.EqualFold(strings.TrimSuffix(path.Base(doc.passage.Path), ".md"), query)
}
func retrievalQuotedMatch(doc bm25Document, query string) bool {
	phrases := retrievalQuotedPhrases(query)
	if len(phrases) == 0 {
		return false
	}
	text := " " + strings.Join(retrievalTokens(doc.title+" "+doc.passage.Path+" "+doc.passage.Text), " ") + " "
	for _, phrase := range phrases {
		if phrase != "" && strings.Contains(text, " "+phrase+" ") {
			return true
		}
	}
	return false
}
func (index *bm25Index) weakQueryCoverage(query string, selected []Passage) bool {
	terms := retrievalQueryTerms(query)
	seen := map[string]bool{}
	ids := map[string]bool{}
	for _, p := range selected {
		ids[p.ID] = true
	}
	for _, doc := range index.documents {
		if !ids[doc.passage.ID] {
			continue
		}
		if retrievalExactName(doc, query) {
			return false
		}
		for _, term := range retrievalTokens(doc.title + " " + doc.passage.Path + " " + doc.passage.Text) {
			seen[term] = true
		}
	}
	if len(terms) == 0 || len(selected) == 0 {
		return true
	}
	for _, term := range terms {
		if !seen[term] {
			return true
		}
	}
	return false
}
