package reposource

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"forgetmenote/internal/retrieval"
	"forgetmenote/internal/workspace"
)

type SearchQuery struct {
	Scopes          map[string]workspace.ContentScope   `json:"-"`
	RequireExternal bool                                `json:"-"`
	Bindings        map[string]string                   `json:"-"`
	Projects        []string                            `json:"projects"`
	Query           string                              `json:"query"`
	Mode            string                              `json:"mode,omitempty"`
	PathPrefix      string                              `json:"pathPrefix,omitempty"`
	Limit           int                                 `json:"limit,omitempty"`
	Check           func(context.Context, string) error `json:"-"`
	// Rerank is supplied by the authenticated gateway and sees the retrieved pool
	// before evidence limits. It runs outside repository and index locks.
	Rerank func(context.Context, *SearchResult) error `json:"-"`
}
type SourceResult struct {
	ContainerTier int `json:"-"`
	SourceChunk
	Excerpt      string   `json:"excerpt"`
	Project      string   `json:"project"`
	Source       string   `json:"source"`
	RepositoryID int64    `json:"repositoryId"`
	Repository   string   `json:"repository"`
	Revision     string   `json:"revision"`
	Citation     string   `json:"citation"`
	Methods      []string `json:"methods"`
	Score        float64  `json:"-"`
	Exact        bool     `json:"-"`
}
type Coverage struct {
	Generation uint64 `json:"-"`
	Source     string `json:"-"`
	Downloaded string `json:"downloaded"`
	Indexed    string `json:"indexed"`
	Chunks     int    `json:"chunks"`
	Embedded   int    `json:"embedded"`
	Excluded   int    `json:"excluded"`
	Mode       string `json:"mode"`
	Message    string `json:"message,omitempty"`
}
type SearchResult struct {
	Results   []SourceResult      `json:"results"`
	Coverage  map[string]Coverage `json:"coverage"`
	Truncated bool                `json:"truncated"`
	Reranked  bool                `json:"reranked"`
}

func (q SearchQuery) Validate() error {
	if len(q.Projects) < 1 || len(q.Projects) > 10 || strings.TrimSpace(q.Query) == "" || len(q.Query) > 2000 || q.Limit < 0 || q.Limit > retrieval.DefaultLimit {
		return errors.New("select 1–10 notebooks, a 1–2000 byte query, and at most 12 results")
	}
	seen := map[string]bool{}
	for _, p := range q.Projects {
		if !identifier.MatchString(p) || seen[p] {
			return errors.New("invalid or duplicate notebook")
		}
		seen[p] = true
	}
	if q.Mode != "" && q.Mode != "auto" && q.Mode != "text" && q.Mode != "meaning" {
		return errors.New("mode must be auto, text or meaning")
	}
	if q.PathPrefix != "" && !safePath(strings.TrimSuffix(q.PathPrefix, "/")) {
		return ErrDenied
	}
	return nil
}
func (m *Manager) Search(ctx context.Context, q SearchQuery) (SearchResult, error) {
	out := SearchResult{Results: []SourceResult{}, Coverage: map[string]Coverage{}}
	if err := q.Validate(); err != nil {
		return out, err
	}
	if q.Check == nil {
		return out, ErrDenied
	}
	if q.Limit == 0 {
		q.Limit = retrieval.DefaultLimit
	}
	for _, p := range q.Projects {
		if err := q.Check(ctx, p); err != nil {
			return out, err
		}
	}
	queryVectors := map[string][]float32{}
	queryFailures := map[string]bool{}
	candidates := []SourceResult{}
	for _, p := range q.Projects {
		if err := q.Check(ctx, p); err != nil {
			return out, err
		}
		s, err := m.Status(p)
		if err != nil {
			return out, err
		}
		if !s.Connected || s.HistoricalOnly {
			out.Coverage[p] = Coverage{Message: "No connected repository"}
			continue
		}
		var vector []float32
		if q.Bindings != nil && q.Bindings[p] != s.ID {
			return out, ErrDenied
		}
		cfg := s.IndexConfig
		if q.RequireExternal && !cfg.ExternalProcessing {
			return out, ErrDenied
		}
		if q.Mode != "text" && cfg.Embeddings && cfg.ExternalProcessing {
			key := cfg.embeddingID()
			vector = queryVectors[key]
			if vector == nil && !queryFailures[key] && len(queryVectors)+len(queryFailures) < 4 {
				// Validate the connection before disclosing even the question to a provider.
				err = m.read(ctx, p, s.ID, func(_ *cache, current State) error {
					if current.Generation != s.Generation || q.RequireExternal && !current.IndexConfig.ExternalProcessing {
						return ErrConflict
					}
					return q.Check(ctx, p)
				})
				if err != nil {
					return out, err
				}
				m.mu.Lock()
				embed := m.embed
				m.mu.Unlock()
				if embed != nil {
					vv, e := embed(ctx, cfg, []string{q.Query}, false)
					if e == nil && len(vv) == 1 {
						vector = vv[0]
						queryVectors[key] = vector
					} else {
						queryFailures[key] = true
					}
				} else {
					queryFailures[key] = true
				}
			}
		}
		// The notebook lock pins the generation throughout disk/vector reads. Queries
		// admit one source at a time; no unbounded lexical cache accumulates per repo.
		var ranked []SourceResult
		err = m.read(ctx, p, s.ID, func(c *cache, current State) error {
			if current.Generation != s.Generation || q.RequireExternal && !current.IndexConfig.ExternalProcessing {
				return ErrConflict
			}
			if err := q.Check(ctx, p); err != nil {
				return err
			}
			m.indexMu.Lock()
			defer m.indexMu.Unlock()
			cov := Coverage{Generation: current.Generation, Source: current.ID, Downloaded: current.Downloaded, Indexed: current.Index.Commit, Chunks: current.Index.Chunks, Embedded: current.Index.Embedded, Excluded: len(current.Index.Exclusions), Mode: "text"}
			idx, e := m.lexicalIndex(ctx, c, current)
			if e != nil || idx.Source != current.ID || idx.Commit != current.Downloaded || idx.ConfigID != current.indexID() {
				cov.Message = "Index unavailable or updating; refresh and retry"
				out.Coverage[p] = cov
				return nil
			}
			cov.Indexed = idx.Commit
			cov.Chunks = len(idx.Chunks)
			cov.Excluded = len(idx.Exclusions)
			ranked, cov.Embedded, e = rankSourceLexical(ctx, idx, current, q, vector, m.lexical.terms)
			if e != nil {
				return e
			}
			if vector != nil && cov.Embedded > 0 {
				cov.Mode = "hybrid"
				if q.Mode == "meaning" {
					cov.Mode = "meaning"
				}
			}
			if q.Mode != "text" && (vector == nil || cov.Embedded < len(idx.Chunks)) {
				cov.Message = "Semantic coverage unavailable or partial; keyword results remain available"
			}
			out.Coverage[p] = cov
			return nil
		})
		if err != nil {
			return out, err
		}
		candidates = append(candidates, ranked...)
	}
	sort.SliceStable(candidates, func(i, j int) bool { return sourceLess(candidates[i], candidates[j]) })
	if q.Rerank != nil && q.Mode != "text" {
		out.Results = candidates
		if err := q.Rerank(ctx, &out); err != nil {
			return SearchResult{}, err
		}
		candidates = out.Results
		out.Results = []SourceResult{}
	}
	budget := retrieval.NewBudget(q.Limit, 0, retrieval.DefaultPerFile)
	for _, r := range candidates {
		key := r.Project + "\x00" + r.Path
		if budget.Full(key) {
			out.Truncated = true
			continue
		}
		overlap := false
		for _, prior := range out.Results {
			if prior.Project == r.Project && prior.Path == r.Path && prior.StartByte < r.EndByte && r.StartByte < prior.EndByte {
				overlap = true
				break
			}
		}
		if overlap {
			continue
		}
		if !budget.Add(key, retrieval.EvidenceTokens(r.Path, r.Text)) {
			out.Truncated = true
			continue
		}
		out.Results = append(out.Results, r)
	}
	if err := m.displayExcerpts(ctx, q, &out); err != nil {
		return SearchResult{}, err
	}
	// Never return an old commit if a refresh or access change won during search.
	for _, p := range q.Projects {
		if err := q.Check(ctx, p); err != nil {
			return SearchResult{}, err
		}
		err := m.read(ctx, p, "", func(_ *cache, s State) error {
			if out.Coverage[p].Downloaded != "" && (s.Downloaded != out.Coverage[p].Downloaded || s.Generation != out.Coverage[p].Generation || s.ID != out.Coverage[p].Source) {
				return ErrConflict
			}
			return q.Check(ctx, p)
		})
		if out.Coverage[p].Downloaded != "" && err != nil {
			return SearchResult{}, err
		}
	}
	return out, nil
}

// Render selected Markdown ranges from their verified complete source, once per
// file. Parsing runs outside notebook/index locks and never changes provider input.
func (m *Manager) displayExcerpts(ctx context.Context, q SearchQuery, out *SearchResult) error {
	type fileKey struct{ project, source, revision, path string }
	groups := map[fileKey][]int{}
	for i := range out.Results {
		r := &out.Results[i]
		path := strings.ToLower(r.Path)
		if strings.HasSuffix(path, ".md") || strings.HasSuffix(path, ".markdown") {
			key := fileKey{r.Project, r.Source, r.Revision, r.Path}
			groups[key] = append(groups[key], i)
		} else {
			r.Excerpt = r.Text
			if runes := []rune(r.Excerpt); len(runes) > 320 {
				r.Excerpt = string(runes[:320]) + "…"
			}
		}
	}
	for key, indices := range groups {
		if err := q.Check(ctx, key.project); err != nil {
			return err
		}
		file, err := m.File(ctx, key.project, key.source, key.revision, key.path)
		if err != nil {
			return err
		}
		ranges := make([][2]int, len(indices))
		for i, index := range indices {
			r := out.Results[index]
			if r.StartByte < 0 || r.EndByte > len(file.Content) || r.StartByte > r.EndByte || file.Content[r.StartByte:r.EndByte] != r.Text {
				return ErrConflict
			}
			ranges[i] = [2]int{r.StartByte, r.EndByte}
		}
		excerpts := workspace.MarkdownRangeExcerpts([]byte(file.Content), ranges, q.Query)
		for i, index := range indices {
			out.Results[index].Excerpt = excerpts[i]
		}
	}
	return nil
}

func sourceLess(a, b SourceResult) bool {
	if a.ContainerTier != b.ContainerTier {
		return a.ContainerTier < b.ContainerTier
	}
	if a.Exact != b.Exact {
		return a.Exact
	}
	if a.Score != b.Score {
		return a.Score > b.Score
	}
	if a.Project != b.Project {
		return a.Project < b.Project
	}
	if a.Path != b.Path {
		return a.Path < b.Path
	}
	return a.StartByte < b.StartByte
}
func rankSourceLexical(ctx context.Context, idx sourceIndex, s State, q SearchQuery, queryVector []float32, lexicon *sourceLexicon) ([]SourceResult, int, error) {
	values, exact, err := lexicon.scores(ctx, idx, q)
	if err != nil {
		return nil, 0, err
	}
	type rank struct {
		i     int
		score float64
	}
	lex := []rank{}
	semantic := []rank{}
	for i, score := range values {
		if q.notebookTier(s, idx.Chunks[i]) < 0 {
			continue
		}
		lex = append(lex, rank{i, score})
	}
	embedded := 0
	vm, f, e := vectors(idx, s.IndexConfig)
	if e == nil {
		defer f.Close()
		for i, ch := range idx.Chunks {
			if q.notebookTier(s, ch) < 0 {
				continue
			}
			row, ok := vm.Rows[ch.EmbeddingHash]
			if !ok {
				continue
			}
			v, e := readVector(f, row, vm.Dimensions)
			if e != nil {
				return nil, 0, e
			}
			embedded++
			if queryVector != nil && strings.HasPrefix(ch.Path, q.PathPrefix) {
				score := retrieval.Cosine(v, queryVector)
				if score > 0 {
					semantic = append(semantic, rank{i, score})
				}
			}
		}
	}
	order := func(r []rank) {
		sort.SliceStable(r, func(i, j int) bool {
			if a, b := q.notebookTier(s, idx.Chunks[r[i].i]), q.notebookTier(s, idx.Chunks[r[j].i]); a != b {
				return a < b
			}
			if exact[r[i].i] != exact[r[j].i] {
				return exact[r[i].i]
			}
			if r[i].score != r[j].score {
				return r[i].score > r[j].score
			}
			return r[i].i < r[j].i
		})
	}
	order(lex)
	order(semantic)
	capBranch := max(1, 100/len(q.Projects))
	scores := map[int]float64{}
	methods := map[int][]string{}
	if q.Mode != "meaning" {
		for rank, r := range lex[:min(len(lex), capBranch)] {
			scores[r.i] += retrieval.ReciprocalRank(rank)
			methods[r.i] = append(methods[r.i], "text")
		}
	}
	for rank, r := range semantic[:min(len(semantic), capBranch)] {
		scores[r.i] += retrieval.ReciprocalRank(rank)
		methods[r.i] = append(methods[r.i], "meaning")
	}
	expandSourceCandidates(idx, q, scores, methods)
	out := []SourceResult{}
	for i, score := range scores {
		ch := idx.Chunks[i]
		tier := q.notebookTier(s, ch)
		if tier < 0 {
			continue
		}
		out = append(out, SourceResult{ContainerTier: tier, SourceChunk: ch, Project: s.Project, Source: s.ID, RepositoryID: s.Repository.ID, Repository: s.Repository.FullName, Revision: idx.Commit, Citation: permalink(s.Repository, idx.Commit, ch.Path) + fmt.Sprintf("#L%d-L%d", ch.StartLine, ch.EndLine), Methods: methods[i], Score: score, Exact: exact[i]})
	}
	sort.SliceStable(out, func(i, j int) bool { return sourceLess(out[i], out[j]) })
	return out, embedded, nil
}

var sourceCall = regexp.MustCompile(`\b([\pL_][\pL\pN_]*)\s*\(`)

// Navigation and local symbol references add original excerpts to the pool;
// neither labels nor inferred relationships become answer evidence themselves.
// Expansion is one hop and bounded across the complete notebook selection.
func expandSourceCandidates(idx sourceIndex, q SearchQuery, scores map[int]float64, methods map[int][]string) {
	capExtra := max(1, 32/len(q.Projects))
	added := 0
	add := func(i int, score float64, method string) {
		if _, exists := scores[i]; exists || added >= capExtra || !strings.HasPrefix(idx.Chunks[i].Path, q.PathPrefix) {
			return
		}
		scores[i], methods[i] = score, []string{method}
		added++
	}
	seeds := make([]int, 0, len(scores))
	for i := range scores {
		seeds = append(seeds, i)
	}
	sort.Slice(seeds, func(i, j int) bool {
		if scores[seeds[i]] != scores[seeds[j]] {
			return scores[seeds[i]] > scores[seeds[j]]
		}
		return seeds[i] < seeds[j]
	})
	// Resolve calls only within the same file. A same-named symbol in another
	// module is not proof of an import relationship.
	definitions := map[string][]int{}
	for i, ch := range idx.Chunks {
		for _, symbol := range strings.Split(ch.Symbol, ", ") {
			if symbol != "" {
				key := ch.Path + "\x00" + symbol
				matches := definitions[key]
				if len(matches) == 0 || matches[len(matches)-1] != i {
					definitions[key] = append(matches, i)
				}
			}
		}
	}
	for _, seed := range seeds[:min(12, len(seeds))] {
		ch := idx.Chunks[seed]
		if ch.Language != "py" && ch.Language != "go" {
			continue
		}
		for _, call := range sourceCall.FindAllStringSubmatch(ch.Text, 32) {
			matches := definitions[ch.Path+"\x00"+call[1]]
			// A bare call name cannot disambiguate repeated nested functions or
			// methods. Leave ambiguous/multi-excerpt definitions to normal retrieval.
			if len(matches) == 1 && added < capExtra/2 {
				add(matches[0], scores[seed]*.95, "reference")
			}
		}
		if added >= capExtra/2 {
			break
		}
	}
	tokens := map[string]bool{}
	for _, token := range retrieval.SourceTokens(q.Query) {
		if !retrieval.Framing(token) {
			tokens[token] = true
		}
	}
	type mapHit struct {
		path  string
		score int
	}
	hits := []mapHit{}
	for _, entry := range idx.Map {
		if !strings.HasPrefix(entry.Path, q.PathPrefix) {
			continue
		}
		if score := mapQueryScore(entry, q.Query, tokens); score > 0 {
			hits = append(hits, mapHit{entry.Path, score})
		}
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return hits[i].path < hits[j].path
	})
	for rank, hit := range hits[:min(8, len(hits))] {
		count := 0
		for i, ch := range idx.Chunks {
			if ch.Path != hit.path {
				continue
			}
			add(i, retrieval.ReciprocalRank(100+rank), "map")
			count++
			if count == 2 {
				break
			}
		}
	}
}

// ValidateResult is the final delivery boundary after optional provider ranking
// or note retrieval has yielded control to other work.
func (m *Manager) ValidateResult(ctx context.Context, result SearchResult) error {
	for project, coverage := range result.Coverage {
		if coverage.Downloaded == "" {
			continue
		}
		if err := m.read(ctx, project, coverage.Source, func(_ *cache, s State) error {
			if s.ID != coverage.Source || s.Generation != coverage.Generation || s.Downloaded != coverage.Downloaded {
				return ErrConflict
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}
