package knowledge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"forgetmenote/internal/retrieval"
	"net/url"
	"strconv"
	"strings"
	"time"

	"forgetmenote/internal/workspace"
)

type RetrievalQuery struct {
	PathPrefix   string      `json:"pathPrefix,omitempty"`
	SemanticOnly bool        `json:"-"`
	Jev          *JevRanking `json:"-"`
	ContainerSelection
	contentScope workspace.ContentScope
	// EmbeddingWait is a server-owned sub-budget, never a tool argument.
	EmbeddingWait   time.Duration `json:"-"`
	PerNotePassages int           `json:"perNotePassages,omitempty"`
	Text            string        `json:"text"`
	Scope           string        `json:"scope,omitempty"`
	Limit           int           `json:"limit,omitempty"`
	TokenBudget     int           `json:"tokenBudget,omitempty"`
	IncludeTrash    bool          `json:"includeTrash,omitempty"`
	LocalOnly       bool          `json:"localOnly,omitempty"`
}
type RetrievalResult struct {
	Reranked        bool                   `json:"reranked"`
	ContentScope    workspace.ContentScope `json:"contentScope"`
	ScopeID         string                 `json:"scopeId"`
	Visibility      string                 `json:"visibility"`
	Semantic        string                 `json:"semantic"`
	Weak            bool                   `json:"weak"`
	Passages        []Passage              `json:"passages"`
	Mode            string                 `json:"mode"`
	Message         string                 `json:"message,omitempty"`
	Revision        string                 `json:"revision"`
	EstimatedTokens int                    `json:"estimatedTokens"`
	History         *HistoryStatus         `json:"history,omitempty"`
}

type retrievalInput struct {
	index                *bm25Index
	revision, visibility string
	config               Config
	provider             *Endpoint
	vectors              map[string][]float32
	history              *HistoryStatus
}

// Retrieve returns complete committed passages. Private browser drafts and live
// collaborative text require explicit attachments at the chat boundary.
func (e *Engine) Retrieve(ctx context.Context, query RetrievalQuery) (RetrievalResult, error) {
	scope, err := e.ResolveContentScope(query.ContainerSelection, false)
	if err != nil {
		return RetrievalResult{}, err
	}
	query.contentScope = scope
	result := RetrievalResult{Passages: []Passage{}, Mode: "bm25", Semantic: "disabled", ContentScope: scope, ScopeID: scope.ID()}
	query.Text = strings.TrimSpace(query.Text)
	if query.Text == "" || len(query.Text) > 2000 || len(retrievalTokens(query.Text)) == 0 {
		return result, errors.New("enter retrieval text of 1–2000 bytes containing words")
	}
	if query.Scope != "" && query.Scope != "current" && query.Scope != "history" {
		return result, errors.New("retrieval scope must be current or history")
	}
	if query.Limit < 0 || query.Limit > 50 || query.PerNotePassages < 0 || query.PerNotePassages > 50 || query.TokenBudget < 0 || query.TokenBudget > 24000 {
		return result, errors.New("retrieval accepts at most 50 passages, 50 primary passages per note and 24000 estimated evidence tokens")
	}
	if query.Limit == 0 {
		query.Limit = retrieval.DefaultLimit
	}
	if query.PerNotePassages == 0 {
		query.PerNotePassages = retrieval.DefaultPerFile
	}
	if query.TokenBudget == 0 {
		query.TokenBudget = retrieval.DefaultTokenBudget
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	input, err := e.retrievalInput(ctx, query, 16)
	if err != nil {
		return result, err
	}
	if input.config.Enabled && input.config.Endpoint {
		result.Semantic = "unavailable"
		if input.provider != nil && len(input.vectors) == 0 {
			result.Semantic = "unindexed"
		}
	}
	vector := []float32(nil)
	result.Message = "BM25 retrieves matching committed passages; results are selected evidence, not exhaustive coverage."
	switch {
	case query.LocalOnly:
		result.Semantic = "local_only"
		result.Message = "Local-only retrieval uses BM25; query embeddings are disabled. Results are selected evidence, not exhaustive coverage."
	case input.config.Enabled && input.config.Endpoint && input.provider != nil && len(input.vectors) > 0:
		wait := query.EmbeddingWait
		if wait <= 0 || wait > 30*time.Second {
			wait = 30 * time.Second
		}
		searchCtx, cancel := context.WithTimeout(ctx, wait)
		embedded, embedErr := input.provider.Embed(searchCtx, input.config, []string{query.Text})
		timedOut := errors.Is(searchCtx.Err(), context.DeadlineExceeded)
		cancel()
		if ctx.Err() != nil {
			return RetrievalResult{}, ctx.Err()
		}
		if embedErr == nil {
			vector, embedErr = normalize(embedded[0], input.config.Dimensions)
		}
		result.Semantic = "ready"
		if embedErr != nil {
			result.Semantic = "unavailable"
			if timedOut {
				result.Semantic = "timeout"
			}
			result.Message = "Query embeddings are unavailable; using BM25 committed passages."
		}
		fresh, refreshErr := e.retrievalInput(ctx, query, 0)
		if refreshErr != nil {
			return RetrievalResult{}, refreshErr
		}
		if input.index.key != fresh.index.key || input.config != fresh.config || input.provider != fresh.provider {
			vector = nil
			result.Semantic = "changed"
			result.Message = "Notes, visibility or embedding configuration changed during retrieval; using current BM25 passages."
		}
		input = fresh
	}
	ranked, err := input.index.rank(ctx, query.Text)
	if err != nil {
		return RetrievalResult{}, err
	}
	ranked = diverseRetrievalCandidates(input.index, ranked, 100, query.PerNotePassages)
	if query.SemanticOnly {
		ranked = nil
		result.Mode = "meaning"
	}
	if vector != nil {
		semantic := []passageRank{}
		complete := true
		for i, doc := range input.index.documents {
			if err := ctx.Err(); err != nil {
				return RetrievalResult{}, err
			}
			values, ok := input.vectors[doc.passage.Hash]
			if !ok {
				complete = false
				continue
			}
			score := 0.0
			for j, v := range vector {
				score += float64(v) * float64(values[j])
			}
			// Nonpositive similarity supplies no directional evidence. Remaining scores
			// only determine rank; they never establish relevance or factual confidence.
			if score > 0 {
				semantic = append(semantic, passageRank{index: i, score: score})
			}
		}
		input.index.sort(semantic)
		semantic = diverseRetrievalCandidates(input.index, semantic, 100, query.PerNotePassages)
		combined := map[int]passageRank{}
		for rank, p := range ranked {
			p.score = retrieval.ReciprocalRank(rank)
			combined[p.index] = p
		}
		for rank, p := range semantic {
			old := combined[p.index]
			old.index = p.index
			old.score += retrieval.ReciprocalRank(rank)
			combined[p.index] = old
		}
		ranked = ranked[:0]
		for _, p := range combined {
			ranked = append(ranked, p)
		}
		input.index.sort(ranked)
		result.Mode = "hybrid"
		if query.SemanticOnly {
			result.Mode = "meaning"
		}
		result.Message = "BM25 and semantic ranks select committed evidence; semantic similarity does not establish relevance or exhaustive coverage."
		// Duplicate passage occurrences share vectors; only missing required
		// hashes make coverage incomplete. Unrelated cached vectors cannot fill gaps.
		if !complete {
			result.Message += " Semantic indexing is incomplete."
		}
	}
	result.Revision = input.revision
	if query.SemanticOnly && vector == nil {
		result.Message = "Meaning search is unavailable; enable embeddings and wait for semantic coverage."
	}
	result.Visibility = input.visibility
	result.History = input.history
	if input.history != nil {
		if input.history.CompletedRevision != input.history.Revision {
			result.Message += " Historical scan is incomplete."
		}
		if input.history.Message != "" {
			result.Message += " " + input.history.Message
		}
	}
	ranked, result.Reranked, err = e.rerankJev(ctx, query, input, ranked)
	if err != nil {
		return RetrievalResult{}, err
	}
	result.Passages, result.EstimatedTokens = packRetrieval(input.index, ranked, query.Limit, query.TokenBudget, query.PerNotePassages)
	result.Weak = input.index.weakQueryCoverage(query.Text, result.Passages)
	if result.Weak {
		result.Message += " Weak topical coverage: selected originals do not match every substantive question term. Semantic candidates may still answer a paraphrase; inspect their content. Missing matches do not establish absence from the notebook."
	}
	if len(result.Passages) == 0 {
		if len(ranked) > 0 {
			result.Message += " No complete matching passage fits the evidence budget."
		} else {
			result.Message += " No matching evidence was found."
		}
	}
	// Even a cache hit must prove the current owner, commit and visibility before
	// publication. This also catches policy changes after the embedding response.
	status, err := e.Store.Status(ctx)
	if err != nil {
		return RetrievalResult{}, err
	}
	visibility, err := e.Store.HistoryVisibilityID(ctx)
	if err != nil {
		return RetrievalResult{}, err
	}
	if status.Revision != input.revision || visibility != input.visibility {
		return RetrievalResult{}, fmt.Errorf("notes or visibility changed during retrieval; retry: %w", workspace.ErrConflict)
	}
	if err := ctx.Err(); err != nil {
		return RetrievalResult{}, err
	}
	return result, nil
}

func (e *Engine) retrievalInput(ctx context.Context, query RetrievalQuery, batch int) (retrievalInput, error) {
	if query.contentScope == (workspace.ContentScope{}) {
		scope, err := e.ResolveContentScope(query.ContainerSelection, false)
		if err != nil {
			return retrievalInput{}, err
		}
		query.contentScope = scope
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.refreshStored(ctx); err != nil {
		return retrievalInput{}, err
	}
	input := retrievalInput{config: e.historyConfig(), provider: e.provider.Load(), vectors: map[string][]float32{}}
	// syncHistory also schedules the existing embedding worker. A local-only run
	// must not cause a historical corpus to be sent there indirectly.
	if query.LocalOnly && query.Scope == "history" && input.config.Enabled && input.config.Endpoint {
		return input, errors.New("local-only history cannot scan while endpoint indexing is enabled; use current notes or disable the project's embedding endpoint")
	}
	visibility, err := e.Store.HistoryVisibilityID(ctx)
	if err != nil {
		return input, err
	}
	snapshot, err := e.Store.SnapshotScoped(ctx, "", query.contentScope)
	if err != nil {
		return input, err
	}
	input.revision, input.visibility = snapshot.Revision, visibility
	titles := map[string]string{}
	passages := []Passage{}
	var binding strings.Builder
	fmt.Fprintf(&binding, "%s\x00%s\x00%s\x00%t\x00%s\x00%s", snapshot.Revision, visibility, input.config.ID(), query.IncludeTrash, query.contentScope.ID(), query.PathPrefix)
	if query.Scope == "history" {
		if err = e.syncHistory(ctx, batch); err != nil {
			return input, err
		}
		// The catalogue releases the index lock during Git reads. Bind splitting to
		// the configuration that survived those reads, including concurrent rebuilds.
		input.config, input.provider = e.historyConfig(), e.provider.Load()
		binding.Reset()
		fmt.Fprintf(&binding, "%s\x00%s\x00%s\x00%t\x00%s\x00%s", snapshot.Revision, visibility, input.config.ID(), query.IncludeTrash, query.contentScope.ID(), query.PathPrefix)
		versions, _, err := e.searchableHistory(ctx, query.IncludeTrash, &query.contentScope)
		if err != nil {
			return input, err
		}
		paths := []string{}
		ids := []string{}
		for id, v := range versions {
			paths = append(paths, v.Path)
			ids = append(ids, id)
		}
		allowed, err := e.Store.VisibleHistoryPaths(ctx, paths)
		if err != nil {
			return input, err
		}
		e.sortHistoryVersions(ids, versions, &query.contentScope)
		truncated := false
		for _, id := range ids {
			if err := ctx.Err(); err != nil {
				return input, err
			}
			v := versions[id]
			if !strings.HasPrefix(v.Path, query.PathPrefix) {
				continue
			}
			if !allowed[v.Path] {
				continue
			}
			file := e.history.Blobs[v.Blob]
			file.Path = v.Path
			titles[file.Path+"\x00"+file.Hash] = noteTitle(file)
			for _, p := range e.historyParts(v) {
				if len(passages) >= 10000 {
					truncated = true
					break
				}
				p.Path, p.Revision = v.Path, v.Revisions[0].Revision
				p.Priority = query.contentScope.Tier(file.Metadata)
				p.ID = workspace.ContentHash([]byte(id + "\x00" + p.ID))
				citation := url.URL{Path: "/" + p.Path, Fragment: p.Anchor, RawQuery: url.Values{"fmn-rev": {p.Revision}, "fmn-passage": {p.Hash}, "fmn-chars": {strconv.Itoa(input.config.PassageRunes)}}.Encode()}
				p.Citation = citation.String()
				passages = append(passages, p)
				fmt.Fprintf(&binding, "\x00%s\x00%s", p.ID, p.Revision)
			}
			if truncated {
				break
			}
		}
		h := e.historyStatus()
		input.history = &h
		if truncated {
			h.State = "limited"
			h.Message = strings.TrimSpace(h.Message + " Historical retrieval is limited to 10000 passage occurrences; evidence coverage is partial.")
		}
		input.index, err = newBM25(ctx, workspace.ContentHash([]byte(binding.String())), passages, titles)
		if err != nil {
			return input, err
		}
	} else {
		visible := snapshot
		visible.Files = nil
		for _, file := range snapshot.Files {
			if !strings.HasPrefix(file.Path, query.PathPrefix) {
				continue
			}
			if !matches(file, SearchQuery{IncludeTrash: query.IncludeTrash}) {
				continue
			}
			visible.Files = append(visible.Files, file)
			titles[file.Path+"\x00"+file.Hash] = noteTitle(file)
			fmt.Fprintf(&binding, "\x00%s\x00%s", file.Path, file.Hash)
		}
		key := workspace.ContentHash([]byte(binding.String()))
		input.index = e.retrieval
		if input.index == nil || input.index.key != key {
			passages, err = Split(visible, input.config)
			if err != nil {
				return input, err
			}
			input.index, err = newBM25(ctx, key, passages, titles)
			if err != nil {
				return input, err
			}
		}
	}
	currentVisibility, err := e.Store.HistoryVisibilityID(ctx)
	if err != nil {
		return input, err
	}
	if visibility != snapshot.Visibility || currentVisibility != visibility {
		return input, fmt.Errorf("note visibility changed during retrieval; retry: %w", workspace.ErrConflict)
	}
	if query.Scope != "history" {
		e.retrieval = input.index
	}
	if e.cache.ConfigID == input.config.ID() {
		for _, doc := range input.index.documents {
			if v, ok := e.cache.Vectors[doc.passage.Hash]; ok {
				input.vectors[doc.passage.Hash] = v
			}
		}
	}
	return input, nil
}

// Estimate the full serialized evidence (including pinned source metadata) at
// three UTF-8 bytes per token. This is a conservative estimate, not model usage.
func retrievalPassageTokens(p Passage) int { raw, _ := json.Marshal(p); return (len(raw) + 4) / 3 }
func packRetrieval(index *bm25Index, ranked []passageRank, limit, budget, perNoteLimit int) ([]Passage, int) {
	selected := []Passage{}
	cost := 0
	chosen := map[int]bool{}
	perNote := map[string]int{}
	add := func(i int) bool {
		p := index.documents[i].passage
		if chosen[i] || len(selected) >= limit {
			return false
		}
		normalized := strings.ToLower(strings.Join(strings.Fields(p.Text), " "))
		for _, old := range selected {
			previous := strings.ToLower(strings.Join(strings.Fields(old.Text), " "))
			if p.Hash == old.Hash || normalized == previous {
				return false
			}
			shorter, longer := normalized, previous
			if len(shorter) > len(longer) {
				shorter, longer = longer, shorter
			}
			if len(shorter) >= 64 && len(shorter)*5 >= len(longer)*4 && strings.Contains(longer, shorter) {
				return false
			}
		}
		tokens := retrievalPassageTokens(p)
		if tokens > budget-cost {
			return false
		}
		selected = append(selected, p)
		cost += tokens
		chosen[i] = true
		perNote[p.Path]++
		return true
	}
	for tier := 0; tier <= 1; tier++ {
		primary := []int{}
		for _, p := range ranked {
			if index.documents[p.index].passage.Priority != tier {
				continue
			}
			if len(primary) >= min(limit, 4) {
				break
			}
			if perNote[index.documents[p.index].passage.Path] == 0 && add(p.index) {
				primary = append(primary, p.index)
			}
		}
		for _, p := range ranked {
			if index.documents[p.index].passage.Priority != tier {
				continue
			}
			if perNote[index.documents[p.index].passage.Path] >= perNoteLimit {
				continue
			}
			if add(p.index) {
				primary = append(primary, p.index)
			}
		}
		// Neighbors are optional, complete and from the same pinned section. All
		// ranked evidence in this tier is considered first; lower-priority notes
		// cannot consume the budget before included evidence or its neighbors.
		for _, i := range primary {
			p := index.documents[i].passage
			for _, j := range []int{i - 1, i + 1} {
				if j < 0 || j >= len(index.documents) {
					continue
				}
				neighbor := index.documents[j].passage
				if neighbor.Path == p.Path && neighbor.Revision == p.Revision && neighbor.SectionHash == p.SectionHash && perNote[p.Path] < perNoteLimit+2 {
					add(j)
				}
			}
		}
	}
	return selected, cost
}

// Bound concentration before truncation so one long note cannot consume the
// entire lexical or semantic candidate budget ahead of comparison evidence.
func diverseRetrievalCandidates(index *bm25Index, ranked []passageRank, limit, perNoteLimit int) []passageRank {
	out := make([]passageRank, 0, min(limit, len(ranked)))
	counts := map[string]int{}
	for _, p := range ranked {
		path := index.documents[p.index].passage.Path
		if counts[path] >= perNoteLimit {
			continue
		}
		counts[path]++
		out = append(out, p)
		if len(out) == limit {
			break
		}
	}
	return out
}
