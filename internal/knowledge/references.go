package knowledge

import (
	"context"
	"errors"
	"forgetmenote/internal/workspace"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
	"html"
	"net/url"
	"path"
	"strconv"
	"strings"
)

type Reference struct {
	Code          *CodeReference `json:"code,omitempty"`
	Project       string         `json:"project,omitempty"`
	SourceProject string         `json:"sourceProject,omitempty"`
	NoteID        string         `json:"noteId,omitempty"`
	Title         string         `json:"title,omitempty"`
	Notebook      string         `json:"notebook,omitempty"`
	Note          string         `json:"note"`
	Label         string         `json:"label"`
	URL           string         `json:"url"`
	Path          string         `json:"path,omitempty"`
	Heading       string         `json:"heading,omitempty"`
	Revision      string         `json:"revision,omitempty"`
	State         string         `json:"state"`
	Scope         string         `json:"scope,omitempty"`
	External      bool           `json:"external"`
	page          string         // a [[…]] link's target before it is resolved
}
type CodeReference struct {
	Source   string `json:"source"`
	Revision string `json:"revision"`
	Path     string `json:"path"`
}
type Evidence struct {
	Revision    string      `json:"revision"`
	Sources     []Reference `json:"sources"`
	Backlinks   []Reference `json:"backlinks"`
	Limitations string      `json:"limitations"`
}

func (e *Engine) Evidence(ctx context.Context, note string) (Evidence, error) {
	return e.evidence(ctx, note, nil, false)
}
func (e *Engine) EvidenceScoped(ctx context.Context, note string, scope workspace.ContentScope, history bool) (Evidence, error) {
	if err := scope.Validate(); err != nil {
		return Evidence{}, err
	}
	return e.evidence(ctx, note, &scope, history)
}
func (e *Engine) evidence(ctx context.Context, note string, scope *workspace.ContentScope, history bool) (Evidence, error) {
	var snapshot workspace.Snapshot
	var err error
	if scope == nil {
		snapshot, err = e.Store.Snapshot(ctx, "")
	} else {
		snapshot, err = e.Store.SnapshotScoped(ctx, "", *scope)
	}
	if err != nil {
		return Evidence{}, err
	}
	files := map[string]workspace.SnapshotFile{}
	for _, file := range snapshot.Files {
		files[file.Path] = file
	}
	if _, ok := files[note]; !ok {
		return Evidence{}, workspace.ErrForbidden
	}
	links := map[string][]Reference{}
	pages := snapshotPages(snapshot.Files)
	if scope != nil {
		for _, file := range snapshot.Files {
			if !workspace.IsTrash(file.Path) && file.Path != "AGENTS.md" {
				links[file.Path] = noteLinks(file, pages)
			} else {
				delete(files, file.Path)
			}
		}
	} else {
		e.mu.Lock()
		if e.referenceRevision != snapshot.Revision || e.referenceVisibility != snapshot.Visibility {
			e.referenceLinks = map[string][]Reference{}
			for _, file := range snapshot.Files {
				e.referenceLinks[file.Path] = noteLinks(file, pages)
			}
			e.referenceRevision = snapshot.Revision
			e.referenceVisibility = snapshot.Visibility
		}
		links = e.referenceLinks
		e.mu.Unlock()
	}
	readReference := func(path, revision string) (workspace.Document, error) {
		if scope != nil && (!history || workspace.IsTrash(path) || path == "AGENTS.md") {
			return workspace.Document{}, workspace.ErrForbidden
		}
		doc, err := e.readPinned(ctx, path, revision)
		if err == nil && scope != nil {
			metadata, _ := workspace.ParseMetadata(doc.Content)
			if scope.Tier(metadata) < 0 {
				return workspace.Document{}, workspace.ErrForbidden
			}
		}
		return doc, err
	}
	result := Evidence{Revision: snapshot.Revision, Sources: []Reference{}, Backlinks: []Reference{}, Limitations: "Resolves Markdown links, reference-style links and [[Page title]] links. Raw HTML attributes are not resolved. Source changed is a content comparison, not a factual contradiction."}
	for _, ref := range links[note] {
		if ref.Project != "" {
			ref.State = "Notebook reference"
			result.Sources = append(result.Sources, ref)
			continue
		}
		if ref.External {
			var archived *workspace.SnapshotFile
			for i := range snapshot.Files {
				file := &snapshot.Files[i]
				record := file.Metadata.Source
				if record == nil || (sourceURLKey(record.URL) != sourceURLKey(ref.URL) && sourceURLKey(record.FinalURL) != sourceURLKey(ref.URL)) {
					continue
				}
				if archived == nil || record.CapturedAt > archived.Metadata.Source.CapturedAt {
					archived = file
				}
			}
			if archived != nil {
				ref.Path = archived.Path
				ref.Revision = snapshot.Revision
				ref.State = "Archived copy available; external citation is not pinned"
				ref.Scope = archived.Metadata.Source.CapturedAt
			}
			result.Sources = append(result.Sources, ref)
			continue
		}
		current, exists := files[ref.Path]
		if !exists {
			ref.State = "Source unavailable or excluded"
			if ref.Revision != "" {
				if _, err := readReference(ref.Path, ref.Revision); err == nil {
					ref.State = "Source changed; moved or deleted since this revision"
					ref.Scope = "note"
				}
			}
			result.Sources = append(result.Sources, ref)
			continue
		}
		ref.State = "Live reference; no recorded revision"
		ref.Scope = "note"
		if ref.Revision != "" {
			old, err := readReference(ref.Path, ref.Revision)
			if errors.Is(err, ErrPurged) {
				ref.State = "Recorded revision purged"
			} else if err != nil {
				ref.State = "Recorded revision unavailable"
			} else {
				before, okOld := sectionHash(old.Content, ref.Heading)
				after, okNew := sectionHash([]byte(current.Content), ref.Heading)
				if ref.Heading != "" && okOld && okNew {
					ref.Scope = "heading"
				} else {
					before = workspace.ContentHash(old.Content)
					after = current.Hash
					if ref.Heading != "" {
						ref.Scope = "whole note (heading unavailable)"
					}
				}
				u, _ := url.Parse(ref.URL)
				passageHash := u.Query().Get("fmn-passage")
				if passageHash != "" {
					size, _ := strconv.Atoi(u.Query().Get("fmn-chars"))
					if size >= 128 && size <= 4096 {
						config := Config{PassageRunes: size}
						oldParts, _ := Split(workspace.Snapshot{Revision: ref.Revision, Files: []workspace.SnapshotFile{{Path: ref.Path, Content: string(old.Content)}}}, config)
						newParts, _ := Split(workspace.Snapshot{Revision: snapshot.Revision, Files: []workspace.SnapshotFile{current}}, config)
						oldCount, newCount := 0, 0
						for _, p := range oldParts {
							if p.Hash == passageHash {
								oldCount++
							}
						}
						for _, p := range newParts {
							if p.Hash == passageHash {
								newCount++
							}
						}
						if oldCount == 1 {
							ref.Scope = "passage"
							before = passageHash
							after = "changed"
							if newCount == 1 {
								after = passageHash
							}
						}
					}
				}
				ref.State = "Unchanged"
				if before != after {
					ref.State = "Source changed"
				}
			}
		}
		result.Sources = append(result.Sources, ref)
	}
	for _, file := range snapshot.Files {
		for _, ref := range links[file.Path] {
			if ref.Project == "" && ref.Path == note {
				ref.State = "Reference"
				result.Backlinks = append(result.Backlinks, ref)
			}
		}
	}
	return result, nil
}
func localReference(note, href string) (string, *url.URL, bool) {
	u, err := url.Parse(href)
	if err != nil || u.IsAbs() || u.Host != "" || u.Opaque != "" || strings.Contains(u.Path, "\\") {
		return "", u, false
	}
	target := note
	if u.Path != "" {
		if strings.HasPrefix(u.Path, "/") {
			target = strings.TrimPrefix(path.Clean(u.Path), "/")
		} else {
			target = path.Clean(path.Join(path.Dir(note), u.Path))
		}
	}
	if target == ".." || strings.HasPrefix(target, "../") || !strings.HasSuffix(target, ".md") {
		return "", u, false
	}
	return target, u, true
}

// linkParser parses notes as the preview renders them (GFM), so links inside
// table cells, code and autolinks are what the reader sees.
var linkParser = goldmark.New(goldmark.WithExtensions(extension.GFM)).Parser()

// noteLinks lists a note's links in document order: Markdown links and, given
// the snapshot's pages, the page links that name one of them.
func noteLinks(file workspace.SnapshotFile, pages *wikiPages) []Reference {
	return pages.resolve(markdownLinks(file))
}

// markdownLinks extracts a note's links with page links unresolved. As in the
// browser, page links are read from rendered text outside code, links, images
// and raw HTML.
func markdownLinks(file workspace.SnapshotFile) []Reference {
	body := workspace.MarkdownBody([]byte(file.Content))
	tree := linkParser.Parse(text.NewReader(body))
	refs := []Reference{}
	var prose strings.Builder
	flush := func() {
		if raw := prose.String(); strings.Contains(raw, "[") {
			refs = append(refs, pageLinks(file.Path, html.UnescapeString(string(util.UnescapePunctuations([]byte(raw)))))...)
		}
		prose.Reset()
	}
	ast.Walk(tree, func(n ast.Node, enter bool) (ast.WalkStatus, error) {
		if t, ok := n.(*ast.Text); ok {
			if enter {
				prose.Write(t.Segment.Value(body))
				if t.HardLineBreak() {
					flush()
				} else if t.SoftLineBreak() {
					prose.WriteByte('\n')
				}
			}
			return ast.WalkContinue, nil
		}
		flush()
		if !enter {
			return ast.WalkContinue, nil
		}
		switch n.(type) {
		case *ast.CodeSpan, *ast.Image, *ast.AutoLink, *ast.RawHTML:
			return ast.WalkSkipChildren, nil
		}
		link, ok := n.(*ast.Link)
		if !ok {
			return ast.WalkContinue, nil
		}
		href := string(link.Destination)
		u, err := url.Parse(href)
		if err != nil {
			return ast.WalkSkipChildren, nil
		}
		if ref, ok := NotebookReference(href, ""); ok {
			ref.Note = file.Path
			ref.Label = string(link.Text(body))
			refs = append(refs, ref)
			return ast.WalkSkipChildren, nil
		}
		if u.Scheme == "http" || u.Scheme == "https" {
			refs = append(refs, Reference{Note: file.Path, Label: string(link.Text(body)), URL: href, External: true, State: "External source; not checked"})
			return ast.WalkSkipChildren, nil
		}
		target, u, ok := localReference(file.Path, href)
		if ok {
			refs = append(refs, Reference{Note: file.Path, Label: string(link.Text(body)), URL: href, Path: target, Heading: u.Fragment, Revision: u.Query().Get("fmn-rev")})
		}
		return ast.WalkSkipChildren, nil
	})
	flush()
	return refs
}

// NotebookReference recognizes ordinary application URLs. Absolute URLs are
// recognized only by the gateway, which supplies its configured origin.
func NotebookReference(href, origin string) (Reference, bool) {
	u, err := url.Parse(href)
	if err != nil || u.Host != "" && (origin == "" || u.Scheme+"://"+u.Host != origin) || u.Opaque != "" || u.User != nil {
		return Reference{}, false
	}
	if u.Path != "/" && u.Path != "" {
		return Reference{}, false
	}
	q := u.Query()
	project, p, id := q.Get("project"), q.Get("note"), q.Get("noteId")
	if project == "" || p == "" && id == "" {
		return Reference{}, false
	}
	return Reference{URL: href, Project: project, Path: p, NoteID: id, Revision: q.Get("revision"), Heading: u.Fragment, State: "Notebook reference"}, true
}
func MarkdownReferences(file workspace.SnapshotFile) []Reference { return noteLinks(file, nil) }
func HasHeading(content []byte, heading string) bool {
	_, ok := sectionHash(content, heading)
	return ok
}
func HeadingAnchors(content []byte) []string {
	out := []string{}
	for _, section := range sections(content) {
		if section.anchor == "" {
			continue
		}
		if len(out) == 100 {
			break
		}
		out = append(out, section.anchor)
	}
	return out
}
func sectionHash(content []byte, anchor string) (string, bool) {
	if anchor == "" {
		return workspace.ContentHash(content), true
	}
	body := workspace.MarkdownBody(content)
	all := sections(content)
	for i, section := range all {
		if section.anchor == anchor {
			end := len(body)
			for _, next := range all[i+1:] {
				if next.level <= section.level {
					end = next.start
					break
				}
			}
			return workspace.ContentHash([]byte(strings.TrimSpace(string(body[section.start:end])))), true
		}
	}
	return "", false
}

func sourceURLKey(value string) string {
	u, err := url.Parse(value)
	if err != nil {
		return value
	}
	u.Fragment = ""
	return u.String()
}
