package notebooks

import (
	"context"
	"errors"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/knowledge"
	"forgetmenote/internal/reposource"
	"forgetmenote/internal/workspace"
	"net/url"
	"strings"
	"sync"
)

// Links owns only derived candidates. Each disclosure rechecks current access,
// including cache hits, pinned revisions, and cross-notebook backlinks.
type Links struct {
	mu       sync.Mutex
	cache    map[*workspace.Store]linkIndex
	origin   string
	access   func(context.Context, string, string, string) (*workspace.Store, error)
	projects func(context.Context, string) []catalog.Project
	engine   func(context.Context, *workspace.Store) (*knowledge.Engine, func(), error)
}

func NewLinks(origin string, access func(context.Context, string, string, string) (*workspace.Store, error), projects func(context.Context, string) []catalog.Project, engine func(context.Context, *workspace.Store) (*knowledge.Engine, func(), error)) *Links {
	return &Links{origin: origin, access: access, projects: projects, engine: engine}
}

var ErrHeading = errors.New("linked heading is unavailable; open the page to choose its new heading")

type linkIndex struct {
	revision     string
	refs         []knowledge.Reference
	notes, bytes int
	complete     bool
}

// Only link candidates are cached. Current catalog authority is always applied
// by the caller, including on a cache hit and just before returning metadata.
func (s *Links) candidates(ctx context.Context, store *workspace.Store) (linkIndex, error) {
	status, err := store.Status(ctx)
	if err != nil {
		return linkIndex{}, err
	}
	s.mu.Lock()
	cached, ok := s.cache[store]
	s.mu.Unlock()
	if ok && cached.revision == status.Revision {
		return cached, nil
	}
	snap, err := store.Snapshot(ctx, "")
	if err != nil {
		return linkIndex{}, err
	}
	next := linkIndex{revision: snap.Revision, complete: true}
	for _, file := range snap.Files {
		next.notes++
		next.bytes += len(file.Content)
		if next.notes > 5000 || next.bytes > 16<<20 {
			next.complete = false
			break
		}
		if workspace.IsTrash(file.Path) || file.Path == "AGENTS.md" {
			continue
		}
		for _, link := range knowledge.MarkdownReferences(file) {
			ref, ok := knowledge.NotebookReference(link.URL, s.origin)
			if !ok {
				continue
			}
			if len(next.refs) >= 10000 {
				next.complete = false
				break
			}
			ref.Note = file.Path
			ref.Label = link.Label
			ref.Title, _ = workspace.NoteSummary([]byte(file.Content))
			next.refs = append(next.refs, ref)
		}
		if !next.complete {
			break
		}
	}
	s.mu.Lock()
	if s.cache == nil {
		s.cache = map[*workspace.Store]linkIndex{}
	}
	if len(s.cache) >= 32 {
		for old := range s.cache {
			delete(s.cache, old)
			break
		}
	}
	s.cache[store] = next
	s.mu.Unlock()
	return next, nil
}

type Target struct {
	Headings []string `json:"headings,omitempty"`
	Project  string   `json:"project"`
	Path     string   `json:"path"`
	NoteID   string   `json:"noteId,omitempty"`
	Revision string   `json:"revision,omitempty"`
	Heading  string   `json:"heading,omitempty"`
	Title    string   `json:"title,omitempty"`
	Notebook string   `json:"notebook,omitempty"`
	URL      string   `json:"url,omitempty"`
}

func (s *Links) Resolve(ctx context.Context, actor string, in Target) (Target, error) {
	store, err := s.access(ctx, actor, in.Project, "read")
	if err != nil {
		return Target{}, workspace.ErrForbidden
	}
	if in.NoteID != "" && !workspace.ValidNoteID(in.NoteID) {
		return Target{}, workspace.ErrForbidden
	}
	if in.Revision == "" && in.NoteID != "" {
		list, err := store.List(ctx)
		if err != nil {
			return Target{}, err
		}
		found := ""
		for _, e := range list {
			if e.Metadata != nil && e.Metadata.ID == in.NoteID && !workspace.IsTrash(e.Path) {
				if found != "" {
					return Target{}, errors.New("note identity is duplicated; repair the notebook before resolving this link")
				}
				found = e.Path
			}
		}
		if found == "" {
			return Target{}, workspace.ErrForbidden
		}
		in.Path = found
	}
	if workspace.IsTrash(in.Path) || in.Path == "AGENTS.md" {
		return Target{}, workspace.ErrForbidden
	}
	doc, err := store.KnowledgeRead(ctx, in.Path, in.Revision)
	if err != nil {
		return Target{}, workspace.ErrForbidden
	}
	if _, err = s.access(ctx, actor, in.Project, "read"); err != nil {
		return Target{}, workspace.ErrForbidden
	}
	metadata, err := workspace.ParseMetadata(doc.Content)
	if err != nil && in.NoteID != "" {
		return Target{}, err
	}
	if in.NoteID != "" && metadata.ID != in.NoteID {
		return Target{}, workspace.ErrForbidden
	}
	in.NoteID = metadata.ID
	if in.Heading != "" && !knowledge.HasHeading(doc.Content, in.Heading) {
		return Target{}, ErrHeading
	}
	in.Title, _ = workspace.NoteSummary(doc.Content)
	in.Headings = knowledge.HeadingAnchors(doc.Content)
	for _, p := range s.projects(ctx, actor) {
		if p.ID == in.Project {
			in.Notebook = p.Name
			break
		}
	}
	q := url.Values{"project": {in.Project}, "note": {in.Path}}
	if in.NoteID != "" {
		q.Set("noteId", in.NoteID)
	}
	if in.Revision != "" {
		q.Set("revision", in.Revision)
	}
	in.URL = "/?" + q.Encode()
	if in.Heading != "" {
		in.URL += "#" + url.PathEscape(in.Heading)
	}
	return in, nil
}

func (s *Links) Targets(ctx context.Context, actor, target, query string) ([]Target, error) {
	store, err := s.access(ctx, actor, target, "read")
	if err != nil {
		return nil, err
	}
	entries, err := store.List(ctx)
	if err != nil {
		return nil, err
	}
	q := strings.ToLower(query)
	out := []Target{}
	for _, e := range entries {
		if !strings.HasSuffix(e.Path, ".md") || e.Path == "AGENTS.md" || workspace.IsTrash(e.Path) || !strings.Contains(strings.ToLower(e.Path+" "+e.Title), q) {
			continue
		}
		id := ""
		if e.Metadata != nil {
			id = e.Metadata.ID
		}
		out = append(out, Target{Project: target, Path: e.Path, NoteID: id, Title: e.Title})
		if len(out) >= 100 {
			break
		}
	}
	if _, err = s.access(ctx, actor, target, "read"); err != nil {
		return nil, err
	}
	return out, nil
}
func (s *Links) Evidence(ctx context.Context, actor, project, path string, repositories *reposource.Manager) (knowledge.Evidence, error) {
	store, err := s.access(ctx, actor, project, "read")
	if err != nil {
		return knowledge.Evidence{}, err
	}
	engine, release, err := s.engine(ctx, store)
	if err != nil {
		return knowledge.Evidence{}, err
	}
	defer release()
	result, err := engine.Evidence(ctx, path)
	if err != nil {
		return result, err
	}
	current, err := store.KnowledgeRead(ctx, path, result.Revision)
	if err != nil {
		return result, err
	}
	id, _ := workspace.NoteID(current.Content)
	for i, original := range result.Sources {
		if repositories != nil && original.External {
			if source, sha, p := repositories.Citation(project, original.URL); source != "" {
				result.Sources[i].Code = &knowledge.CodeReference{Source: source, Revision: sha, Path: p}
			}
		}
		ref, ok := knowledge.NotebookReference(original.URL, s.origin)
		if !ok {
			continue
		}
		ref.Note = original.Note
		ref.Label = original.Label
		ref.SourceProject = project
		ref.State = "Notebook unavailable or access denied"
		target, e := s.Resolve(ctx, actor, Target{Project: ref.Project, Path: ref.Path, NoteID: ref.NoteID, Revision: ref.Revision, Heading: ref.Heading})
		if e == nil {
			ref.Path = target.Path
			ref.NoteID = target.NoteID
			ref.Title = target.Title
			ref.Notebook = target.Notebook
			ref.State = "Live notebook reference"
			if target.Revision != "" {
				ref.State = "Pinned notebook reference"
			}
		}
		result.Sources[i] = ref
	}
	scanned, bytes := 0, 0
	complete := true
	for i, p := range s.projects(ctx, actor) {
		if i >= 20 {
			complete = false
			break
		}
		other, e := s.access(ctx, actor, p.ID, "read")
		if e != nil {
			continue
		}
		index, e := s.candidates(ctx, other)
		if e != nil {
			complete = false
			continue
		}
		refs := []knowledge.Reference{}
		scanned += index.notes
		bytes += index.bytes
		if scanned > 5000 || bytes > 16<<20 {
			complete = false
			break
		}
		complete = complete && index.complete
		for _, ref := range index.refs {
			if ref.Project != project {
				continue
			}
			matches := ref.NoteID != "" && ref.NoteID == id || ref.NoteID == "" && ref.Path == path
			if !matches {
				continue
			}
			ref.SourceProject = p.ID
			ref.Notebook = p.Name
			ref.State = "Notebook reference"
			refs = append(refs, ref)
		}
		if _, e = s.access(ctx, actor, p.ID, "read"); e == nil {
			result.Backlinks = append(result.Backlinks, refs...)
		}
		if !complete && (scanned > 5000 || bytes > 16<<20) {
			break
		}
	}
	if !complete {
		result.Limitations += " Cross-notebook backlinks are incomplete: indexing is limited to 20 notebooks, 5,000 notes, 10,000 links per notebook and 16 MiB per request."
	}
	for i, ref := range result.Sources {
		if ref.Project == "" {
			continue
		}
		if _, e := s.access(ctx, actor, ref.Project, "read"); e != nil {
			ref.Title = ""
			ref.Notebook = ""
			ref.State = "Notebook unavailable or access denied"
			result.Sources[i] = ref
		}
	}
	visible := result.Backlinks[:0]
	for _, ref := range result.Backlinks {
		if ref.SourceProject != "" {
			if _, e := s.access(ctx, actor, ref.SourceProject, "read"); e != nil {
				continue
			}
		}
		visible = append(visible, ref)
	}
	result.Backlinks = visible
	if _, err = s.access(ctx, actor, project, "read"); err != nil {
		return knowledge.Evidence{}, err
	}
	return result, nil
}
