package web

import (
	"cmp"
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/knowledge"
	"forgetmenote/internal/retrieval"
	"forgetmenote/internal/webcore"
)

// Memory search ranks single items, never heading-sized passages, so an agent
// gets the few items that bear on its next step instead of its whole memory.
// Text ranking always runs. Meaning ranking also runs under the instance's
// adopted embedding policy while the person's Memory setting is on: each item's
// topic and text is embedded once per process and its vector reused. Vectors are
// derived from the Markdown and never stored; a restart embeds items again.
const (
	memorySearchLimit    = 10
	memorySearchMaxLimit = 50
	memoryQueryChars     = 1000
	memoryEmbedBatch     = 64
	memoryEmbedPerSearch = 512 // items embedded for one search; any others rank by text
	memoryVectorEntries  = 4096
	// Embedding scores differ by model, so a match by meaning alone is judged
	// against the best match: one this far below it is left out.
	memoryMeaningMargin = 0.1
	// A new item this close to a current one nearly repeats it.
	memorySimilarMin = 0.9
)

type memorySearch struct {
	Query string `json:"query"`
	Mode  string `json:"mode"`  // meaning, or text when meaning ranking is unavailable
	Total int    `json:"total"` // current items searched
}

type memoryVectorKey struct {
	endpoint   *knowledge.Endpoint
	model      string
	dimensions int
	text       string
}

// memoryVectors holds item vectors for this process. A vector depends only on the
// endpoint, model and text, and never leaves the server.
type memoryVectors struct {
	mu     sync.Mutex
	values map[memoryVectorKey][]float32
}

func (v *memoryVectors) get(key memoryVectorKey) []float32 {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.values[key]
}

func (v *memoryVectors) put(key memoryVectorKey, vector []float32) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.values == nil {
		v.values = map[memoryVectorKey][]float32{}
	}
	for old := range v.values {
		if len(v.values) < memoryVectorEntries {
			break
		}
		delete(v.values, old) // Map order is unspecified, so this evicts arbitrary entries.
	}
	v.values[key] = vector
}

// memoryQuery normalizes a search query and its limit; an empty query recalls everything.
func memoryQuery(query string, limit int) (string, int, error) {
	query = strings.Join(strings.Fields(query), " ")
	if limit == 0 {
		limit = memorySearchLimit
	}
	if !utf8.ValidString(query) || utf8.RuneCountInString(query) > memoryQueryChars || limit < 1 || limit > memorySearchMaxLimit {
		return "", 0, webcore.Invalid("Query must be at most 1000 characters and limit 1 to 50")
	}
	return query, limit, nil
}

// searchMemory keeps the limit most relevant current items of files, ranked
// across them by reciprocal rank fusion, and drops recent changes, which belong
// to a full recall.
func (s *Server) searchMemory(ctx context.Context, settings catalog.AssistantSettings, query string, limit int, files ...*memoryFile) memorySearch {
	type candidate struct {
		file  *memoryFile
		item  memoryItem
		score float64
	}
	var all []candidate
	var texts []string
	for _, f := range files {
		if f == nil {
			continue
		}
		for _, item := range f.Items {
			all = append(all, candidate{file: f, item: item})
			texts = append(texts, memoryVectorText(item))
		}
		f.Items, f.Changed = []memoryItem{}, nil
	}
	out := memorySearch{Query: query, Mode: "text", Total: len(all)}
	for rank, i := range memoryTextRanking(query, texts) {
		all[i].score += retrieval.ReciprocalRank(rank)
	}
	if ranked, similarity := s.memoryMeaningRanking(ctx, settings, query, texts); ranked != nil {
		out.Mode = "meaning"
		best := similarity[ranked[0]]
		for rank, i := range ranked {
			if all[i].score > 0 || similarity[i] >= best-memoryMeaningMargin { // a text match always counts
				all[i].score += retrieval.ReciprocalRank(rank)
			}
		}
	}
	all = slices.DeleteFunc(all, func(c candidate) bool { return c.score == 0 })
	slices.SortStableFunc(all, func(a, b candidate) int { return cmp.Compare(b.score, a.score) })
	for rank, c := range all[:min(limit, len(all))] {
		c.item.Rank = rank + 1
		c.file.Items = append(c.file.Items, c.item)
	}
	return out
}

// memoryTextRanking returns the indexes of texts sharing a query term, best
// first, scored with the BM25 weights notebook search uses.
func memoryTextRanking(query string, texts []string) []int {
	var terms []string
	for _, framing := range []bool{false, true} { // A query of framing words alone still matches them.
		for _, term := range retrieval.Tokens(query) {
			if (framing || !retrieval.Framing(term)) && !slices.Contains(terms, term) {
				terms = append(terms, term)
			}
		}
		if len(terms) > 0 {
			break
		}
	}
	if len(texts) == 0 || len(terms) == 0 {
		return nil
	}
	counts, lengths, frequency, total := make([]map[string]int, len(texts)), make([]int, len(texts)), map[string]int{}, 0
	for i, text := range texts {
		tokens := retrieval.Tokens(text)
		counts[i], lengths[i] = map[string]int{}, len(tokens)
		total += len(tokens)
		for _, token := range tokens {
			counts[i][token]++
		}
		for _, term := range terms {
			if counts[i][term] > 0 {
				frequency[term]++
			}
		}
	}
	average, scores, ranked := float64(total)/float64(len(texts)), make([]float64, len(texts)), []int{}
	for i := range texts {
		for _, term := range terms {
			scores[i] += retrieval.TermScore(counts[i][term], lengths[i], average, retrieval.IDF(len(texts), frequency[term]), 1)
		}
		if scores[i] > 0 {
			ranked = append(ranked, i)
		}
	}
	slices.SortStableFunc(ranked, func(a, b int) int { return cmp.Compare(scores[b], scores[a]) })
	return ranked
}

func memoryVectorText(item memoryItem) string { return item.Topic + ": " + item.Text }

// memoryMeaning returns the endpoint and policy for meaning ranking, or a nil
// endpoint while the person's Memory setting is off, the instance has adopted no
// embedding policy, or local-only operation withholds the endpoint. The endpoint
// rechecks the instance policy around every request.
func (s *Server) memoryMeaning(settings catalog.AssistantSettings) (*knowledge.Endpoint, catalog.AISettings) {
	if !settings.MaintainMemory || s.indexes == nil {
		return nil, catalog.AISettings{}
	}
	policy, managed := s.catalog.AISettings()
	endpoint := s.chatEmbeddingPolicy(s.indexes.Endpoint())
	if !managed || !policy.Embeddings {
		return nil, policy
	}
	return endpoint, policy
}

// memoryItemVectors returns the vectors it has or could make for texts,
// embedding at most memoryEmbedPerSearch texts this process has no vector for.
func (s *Server) memoryItemVectors(ctx context.Context, endpoint *knowledge.Endpoint, policy catalog.AISettings, texts []string) map[string][]float32 {
	key := func(text string) memoryVectorKey {
		return memoryVectorKey{endpoint, policy.Model, policy.Dimensions, text}
	}
	vectors, missing := map[string][]float32{}, []string{}
	for _, text := range texts {
		if _, seen := vectors[text]; seen {
			continue
		}
		vectors[text] = s.memoryVectors.get(key(text))
		if vectors[text] == nil && len(missing) < memoryEmbedPerSearch {
			missing = append(missing, text)
		}
	}
	for batch := range slices.Chunk(missing, memoryEmbedBatch) {
		embedded, err := endpoint.EmbedModel(ctx, policy.Model, policy.Dimensions, batch, true)
		if err != nil {
			slog.Warn("memory could not embed items", "error", err)
			break
		}
		for i, vector := range embedded {
			vectors[batch[i]] = vector
			s.memoryVectors.put(key(batch[i]), vector)
		}
	}
	return vectors
}

// memoryMeaningRanking returns the indexes of embedded texts, nearest the query
// first, with each text's similarity, or nil when meaning ranking is unavailable.
func (s *Server) memoryMeaningRanking(ctx context.Context, settings catalog.AssistantSettings, query string, texts []string) ([]int, []float64) {
	endpoint, policy := s.memoryMeaning(settings)
	if endpoint == nil || len(texts) == 0 {
		return nil, nil
	}
	queried, err := endpoint.EmbedModel(ctx, policy.Model, policy.Dimensions, []string{query}, false)
	if err != nil {
		slog.Warn("memory search ranks by text only", "error", err)
		return nil, nil
	}
	vectors := s.memoryItemVectors(ctx, endpoint, policy, texts)
	ranked, similarity := []int{}, make([]float64, len(texts))
	for i, text := range texts {
		if vector := vectors[text]; vector != nil {
			similarity[i] = retrieval.Cosine(queried[0], vector)
			ranked = append(ranked, i)
		}
	}
	if len(ranked) == 0 {
		return nil, nil
	}
	slices.SortStableFunc(ranked, func(a, b int) int { return cmp.Compare(similarity[b], similarity[a]) })
	return ranked, similarity
}

// markSimilarMemory names, for each item remember added as new, the current item
// it nearly repeats. Only the agent knows whether the new item updates or merely
// repeats it, so both are kept for the agent to resolve with confirm_memory.
// Embedding the current items here also prepares them for later searches.
func (s *Server) markSimilarMemory(ctx context.Context, settings catalog.AssistantSettings, current, added []memoryItem, out *rememberOutput) {
	endpoint, policy := s.memoryMeaning(settings)
	if endpoint == nil || len(current) == 0 || len(out.Items) != len(added) {
		return
	}
	texts := make([]string, 0, len(current)+len(added))
	for _, item := range slices.Concat(current, added) {
		texts = append(texts, memoryVectorText(item))
	}
	vectors := s.memoryItemVectors(ctx, endpoint, policy, texts)
	for i, result := range out.Items {
		vector := vectors[memoryVectorText(added[i])]
		if result.Duplicate || result.Replaced != "" || result.Disputes != "" || vector == nil {
			continue
		}
		nearest := memorySimilarMin
		for _, item := range current {
			if other := vectors[memoryVectorText(item)]; other != nil && item.ID != result.ID {
				if similarity := retrieval.Cosine(vector, other); similarity >= nearest {
					nearest, out.Items[i].Similar = similarity, item.ID
				}
			}
		}
	}
}
