package chatrun

import (
	"context"
	"fmt"
	"net/url"
	"slices"

	"forgetmenote/internal/auth"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/knowledge"
	"forgetmenote/internal/reposource"
	"forgetmenote/internal/retrieval"
	"forgetmenote/internal/search"
	"forgetmenote/internal/workspace"
)

type chatRetrieval struct {
	knowledge.RetrievalResult
	Sources []reposource.SourceResult `json:"sources,omitempty"`
	Notes   []notebookEvidence        `json:"notes,omitempty"`
}

type notebookEvidence struct {
	Project string `json:"project"`
	knowledge.Passage
}

// passageCitation cites a committed passage of the run's notebook.
func passageCitation(id, project string, p knowledge.Passage) chat.Citation {
	return chat.Citation{ID: id, Project: project, Path: p.Path, URL: p.Citation, Title: p.Heading, Text: p.Text, Revision: p.Revision, Hash: p.NoteHash, PassageHash: p.Hash, Kind: "committed"}
}

func notebookCitation(id string, p notebookEvidence) chat.Citation {
	link := url.URL{Path: "/", RawQuery: url.Values{"project": {p.Project}, "note": {p.Path}, "noteId": {p.NoteID}, "revision": {p.Revision}}.Encode(), Fragment: p.Anchor}
	return chat.Citation{ID: id, Project: p.Project, Path: p.Path, URL: link.String(), Title: p.Heading, Text: p.Text, Revision: p.Revision, Hash: p.NoteHash, PassageHash: p.Hash, Kind: "notebook", Scope: "notebook"}
}

func sourceCitation(id string, p reposource.SourceResult) chat.Citation {
	return chat.Citation{ID: id, Project: p.Project, Source: p.Source, StartLine: p.StartLine, EndLine: p.EndLine, StartByte: p.StartByte, EndByte: p.EndByte, Path: p.Path, URL: p.Citation, Title: p.Repository + " · " + p.Path, Text: p.Text, Revision: p.Revision, Hash: p.ContentHash, PassageHash: p.ContentHash, Kind: "repository", Scope: "repository"}
}
func (s *Service) appendChatSources(ctx context.Context, run chat.Run, query knowledge.RetrievalQuery, result *chatRetrieval) error {
	if run.Options.Corpus == nil || len(run.Options.Corpus.SourceProjects)+len(run.Options.Corpus.NoteProjects) == 0 {
		return nil
	}
	mode := "auto"
	if query.LocalOnly {
		mode = "text"
	}
	projects := append([]string(nil), run.Options.Corpus.SourceProjects...)
	for _, p := range run.Options.Corpus.NoteProjects {
		if !slices.Contains(projects, p) {
			projects = append(projects, p)
		}
	}
	found, err := s.hooks.RetrieveScoped(ctx, search.Request{Projects: projects, Corpora: []string{"notes", "repository"}, Query: query.Text, Mode: mode, Limit: min(retrieval.DefaultLimit, query.Limit)})
	if err != nil {
		return err
	}
	// Interleave original sources with editable notebook context under one budget.
	notes := result.Passages
	result.Passages = nil
	budget := retrieval.NewBudget(min(retrieval.DefaultLimit, query.Limit), min(retrieval.DefaultTokenBudget, query.TokenBudget), 0)
	for i := 0; i < len(notes) || i < len(found.Results); i++ {
		if i < len(found.Results) {
			item := found.Results[i]
			if item.Source != nil {
				r := *item.Source
				if budget.Add("", retrieval.EvidenceTokens(r.Path, r.Text)) {
					result.Sources = append(result.Sources, r)
				}
			} else if item.Note != nil {
				r := *item.Note
				if scope := chat.RunScopeFrom(ctx); scope == nil || item.Project != scope.Project {
					if budget.Add("", retrieval.EvidenceTokens(r.Path, r.Text)) {
						result.Notes = append(result.Notes, notebookEvidence{Project: item.Project, Passage: r})
					}
				}
			}
		}
		if i < len(notes) {
			p := notes[i]
			if budget.Add("", retrieval.EvidenceTokens(p.Path, p.Text)) {
				result.Passages = append(result.Passages, p)
			}
		}
	}
	result.EstimatedTokens = budget.Tokens()
	for p, c := range found.Sources.Coverage {
		result.Message += fmt.Sprintf(" Source %s: %s, %d/%d embedded. %s", p, c.Indexed, c.Embedded, c.Chunks, c.Message)
	}
	return nil
}

// ValidateSources checks that a run may still send its evidence to a
// model: the grants checkChatSourceAccess covers, each cited note of another
// notebook unchanged, and, before an upstream request, the processing of each
// cited repository file.
func (s *Service) ValidateSources(ctx context.Context, run chat.Run, upstream bool) error {
	sources, err := s.chatSourceAccess(ctx, run)
	if err != nil {
		return err
	}
	ctx = withChatScopeRun(ctx, run)
	if err = s.checkChatNoteCitations(ctx, run); err != nil {
		return err
	}
	if upstream {
		for _, source := range sources {
			if err = s.hooks.Repositories().AuthorizeProcessing(ctx, source.project, source.id, source.revision, source.paths); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkChatSourceAccess rechecks, without reading any content, the grants a
// run's evidence depends on: its personal consent, its data grants, and read
// access to every notebook and connected repository of its corpus at the
// bound repository snapshot.
func (s *Service) checkChatSourceAccess(ctx context.Context, run chat.Run) error {
	_, err := s.chatSourceAccess(ctx, run)
	return err
}

type chatSourceSnapshot struct {
	project, id, revision string
	paths                 []string
}

func (s *Service) chatSourceAccess(ctx context.Context, run chat.Run) ([]chatSourceSnapshot, error) {
	if run.PersonalConsent != "" && run.PersonalConsent != s.hooks.PersonalConsentStamp(auth.Subject(ctx)) {
		return nil, workspace.ErrForbidden
	}
	if err := s.hooks.CheckDataCitations(ctx, run.Citations); err != nil {
		return nil, err
	}
	ctx = withChatScopeRun(ctx, run)
	if run.Options.Corpus == nil {
		return nil, chat.ErrCorpus
	}
	for _, project := range run.Options.Corpus.NoteProjects {
		if _, err := s.hooks.ToolStore(ctx, project, "read"); err != nil {
			return nil, err
		}
	}
	if len(run.Options.Corpus.SourceProjects) > 0 && s.hooks.Repositories() == nil {
		return nil, reposource.ErrUnavailable
	}
	sources := []chatSourceSnapshot{}
	for _, project := range run.Options.Corpus.SourceProjects {
		if _, err := s.catalog.Access(auth.Subject(ctx), project, "read"); err != nil {
			return nil, err
		}
		if err := s.hooks.CheckSourceScope(ctx, project); err != nil {
			return nil, err
		}
		state, err := s.hooks.Repositories().Status(project)
		if err != nil {
			return nil, err
		}
		if !state.Connected || !state.IndexConfig.ExternalProcessing || state.ID != run.Options.Corpus.SourceBindings[project] {
			return nil, reposource.ErrDenied
		}
		source := chatSourceSnapshot{project: project, id: state.ID, revision: state.Downloaded}
		for _, citation := range run.Citations {
			if citation.Kind == "repository" && citation.Project == project && (citation.Source != state.ID || citation.Revision != state.Downloaded) {
				return nil, workspace.ErrConflict
			}
			if citation.Kind == "repository" && citation.Project == project {
				source.paths = append(source.paths, citation.Path)
			}
		}
		sources = append(sources, source)
	}
	return sources, nil
}

// withChatScopeRun binds ctx's chat scope to run. The run gains citations
// after its request context is created; validate against that current
// evidence set, including after an unrelated save.
func withChatScopeRun(ctx context.Context, run chat.Run) context.Context {
	scope := chat.RunScopeFrom(ctx)
	if scope == nil {
		return ctx
	}
	copy := *scope
	copy.Run = run
	return chat.WithScope(ctx, &copy)
}

// checkChatNoteCitations rereads each cited note of a notebook at its pinned
// revision and, unless the corpus includes history, requires its current
// content to be unchanged. One pass shares each notebook's scoped snapshot,
// and each pinned revision's, across all of that notebook's citations.
func (s *Service) checkChatNoteCitations(ctx context.Context, run chat.Run) error {
	type notebook struct {
		ctx       context.Context
		store     *workspace.Store
		snapshots map[string]workspace.Snapshot
	}
	notebooks := map[string]*notebook{}
	for _, citation := range run.Citations {
		if citation.Kind != "notebook" {
			continue
		}
		n := notebooks[citation.Project]
		if n == nil {
			bound, err := s.DocumentScope(ctx, citation.Project, "notes", "", "", citation.Path)
			if err != nil {
				return err
			}
			store, err := s.hooks.ToolStore(bound, citation.Project, "read")
			if err != nil {
				return err
			}
			n = &notebook{ctx: bound, store: store, snapshots: map[string]workspace.Snapshot{}}
			notebooks[citation.Project] = n
		}
		doc, current, err := readChatNoteFrom(n.ctx, n.store, citation.Path, citation.Revision, func(revision string) (workspace.Snapshot, error) {
			if snapshot, ok := n.snapshots[revision]; ok {
				return snapshot, nil
			}
			snapshot, err := ScopedSnapshot(n.ctx, n.store, revision)
			if err == nil {
				n.snapshots[revision] = snapshot
			}
			return snapshot, err
		})
		if err != nil {
			return err
		}
		if workspace.ContentHash(doc.Content) != citation.Hash {
			return workspace.ErrConflict
		}
		if run.Options.Corpus.History {
			continue
		}
		if chat.RunScopeFrom(n.ctx) == nil {
			latest, err := ReadNote(n.ctx, n.store, citation.Path, "")
			if err != nil {
				return err
			}
			current.Files = []workspace.SnapshotFile{{Path: citation.Path, Hash: workspace.ContentHash(latest.Content)}}
		}
		eligible := false
		for _, file := range current.Files {
			if file.Path == citation.Path {
				if file.Hash != citation.Hash {
					return workspace.ErrConflict
				}
				eligible = true
				break
			}
		}
		if !eligible {
			return chat.ErrOutsideCorpus
		}
	}
	return nil
}
