package knowledge

import (
	"cmp"
	"path"
	"regexp"
	"slices"
	"strings"

	"forgetmenote/internal/workspace"
)

// Page links name a note as [[Page title]] or [[Target|label]] in prose. They
// resolve as the wiki resolves them in the browser (wikiLinkTarget in
// internal/web/ui/wiki.mjs; change both together): by title in any case, the
// page nearest the linking note winning, then by path from the notebook root,
// then from the linking note's folder, where a folder names its index.md.
// A title belongs to the linked page, so page links are resolved against the
// snapshot being read, never cached with the linking note alone.
var pageLinkPattern = regexp.MustCompile(`\[\[([^\[\]|\n]+)(?:\|([^\[\]\n]+))?\]\]`)

// pageLinks lists the page links in one run of rendered prose text, with their
// target in Reference.page until wikiPages.resolve gives them a Path.
func pageLinks(note, prose string) []Reference {
	if !strings.Contains(prose, "[[") {
		return nil
	}
	refs := []Reference{}
	for _, match := range pageLinkPattern.FindAllStringSubmatch(prose, -1) {
		target, label := strings.TrimSpace(match[1]), strings.TrimSpace(match[2])
		if match[2] == "" {
			label = target
		}
		if target != "" && label != "" {
			refs = append(refs, Reference{Note: note, Label: label, URL: match[0], page: target})
		}
	}
	return refs
}

// pageNames reports whether a note is a page a link can name, and the
// lower-cased titles it answers to: its title as the wiki shows it (front
// matter, then first heading or text, then file name). Pages are Markdown
// notes in the notes collection, outside Trash, other than agent instructions.
// An index.md without a title answers to its folder's name and "Overview".
func pageNames(file workspace.SnapshotFile) ([]string, bool) {
	p := file.Path
	if !strings.HasSuffix(p, ".md") || p == "AGENTS.md" || workspace.IsTrash(p) || !workspace.InCollection(&file.Metadata, "notes") {
		return nil, false
	}
	title := file.Metadata.Title
	if title == "" {
		title, _ = workspace.NoteSummary([]byte(file.Content))
	}
	if title == "" && path.Base(p) == "index.md" {
		names := []string{"overview"}
		if folder := path.Dir(p); folder != "." {
			names = append(names, strings.ToLower(path.Base(folder)))
		}
		return names, true
	}
	if title == "" {
		title = strings.TrimSuffix(path.Base(p), ".md")
	}
	return []string{strings.ToLower(title)}, true
}

// wikiPages are the pages of one snapshot. Built from snapshot files, titles
// are read only once a note actually contains a page link.
type wikiPages struct {
	files []workspace.SnapshotFile
	paths map[string]bool
	named map[string][]string
}

func snapshotPages(files []workspace.SnapshotFile) *wikiPages {
	return &wikiPages{files: files}
}

func (w *wikiPages) add(p string, names []string) {
	if w.paths == nil {
		w.paths, w.named = map[string]bool{}, map[string][]string{}
	}
	w.paths[p] = true
	for _, name := range names {
		if name != "" {
			w.named[name] = append(w.named[name], p)
		}
	}
}

func (w *wikiPages) load() {
	files := w.files
	w.files = nil
	for _, file := range files {
		if names, ok := pageNames(file); ok {
			w.add(file.Path, names)
		}
	}
}

// resolve gives page links their target page and drops those that name none;
// without pages, every page link is dropped.
func (w *wikiPages) resolve(refs []Reference) []Reference {
	out := refs[:0]
	for _, ref := range refs {
		if ref.page != "" {
			if w == nil {
				continue
			}
			if ref.Path = w.target(ref.Note, ref.page); ref.Path == "" {
				continue
			}
			ref.page = ""
		}
		out = append(out, ref)
	}
	return out
}

func (w *wikiPages) target(source, text string) string {
	wanted := strings.TrimSpace(text)
	if wanted == "" {
		return ""
	}
	w.load()
	folder := func(p string) string {
		if i := strings.LastIndexByte(p, '/'); i >= 0 {
			return p[:i]
		}
		return ""
	}
	if titled := w.named[strings.ToLower(wanted)]; len(titled) > 0 {
		from := strings.Split(folder(source), "/")
		shared := func(p string) int {
			parts, n := strings.Split(folder(p), "/"), 0
			for n < len(parts) && n < len(from) && parts[n] != "" && parts[n] == from[n] {
				n++
			}
			return n
		}
		return slices.MinFunc(titled, func(a, b string) int {
			return cmp.Or(cmp.Compare(shared(b), shared(a)), cmp.Compare(strings.Count(a, "/"), strings.Count(b, "/")), strings.Compare(a, b))
		})
	}
	relative := strings.TrimLeft(wanted, "/")
	if strings.HasSuffix(strings.ToLower(relative), ".md") {
		relative = relative[:len(relative)-3]
	}
	if relative == "" || slices.ContainsFunc(strings.Split(relative, "/"), func(part string) bool { return part == "" || part == "." || part == ".." }) {
		return ""
	}
	bases := []string{"", folder(source)}
	if strings.HasPrefix(wanted, "/") {
		bases = bases[:1]
	}
	for _, base := range bases {
		p := relative
		if base != "" {
			p = base + "/" + relative
		}
		for _, candidate := range []string{p + ".md", p + "/index.md"} {
			if w.paths[candidate] {
				return candidate
			}
		}
	}
	return ""
}
