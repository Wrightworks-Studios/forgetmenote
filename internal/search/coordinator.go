package search

import (
	"context"
	"errors"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/knowledge"
	"forgetmenote/internal/notebooks"
	"forgetmenote/internal/reposource"
	"forgetmenote/internal/retrieval"
	"forgetmenote/internal/workspace"
	"slices"
	"sort"
)

var ErrOutsideCorpus = errors.New("requested content is outside this run's saved corpus")

// Frozen is trusted run state supplied by chat admission, never tool arguments.
type Frozen struct {
	Project string
	Corpus  *chat.Corpus
	Bind    func(knowledge.RetrievalQuery) (knowledge.RetrievalQuery, error)
}

// Coordinator authorizes the complete corpus before provider work and rechecks
// authorization and source revisions before disclosing merged evidence.
type Coordinator struct {
	catalog      *catalog.Catalog
	indexes      *Indexes
	access       func(context.Context, string, string) (*workspace.Store, error)
	contentScope func(context.Context, *workspace.Store, knowledge.ContainerSelection) (workspace.ContentScope, error)
	rerank       func(context.Context, reposource.SearchQuery, *reposource.SearchResult) error
}

func NewCoordinator(c *catalog.Catalog, indexes *Indexes, access func(context.Context, string, string) (*workspace.Store, error), scope func(context.Context, *workspace.Store, knowledge.ContainerSelection) (workspace.ContentScope, error), rerank func(context.Context, reposource.SearchQuery, *reposource.SearchResult) error) *Coordinator {
	return &Coordinator{c, indexes, access, scope, rerank}
}

type Request struct {
	Projects   []string `json:"projects"`
	Corpora    []string `json:"corpora,omitempty"`
	Query      string   `json:"query"`
	Mode       string   `json:"mode,omitempty"`
	PathPrefix string   `json:"pathPrefix,omitempty"`
	Limit      int      `json:"limit,omitempty"`
	History    bool     `json:"history,omitempty"`
	Archives   string   `json:"archives,omitempty"`
	Drafts     string   `json:"drafts,omitempty"`
}

func (r Request) ValidateCorpora() error {
	if len(r.Corpora) == 0 || len(r.Corpora) > 2 {
		return errors.New("select notes, repository, or both corpora")
	}
	for i, c := range r.Corpora {
		if c != "notes" && c != "repository" || i > 0 && c == r.Corpora[0] {
			return errors.New("corpora must contain notes and/or repository without duplicates")
		}
	}
	return nil
}

type Item struct {
	Ref     notebooks.DocumentRef    `json:"ref"`
	Project string                   `json:"project"`
	Kind    string                   `json:"kind"`
	Note    *knowledge.Passage       `json:"note,omitempty"`
	Source  *reposource.SourceResult `json:"source,omitempty"`
	score   float64
	exact   bool
}
type Result struct {
	Results   []Item                               `json:"results"`
	Sources   reposource.SearchResult              `json:"sources"`
	Notes     map[string]knowledge.RetrievalResult `json:"notes"`
	Truncated bool                                 `json:"truncated"`
}

func (s *Coordinator) Retrieve(ctx context.Context, in Request, repositories *reposource.Manager, frozen *Frozen) (Result, error) {
	ctx = knowledge.WithQueryCache(ctx)
	out := Result{Results: []Item{}, Notes: map[string]knowledge.RetrievalResult{}, Sources: reposource.SearchResult{Results: []reposource.SourceResult{}, Coverage: map[string]reposource.Coverage{}}}
	q := reposource.SearchQuery{Projects: in.Projects, Query: in.Query, Mode: in.Mode, PathPrefix: in.PathPrefix, Limit: in.Limit}
	if err := q.Validate(); err != nil {
		return out, err
	}
	if err := in.ValidateCorpora(); err != nil {
		return out, err
	}
	notes, source := false, false
	for _, c := range in.Corpora {
		switch c {
		case "notes":
			notes = true
		case "repository":
			source = true
		}
	}
	noteProjects, sourceProjects := []string{}, []string{}
	for _, project := range in.Projects {
		n, r := notes, source
		if scope := frozen; scope != nil {
			if scope.Corpus.Validate() != nil {
				return out, ErrOutsideCorpus
			}
			n = n && (project == scope.Project || slices.Contains(scope.Corpus.NoteProjects, project))
			r = r && slices.Contains(scope.Corpus.SourceProjects, project)
			if !n && !r {
				return out, ErrOutsideCorpus
			}
		}
		if n && (s.catalog.Kind(project) != "repository" || in.History) {
			noteProjects = append(noteProjects, project)
		}
		if r {
			sourceProjects = append(sourceProjects, project)
		}
	}
	check := func(ctx context.Context, project string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, err := s.access(ctx, project, "read")
		return err
	}
	// Authorize the complete selection before doing any candidate/provider work.
	for _, p := range in.Projects {
		if err := check(ctx, p); err != nil {
			return out, err
		}
	}
	q.Scopes = map[string]workspace.ContentScope{}
	for _, p := range sourceProjects {
		store, err := s.access(ctx, p, "read")
		if err != nil {
			return out, err
		}
		scope, err := s.contentScope(ctx, store, knowledge.ContainerSelection{Archives: in.Archives, Drafts: in.Drafts})
		if err != nil {
			return out, err
		}
		q.Scopes[p] = scope
	}
	q.Projects = sourceProjects
	q.Check = check
	q.RequireExternal = frozen != nil
	if scope := frozen; scope != nil {
		q.Bindings = scope.Corpus.SourceBindings
	}
	if len(sourceProjects) > 0 {
		if repositories == nil {
			out.Sources = reposource.SearchResult{Results: []reposource.SourceResult{}, Coverage: map[string]reposource.Coverage{}}
			for _, p := range sourceProjects {
				out.Sources.Coverage[p] = reposource.Coverage{Message: "Repository integration is not configured"}
			}
		} else {
			var err error
			q.Rerank = func(ctx context.Context, candidates *reposource.SearchResult) error {
				return s.rerank(ctx, q, candidates)
			}
			out.Sources, err = repositories.Search(ctx, q)
			if errors.Is(err, reposource.ErrConflict) {
				out.Sources, err = repositories.Search(ctx, q)
			}
			if err != nil {
				return out, err
			}
		}
		for i := range out.Sources.Results {
			r := &out.Sources.Results[i]
			out.Results = append(out.Results, Item{Ref: notebooks.DocumentRef{Project: r.Project, Origin: "repository", Source: r.Source, Path: r.Path, Revision: r.Revision}, Project: r.Project, Kind: "repository", Source: r, score: retrieval.ReciprocalRank(i), exact: r.Exact})
		}
	}
	if notes {
		for _, p := range noteProjects {
			store, err := s.access(ctx, p, "read")
			if err != nil {
				return out, err
			}
			scope := "current"
			if in.History {
				scope = "history"
			}
			query := knowledge.RetrievalQuery{Text: in.Query, Scope: scope, Limit: retrieval.DefaultLimit, TokenBudget: retrieval.DefaultTokenBudget, LocalOnly: in.Mode == "text", ContainerSelection: knowledge.ContainerSelection{Archives: in.Archives, Drafts: in.Drafts}}
			query.PathPrefix, query.SemanticOnly = in.PathPrefix, in.Mode == "meaning"
			if run := frozen; run != nil {
				query, err = run.Bind(query)
				if err != nil {
					return out, err
				}
			}
			engine, release, err := s.indexes.Acquire(ctx, store)
			if err != nil {
				return out, err
			}
			result, err := engine.Retrieve(ctx, query)
			release()
			if err != nil {
				return out, err
			}
			out.Notes[p] = result
			for i := range result.Passages {
				passage := result.Passages[i]
				out.Results = append(out.Results, Item{Ref: notebooks.DocumentRef{Project: p, Origin: "notes", NoteID: passage.NoteID, Path: passage.Path, Revision: passage.Revision, Heading: passage.Anchor}, Project: p, Kind: "notes", Note: &passage, score: retrieval.ReciprocalRank(i)})
			}
		}
	}
	sort.SliceStable(out.Results, func(i, j int) bool {
		a, b := out.Results[i], out.Results[j]
		if a.exact != b.exact {
			return a.exact
		}
		return a.score > b.score
	})
	limit := in.Limit
	if limit == 0 {
		limit = retrieval.DefaultLimit
	}
	packed := make([]Item, 0, limit)
	budget := retrieval.NewBudget(limit, 0, retrieval.DefaultPerFile)
	for _, item := range out.Results {
		path, text := "", ""
		if item.Source != nil {
			path = item.Source.Path
			text = item.Source.Text
		} else {
			path = item.Note.Path
			text = item.Note.Text
		}
		key := item.Project + "\x00" + item.Kind + "\x00" + path
		if !budget.Add(key, retrieval.EvidenceTokens(path, text)) {
			out.Truncated = true
			continue
		}
		packed = append(packed, item)
	}
	out.Results = packed
	if err := s.ValidateResult(ctx, in, out, repositories); err != nil {
		return Result{}, err
	}
	return out, nil
}

// ValidateResult is also the final disclosure fence after a presentation layer
// reads pinned originals. It never repeats ranking or provider work.
func (s *Coordinator) ValidateResult(ctx context.Context, in Request, out Result, repositories *reposource.Manager) error {
	for _, project := range in.Projects {
		if err := ctx.Err(); err != nil {
			return err
		}
		store, err := s.access(ctx, project, "read")
		if err != nil {
			return err
		}
		if result, ok := out.Notes[project]; ok {
			scope, err := s.contentScope(ctx, store, knowledge.ContainerSelection{Archives: in.Archives, Drafts: in.Drafts})
			if err != nil {
				return err
			}
			status, err := store.Status(ctx)
			if err != nil {
				return err
			}
			visibility, err := store.HistoryVisibilityID(ctx)
			if err != nil {
				return err
			}
			if scope.ID() != result.ScopeID || status.Revision != result.Revision || visibility != result.Visibility {
				return workspace.ErrConflict
			}
		}
	}
	if repositories != nil && len(out.Sources.Coverage) > 0 {
		return repositories.ValidateResult(ctx, out.Sources)
	}
	return nil
}
