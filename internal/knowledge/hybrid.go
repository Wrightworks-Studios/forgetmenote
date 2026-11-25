package knowledge

import (
	"context"
	"errors"
	"forgetmenote/internal/workspace"
	"path"
	"sort"
	"strings"
	"time"
)

// HybridQuery keeps the existing semantic query contract, with literal text
// ranking added before note-level reciprocal rank fusion.
type HybridQuery struct {
	SearchQuery
	Mode string `json:"mode,omitempty"`
	// Rerank asks for one explicit Jev judgment of the shortlist. The gateway
	// binds Jev only after checking instance policy, consent and access.
	Rerank bool        `json:"rerank,omitempty"`
	Jev    *JevRanking `json:"-"`
}
type SearchHit struct {
	Container            string              `json:"container,omitempty"`
	Priority             int                 `json:"priority,omitempty"`
	TrashID              string              `json:"trashId,omitempty"`
	Path                 string              `json:"path"`
	Title                string              `json:"title"`
	Text                 string              `json:"text"`
	Excerpt              string              `json:"excerpt"`
	Revision             string              `json:"revision"`
	Score                float64             `json:"score"`
	ID                   string              `json:"id,omitempty"`
	Hash                 string              `json:"hash,omitempty"`
	NoteHash             string              `json:"noteHash,omitempty"`
	Heading              string              `json:"heading,omitempty"`
	Citation             string              `json:"citation,omitempty"`
	CommittedAt          string              `json:"committedAt,omitempty"`
	Current              bool                `json:"current"`
	Occurrences          []HistoryOccurrence `json:"occurrences,omitempty"`
	OccurrenceCount      int                 `json:"occurrenceCount,omitempty"`
	OccurrencesTruncated bool                `json:"occurrencesTruncated,omitempty"`
	sourceVersions       []string
	displayPassage       *Passage
}
type HybridResult struct {
	ContentScope workspace.ContentScope `json:"contentScope"`
	ScopeID      string                 `json:"scopeId"`
	Visibility   string                 `json:"visibility"`
	Matches      []SearchHit            `json:"matches"`
	Mode         string                 `json:"mode"`
	Message      string                 `json:"message,omitempty"`
	Scope        string                 `json:"scope"`
	Reranked     bool                   `json:"reranked,omitempty"`
	// RerankAvailable is set by the gateway: an explicit request could use Jev.
	RerankAvailable bool            `json:"rerankAvailable,omitempty"`
	History         *HistoryStatus  `json:"history,omitempty"`
	Filters         *HistoryFilters `json:"filters,omitempty"`
}
type HistoryFilters struct {
	Paths    []string `json:"paths"`
	Tags     []string `json:"tags"`
	Statuses []string `json:"statuses"`
}

func noteTitle(f workspace.SnapshotFile) string {
	if f.Metadata.Title != "" {
		return f.Metadata.Title
	}
	title, _ := workspace.NoteSummary([]byte(f.Content))
	if title != "" {
		return title
	}
	return strings.TrimSuffix(path.Base(f.Path), ".md")
}
func excerpt(content, query string) string {
	for _, line := range strings.Split(content, "\n") {
		if strings.Contains(strings.ToLower(line), query) {
			content = line
			break
		}
	}
	runes := []rune(strings.Join(strings.Fields(content), " "))
	if len(runes) > 240 {
		return string(runes[:240]) + "…"
	}
	return string(runes)
}

func (e *Engine) HybridSearch(ctx context.Context, query HybridQuery) (HybridResult, error) {
	var resolveErr error
	query.SearchQuery, resolveErr = e.resolveSearch(query.SearchQuery)
	if resolveErr != nil {
		return HybridResult{}, resolveErr
	}
	if query.Scope == "history" {
		return e.historySearch(ctx, query)
	}
	if query.Scope != "" && query.Scope != "current" {
		return HybridResult{}, errors.New("search scope must be current or history")
	}
	result := HybridResult{Matches: []SearchHit{}, Mode: query.Mode, ContentScope: query.contentScope, ScopeID: query.contentScope.ID()}
	result.Scope = "current"
	if result.Mode == "" {
		result.Mode = "all"
	}
	if result.Mode != "all" && result.Mode != "text" && result.Mode != "meaning" {
		return result, errors.New("search mode must be all, text, or meaning")
	}
	query.Text = strings.TrimSpace(query.Text)
	if query.Text == "" || len(query.Text) > 200 || len(query.Literal) > 200 || len(query.Path) > 512 || len(query.Tag) > 200 || len(query.Status) > 200 {
		return result, errors.New("enter search text of 1–200 bytes; filters exceed limits if longer than 200 bytes (path: 512)")
	}
	snapshot, err := e.Store.SnapshotScoped(ctx, "", query.contentScope)
	if err != nil {
		return result, err
	}
	result.Visibility = snapshot.Visibility
	result.Filters = snapshotFilters(snapshot, query.IncludeTrash)
	requestedMode := result.Mode
	var semantic []Match
	searchedRevision := snapshot.Revision
	if result.Mode != "text" {
		e.mu.Lock()
		config := e.state.Config
		status := IndexStatus{Config: config, ConfigID: config.ID(), Revision: snapshot.Revision, Indexed: len(e.cache.Vectors), Total: len(e.passages), BrowserSearch: config.Enabled && config.Endpoint && e.provider.Load() != nil}
		e.mu.Unlock()
		// Invalid agent vectors are errors, not an endpoint outage to hide with fallback.
		if len(query.Vector) > 0 {
			if query.ConfigID != status.ConfigID {
				return result, errors.New("query embedding configuration does not match the project")
			}
			if _, err = normalize(query.Vector, status.Config.Dimensions); err != nil {
				return result, err
			}
		}
		switch {
		case !status.Config.Enabled:
			result.Message = "Semantic search is off."
		case len(query.Vector) == 0 && !status.BrowserSearch:
			result.Message = "Semantic search needs a server embedding endpoint; agents can supply query vectors."
		case status.Indexed == 0:
			result.Message = "No passages are indexed yet."
		default:
			semanticQuery := query.SearchQuery
			semanticQuery.Limit = 50
			searchCtx := ctx
			cancel := func() {}
			// An unavailable or cold model must not hold ordinary search for 30 seconds.
			if result.Mode == "all" {
				searchCtx, cancel = context.WithTimeout(ctx, 2*time.Second)
			}
			semantic, err = e.search(searchCtx, semanticQuery, true)
			cancel()
			if ctx.Err() != nil {
				return result, ctx.Err()
			}
			if err != nil {
				result.Message = "Semantic search is temporarily unavailable."
			} else if status.Indexed < status.Total {
				result.Message = "Semantic indexing is incomplete."
			}
		}
		if semantic == nil {
			if result.Mode == "meaning" {
				return result, e.validateSearchSnapshot(ctx, snapshot, query.contentScope)
			}
			result.Mode = "text"
			result.Message += " Showing text matches."
		}
	}
	if requestedMode != "text" {
		snapshot, err = e.Store.SnapshotScoped(ctx, "", query.contentScope)
		if err != nil {
			return result, err
		}
		result.Visibility = snapshot.Visibility
		result.Filters = snapshotFilters(snapshot, query.IncludeTrash)
	}
	// Do not combine rankings from different commits if a save raced the model call.
	if result.Mode != "text" && searchedRevision != snapshot.Revision {
		semantic = nil
		result.Message = "Notes changed during search. Search again for meaning matches."
		if result.Mode == "meaning" {
			return result, e.validateSearchSnapshot(ctx, snapshot, query.contentScope)
		}
		result.Mode = "text"
		result.Message += " Showing text matches."
	}

	text := strings.ToLower(query.Text)
	files := map[string]workspace.SnapshotFile{}
	exact := map[string]bool{}
	for _, f := range snapshot.Files {
		if !matches(f, query.SearchQuery) {
			continue
		}
		files[f.Path] = f
		exact[f.Path] = strings.EqualFold(noteTitle(f), query.Text) || strings.EqualFold(strings.TrimSuffix(path.Base(f.Path), ".md"), query.Text)
	}
	previewPassages := map[string]Passage{}
	lexical := []SearchHit{}
	keywords := false
	if requestedMode == "all" {
		lexical, keywords, err = e.keywordHits(ctx, query, snapshot, files, previewPassages)
		if err != nil {
			return result, err
		}
	}
	if !keywords && result.Mode != "meaning" {
		lexical = literalHits(snapshot, files, text, exact)
	} else if keywords {
		// BM25 matches whole words. Search runs as the user types, so partial
		// words keep their substring matches after the keyword ranking.
		seen := map[string]bool{}
		for _, hit := range lexical {
			seen[hit.Path] = true
		}
		for _, hit := range literalHits(snapshot, files, text, exact) {
			if len(lexical) == 50 {
				break
			}
			if !seen[hit.Path] {
				lexical = append(lexical, hit)
			}
		}
	}
	ranked := map[string]SearchHit{}
	if result.Mode != "meaning" {
		for i, hit := range lexical {
			hit.Score = 1 / float64(60+i+1)
			ranked[hit.Path] = hit
		}
	}
	for i, match := range semantic {
		f, ok := files[match.Path]
		if !ok {
			continue
		}
		hit, ok := ranked[match.Path]
		if !ok {
			hit = SearchHit{Container: f.Metadata.Container, Priority: f.Priority, Path: match.Path, Title: noteTitle(f), Revision: snapshot.Revision}
		}
		// A keyword passage contains the typed words, so it is the better preview.
		if _, ok := previewPassages[match.Path]; !ok {
			hit.Text = excerpt(match.Text, text)
			previewPassages[match.Path] = match.Passage
		}
		hit.Score += 1 / float64(60+i+1)
		ranked[match.Path] = hit
	}
	for _, hit := range ranked {
		hit.TrashID = trashID(hit.Path)
		result.Matches = append(result.Matches, hit)
	}
	sort.Slice(result.Matches, func(i, j int) bool {
		a, b := result.Matches[i], result.Matches[j]
		if a.Priority != b.Priority {
			return a.Priority < b.Priority
		}
		if result.Mode != "meaning" && exact[a.Path] != exact[b.Path] {
			return exact[a.Path]
		}
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		return a.Path < b.Path
	})
	if query.Rerank && query.Jev != nil && requestedMode != "text" {
		if err = e.rerankHits(ctx, query, snapshot, &result, exact, previewPassages); err != nil {
			return HybridResult{}, err
		}
	}
	limit := query.Limit
	if limit < 1 || limit > 50 {
		limit = 20
	}
	if len(result.Matches) > limit {
		result.Matches = result.Matches[:limit]
	}
	for i := range result.Matches {
		hit := &result.Matches[i]
		content := files[hit.Path].Content
		if passage, ok := previewPassages[hit.Path]; ok {
			excerpts, err := DisplayExcerpts([]byte(content), []Passage{passage}, query.Text)
			if err != nil {
				return HybridResult{}, err
			}
			hit.Excerpt = excerpts[0]
		} else {
			hit.Excerpt = workspace.SearchExcerpt([]byte(content), query.Text)
		}
	}
	if err = e.validateSearchSnapshot(ctx, snapshot, query.contentScope); err != nil {
		return HybridResult{}, err
	}
	return result, nil
}

// literalHits keeps the Text mode contract: the whole query as one phrase,
// ordered by exact title, title/path match, then body match.
func literalHits(snapshot workspace.Snapshot, files map[string]workspace.SnapshotFile, text string, exact map[string]bool) []SearchHit {
	lexical := []SearchHit{}
	for _, f := range snapshot.Files {
		if _, ok := files[f.Path]; !ok {
			continue
		}
		title := noteTitle(f)
		content := workspace.SearchableText(f.Content)
		if strings.Contains(strings.ToLower(f.Path+"\n"+title+"\n"+content), text) {
			priority := 1.0
			if strings.Contains(strings.ToLower(title), text) || strings.Contains(strings.ToLower(f.Path), text) {
				priority = 2
			}
			if exact[f.Path] {
				priority = 3
			}
			lexical = append(lexical, SearchHit{Container: f.Metadata.Container, Priority: f.Priority, TrashID: trashID(f.Path), Path: f.Path, Title: title, Text: excerpt(content, text), Revision: snapshot.Revision, Score: priority})
		}
	}
	sort.Slice(lexical, func(i, j int) bool {
		if lexical[i].Priority != lexical[j].Priority {
			return lexical[i].Priority < lexical[j].Priority
		}
		if lexical[i].Score != lexical[j].Score {
			return lexical[i].Score > lexical[j].Score
		}
		return lexical[i].Path < lexical[j].Path
	})
	if len(lexical) > 50 {
		lexical = lexical[:50]
	}
	return lexical
}

// keywordHits ranks Automatic search's text half with the committed-passage
// BM25 index that chat retrieval uses, keeping each note's best passage. It
// reports false when that index describes a different commit than snapshot, so
// the caller can use the literal ranking instead of mixing two revisions.
func (e *Engine) keywordHits(ctx context.Context, query HybridQuery, snapshot workspace.Snapshot, files map[string]workspace.SnapshotFile, preview map[string]Passage) ([]SearchHit, bool, error) {
	input, err := e.retrievalInput(ctx, RetrievalQuery{IncludeTrash: query.IncludeTrash, contentScope: query.contentScope}, 0)
	if err != nil {
		return nil, false, err
	}
	if input.revision != snapshot.Revision || input.visibility != snapshot.Visibility {
		return nil, false, nil
	}
	ranked, err := input.index.rank(ctx, query.Text)
	if err != nil {
		return nil, false, err
	}
	text := strings.ToLower(query.Text)
	hits := []SearchHit{}
	seen := map[string]bool{}
	// Ranks are tiered by container priority before score, so a zero score
	// can precede scored passages from a lower tier; skip rather than stop.
	for _, rank := range ranked {
		p := input.index.documents[rank.index].passage
		f, ok := files[p.Path]
		if (rank.score <= 0 && !rank.exact) || !ok || seen[p.Path] {
			continue
		}
		seen[p.Path] = true
		preview[p.Path] = p
		hits = append(hits, SearchHit{Container: f.Metadata.Container, Priority: f.Priority, Path: p.Path, Title: noteTitle(f), Text: excerpt(p.Text, text), Revision: snapshot.Revision, Score: rank.score})
		if len(hits) == 50 {
			break
		}
	}
	return hits, true, nil
}

// rerankHits applies one explicit Jev judgment to the leading note hits, each
// represented by the passage that ranked it. Provider failure keeps the local
// order; revoked access or changed notes remain errors so nothing stale is shown.
func (e *Engine) rerankHits(ctx context.Context, query HybridQuery, snapshot workspace.Snapshot, result *HybridResult, exact map[string]bool, passages map[string]Passage) error {
	binding := *query.Jev
	if binding.Check == nil {
		return workspace.ErrForbidden
	}
	positions := []int{}
	candidates := []Passage{}
	for i, hit := range result.Matches[:min(len(result.Matches), RelevanceCandidateLimit)] {
		if p, ok := passages[hit.Path]; ok {
			positions = append(positions, i)
			candidates = append(candidates, p)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	binding.Check = func(ctx context.Context) error {
		if err := query.Jev.Check(ctx); err != nil {
			return err
		}
		return e.validateSearchSnapshot(ctx, snapshot, query.contentScope)
	}
	binding.Scope.Feature = "rerank"
	binding.Scope.Operation += "\x00" + snapshot.Revision
	values, err := ScorePassages(ctx, query.Text, candidates, binding)
	if err != nil {
		return err
	}
	if values == nil {
		result.Message = strings.TrimSpace(result.Message + " Jev ranking is unavailable; showing the local order.")
		return nil
	}
	hits := make([]SearchHit, len(positions))
	scores := map[string]float64{}
	for i, position := range positions {
		hits[i] = result.Matches[position]
		scores[hits[i].Path] = values[i]
	}
	sort.SliceStable(hits, func(i, j int) bool {
		a, b := hits[i], hits[j]
		if a.Priority != b.Priority {
			return a.Priority < b.Priority
		}
		if result.Mode != "meaning" && exact[a.Path] != exact[b.Path] {
			return exact[a.Path]
		}
		return scores[a.Path] > scores[b.Path]
	})
	for i, position := range positions {
		result.Matches[position] = hits[i]
	}
	result.Reranked = true
	return nil
}

func snapshotFilters(snapshot workspace.Snapshot, includeTrash bool) *HistoryFilters {
	paths, tags, statuses := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, file := range snapshot.Files {
		if workspace.IsTrash(file.Path) && !includeTrash {
			continue
		}
		paths[file.Path] = true
		for _, tag := range file.Metadata.Tags {
			tags[tag] = true
		}
		if file.Metadata.Status != "" {
			statuses[file.Metadata.Status] = true
		}
	}
	keys := func(values map[string]bool) []string {
		out := make([]string, 0, len(values))
		for value := range values {
			out = append(out, value)
		}
		sort.Strings(out)
		return out
	}
	return &HistoryFilters{Paths: keys(paths), Tags: keys(tags), Statuses: keys(statuses)}
}
