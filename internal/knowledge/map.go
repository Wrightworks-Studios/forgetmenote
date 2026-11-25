package knowledge

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"forgetmenote/internal/workspace"
)

type MapHeading struct {
	Text   string `json:"text"`
	Anchor string `json:"anchor"`
}
type MapEntry struct {
	Path      string       `json:"path"`
	NoteID    string       `json:"noteId,omitempty"`
	Title     string       `json:"title"`
	Hash      string       `json:"hash"`
	Headings  []MapHeading `json:"headings,omitempty"`
	Tags      []string     `json:"tags,omitempty"`
	Links     []string     `json:"links,omitempty"`
	Backlinks []string     `json:"backlinks,omitempty"`
}
type MapQuery struct {
	ContainerSelection
	Scope      *workspace.ContentScope `json:"-"`
	Query      string                  `json:"query,omitempty"`
	PathPrefix string                  `json:"pathPrefix,omitempty"`
	Revision   string                  `json:"revision,omitempty" jsonschema:"Use the returned revision with every pagination cursor to keep the same snapshot."`
	After      string                  `json:"after,omitempty" jsonschema:"Returned next cursor; requires the same revision, query and pathPrefix as the previous page."`
	Limit      int                     `json:"limit,omitempty"`
}
type MapResult struct {
	Revision   string     `json:"revision"`
	Visibility string     `json:"visibility"`
	Entries    []MapEntry `json:"entries"`
	Total      int        `json:"total"`
	Next       string     `json:"next,omitempty"`
}
type noteMapCache struct {
	Version  int                     `json:"version"`
	Checksum string                  `json:"checksum"`
	Entries  map[string]noteMapEntry `json:"entries"`
	// Purge is the notebook's latest purge when the map was written; a map
	// from before a later purge is not loaded.
	Purge string `json:"purge,omitempty"`
}

// noteMapEntry caches what an entry's page links need beside it. Their targets
// depend on other notes' titles, so they are resolved for every map.
type noteMapEntry struct {
	MapEntry
	Page      bool     `json:"page,omitempty"`
	PageNames []string `json:"pageNames,omitempty"`
	PageLinks []string `json:"pageLinks,omitempty"`
}

const noteMapVersion = 2

type mapCursor struct {
	Key    string `json:"key"`
	Offset int    `json:"offset"`
}

func boundedMapText(s string) string {
	r := []rune(s)
	if len(r) > 160 {
		return string(r[:160])
	}
	return s
}

// Map derives navigation from the same committed, visible content as retrieval.
// Entries are reusable extraction results; backlinks and pagination are scoped
// afresh so changes to visibility never reveal a previously visible note.
func (e *Engine) Map(ctx context.Context, q MapQuery) (MapResult, error) {
	out := MapResult{Entries: []MapEntry{}}
	if q.Limit < 0 || q.Limit > 40 || len(q.Query) > 2000 || len(q.After) > 2048 || len(q.PathPrefix) > 512 {
		return out, errors.New("map requires a short query and at most 40 entries")
	}
	if q.Limit == 0 {
		q.Limit = 24
	}
	scope, err := e.ResolveContentScope(q.ContainerSelection, false)
	if err != nil {
		return out, err
	}
	if q.Scope != nil {
		scope = *q.Scope
		if err = scope.Validate(); err != nil {
			return out, err
		}
	}
	snapshot, err := e.Store.SnapshotScoped(ctx, "", scope)
	if err != nil {
		return out, err
	}
	if q.Revision != "" && q.Revision != snapshot.Revision {
		return out, workspace.ErrConflict
	}
	e.mu.Lock()
	if e.mapCache == nil {
		e.mapCache = &noteMapCache{Version: noteMapVersion, Entries: map[string]noteMapEntry{}}
		if st, err := os.Stat(filepath.Join(e.dir, "note-map.json")); err == nil && st.Size() <= 16<<20 {
			raw, err := os.ReadFile(filepath.Join(e.dir, "note-map.json"))
			var cache noteMapCache
			if err == nil && json.Unmarshal(raw, &cache) == nil && cache.Purge != e.state.Purge {
				// Written before a purge; it holds purged text.
				_ = os.Remove(filepath.Join(e.dir, "note-map.json"))
			} else if err == nil && cache.Version == noteMapVersion && cache.Entries != nil && cache.Checksum == mapChecksum(cache.Entries) {
				e.mapCache = &cache
			}
		}
	}
	next := &noteMapCache{Version: noteMapVersion, Entries: map[string]noteMapEntry{}}
	for _, file := range snapshot.Files {
		if workspace.IsTrash(file.Path) || file.Path == "AGENTS.md" {
			continue
		}
		entry, ok := e.mapCache.Entries[file.Path]
		if !ok || entry.Hash != file.Hash || entry.Path != file.Path {
			entry = noteMapEntry{MapEntry: MapEntry{Path: file.Path, NoteID: file.Metadata.ID, Title: boundedMapText(noteTitle(file)), Hash: file.Hash}}
			entry.PageNames, entry.Page = pageNames(file)
			for _, tag := range file.Metadata.Tags {
				if len(entry.Tags) >= 8 {
					break
				}
				entry.Tags = append(entry.Tags, boundedMapText(tag))
			}
			for _, section := range sections([]byte(file.Content)) {
				if section.heading != "" && len(section.anchor) <= 512 && len(entry.Headings) < 12 {
					entry.Headings = append(entry.Headings, MapHeading{boundedMapText(section.heading), section.anchor})
				}
			}
			for _, ref := range markdownLinks(file) {
				switch {
				case ref.page != "":
					if len(ref.page) <= 512 && len(entry.PageLinks) < 32 {
						entry.PageLinks = append(entry.PageLinks, ref.page)
					}
				case ref.Project == "" && !ref.External && ref.Path != "" && len(ref.Path) <= 512 && len(entry.Links) < 8:
					entry.Links = append(entry.Links, ref.Path)
				}
			}
		}
		next.Entries[file.Path] = entry
	}
	// Persistence is a cache optimization; failed cache publication cannot make
	// original notes unavailable. The next call can rebuild the map.
	next.Checksum, next.Purge = mapChecksum(next.Entries), e.state.Purge
	raw, _ := json.Marshal(next)
	if (next.Checksum != e.mapCache.Checksum || next.Purge != e.mapCache.Purge) && len(raw) <= 16<<20 {
		_ = persist(filepath.Join(e.dir, "note-map.json"), next)
	}
	e.mapCache = next
	entries := make([]MapEntry, 0, len(next.Entries))
	incoming := map[string][]string{}
	paths := make([]string, 0, len(next.Entries))
	for p := range next.Entries {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	pages := &wikiPages{}
	for _, p := range paths {
		if entry := next.Entries[p]; entry.Page {
			pages.add(p, entry.PageNames)
		}
	}
	links := map[string][]string{}
	for _, p := range paths {
		entry := next.Entries[p]
		for _, target := range entry.Links {
			if _, ok := next.Entries[target]; ok {
				links[p] = append(links[p], target)
			}
		}
		for _, name := range entry.PageLinks {
			if target := pages.target(p, name); target != "" && len(links[p]) < 8 {
				links[p] = append(links[p], target)
			}
		}
		for _, target := range links[p] {
			if len(incoming[target]) < 8 {
				incoming[target] = append(incoming[target], p)
			}
		}
	}
	for _, p := range paths {
		entry := next.Entries[p].MapEntry
		entry.Links = links[p]
		entry.Backlinks = incoming[p]
		if strings.HasPrefix(p, q.PathPrefix) {
			entries = append(entries, boundedMapEntry(entry))
		}
	}
	e.mu.Unlock()
	words := strings.Fields(strings.ToLower(q.Query))
	score := func(entry MapEntry) int {
		text := strings.ToLower(entry.Path + " " + entry.Title + " " + strings.Join(entry.Tags, " "))
		for _, h := range entry.Headings {
			text += " " + strings.ToLower(h.Text)
		}
		n := 0
		for _, w := range words {
			if strings.Contains(text, w) {
				n++
			}
			if strings.EqualFold(entry.Path, w) {
				n += 10
			}
		}
		return n
	}
	if len(words) > 0 {
		filtered := entries[:0]
		for _, entry := range entries {
			if score(entry) > 0 {
				filtered = append(filtered, entry)
			}
		}
		entries = filtered
		sort.SliceStable(entries, func(i, j int) bool { return score(entries[i]) > score(entries[j]) })
	}
	binding, _ := json.Marshal([]string{e.Store.Root, snapshot.Revision, snapshot.Visibility, scope.ID(), q.Query, q.PathPrefix, "note-map-v1"})
	key := workspace.ContentHash(binding)
	offset := 0
	if q.After != "" {
		raw, err := base64.RawURLEncoding.DecodeString(q.After)
		var cursor mapCursor
		if err != nil || json.Unmarshal(raw, &cursor) != nil || cursor.Key != key || cursor.Offset < 0 || cursor.Offset > len(entries) {
			return out, workspace.ErrConflict
		}
		offset = cursor.Offset
	}
	out.Revision, out.Visibility, out.Total = snapshot.Revision, snapshot.Visibility, len(entries)
	used := 0
	for offset < len(entries) && len(out.Entries) < q.Limit {
		raw, _ := json.Marshal(entries[offset])
		if used+len(raw) > 8000 {
			break
		}
		used += len(raw)
		out.Entries = append(out.Entries, entries[offset])
		offset++
	}
	if offset < len(entries) {
		raw, _ := json.Marshal(mapCursor{key, offset})
		out.Next = base64.RawURLEncoding.EncodeToString(raw)
	}
	status, err := e.Store.Status(ctx)
	if err != nil {
		return MapResult{}, err
	}
	visibility, err := e.Store.HistoryVisibilityID(ctx)
	if err != nil {
		return MapResult{}, err
	}
	if status.Revision != snapshot.Revision || visibility != snapshot.Visibility {
		return MapResult{}, workspace.ErrConflict
	}
	return out, nil
}

func mapChecksum(entries map[string]noteMapEntry) string {
	raw, _ := json.Marshal(entries)
	return workspace.ContentHash(raw)
}

func boundedMapEntry(entry MapEntry) MapEntry {
	for {
		raw, _ := json.Marshal(entry)
		if len(raw) <= 6000 {
			return entry
		}
		switch {
		case len(entry.Headings) > 0:
			entry.Headings = entry.Headings[:len(entry.Headings)-1]
		case len(entry.Backlinks) > 0:
			entry.Backlinks = entry.Backlinks[:len(entry.Backlinks)-1]
		case len(entry.Links) > 0:
			entry.Links = entry.Links[:len(entry.Links)-1]
		case len(entry.Tags) > 0:
			entry.Tags = entry.Tags[:len(entry.Tags)-1]
		default:
			return entry
		}
	}
}
