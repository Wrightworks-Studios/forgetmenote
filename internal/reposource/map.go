package reposource

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"forgetmenote/internal/retrieval"
	"forgetmenote/internal/workspace"
)

const mapVersion = "repository-map-v1"
const maxMapBytes = 16 << 20

func (idx *sourceIndex) loadMap() error {
	if idx.MapVersion == "" {
		return nil
	}
	b, err := regularRead(filepath.Join(idx.dir, "map.json"), maxMapBytes)
	if err != nil || hash(b) != idx.MapHash || json.Unmarshal(b, &idx.Map) != nil || validateMap(*idx) != nil {
		return ErrUnavailable
	}
	return nil
}

// Map entries are navigation hints. Original chunks and Git blobs remain the
// evidence; neither inferred roles nor repository prose grant execution authority.
type MapEntry struct {
	Container string    `json:"container,omitempty"`
	NoteID    string    `json:"noteId,omitempty"`
	Title     string    `json:"title,omitempty"`
	Path      string    `json:"path"`
	BlobID    string    `json:"blobId"`
	Component string    `json:"component"`
	Role      string    `json:"role"`
	Symbols   []string  `json:"symbols,omitempty"`
	Links     []string  `json:"links,omitempty"`
	Label     *MapLabel `json:"label,omitempty"`
}
type MapLabel struct {
	InputHash   string  `json:"inputHash"`
	Model       string  `json:"model"`
	Topic       string  `json:"topic,omitempty"`
	Probability float64 `json:"probability"`
	Unavailable bool    `json:"unavailable,omitempty"`
}
type MapQuery struct {
	Scope      *workspace.ContentScope `json:"-"`
	Source     string                  `json:"source,omitempty"`
	Revision   string                  `json:"revision,omitempty"`
	Query      string                  `json:"query,omitempty" jsonschema:"Short navigation query, such as startup, authentication, or a component name; exact role names rank first"`
	PathPrefix string                  `json:"pathPrefix,omitempty"`
	After      string                  `json:"after,omitempty"`
	Limit      int                     `json:"limit,omitempty" jsonschema:"Maximum entries: 1–40; defaults to 24. Pages may contain fewer entries to fit the response size."`
}
type MapResult struct {
	Project    string     `json:"project"`
	Source     string     `json:"source"`
	Repository string     `json:"repository"`
	Revision   string     `json:"revision"`
	Entries    []MapEntry `json:"entries"`
	Total      int        `json:"total"`
	Next       string     `json:"next,omitempty"`
}

func mapRole(p string, ch SourceChunk) string {
	base := strings.ToLower(path.Base(p))
	switch {
	case base == "procfile" || base == "wsgi.py" || base == "asgi.py" || base == "main.go" || base == "__main__.py":
		return "startup"
	case base == "dockerfile" || strings.HasPrefix(base, "dockerfile.") || base == "makefile" || base == "cmakelists.txt":
		return "build"
	case ch.Role == "test":
		return "tests"
	case ch.Role == "documentation" || strings.HasPrefix(base, "readme"):
		return "documentation"
	case ch.Role == "configuration":
		return "configuration"
	default:
		return "source"
	}
}

// Only literal references to already admitted files are retained. This is not
// shell evaluation or a claim that every reference will execute at runtime.
func mapReferences(p, text string, admitted map[string]int) []string {
	seen := map[string]bool{}
	var out []string
	bytes := 0
	for _, word := range strings.FieldsFunc(text, func(r rune) bool {
		return strings.ContainsRune(" \t\r\n\"'`()[]{}<>,;:=", r)
	}) {
		word = strings.TrimPrefix(strings.SplitN(word, "#", 2)[0], "./")
		for _, candidate := range []string{word, path.Join(path.Dir(p), word)} {
			if candidate == p || !safePath(candidate) || seen[candidate] {
				continue
			}
			if _, ok := admitted[candidate]; !ok {
				continue
			}
			if bytes+len(candidate) > 1600 {
				continue
			}
			seen[candidate] = true
			bytes += len(candidate)
			out = append(out, candidate)
			if len(out) == 8 {
				sort.Strings(out)
				return out
			}
		}
	}
	sort.Strings(out)
	return out
}

func buildMap(idx *sourceIndex, prior sourceIndex) {
	old := map[string]MapEntry{}
	if prior.Source == idx.Source && prior.Repository == idx.Repository && prior.MapVersion == mapVersion {
		for _, entry := range prior.Map {
			old[entry.Path] = entry
		}
	}
	paths := map[string]bool{}
	for _, ch := range idx.Chunks {
		paths[ch.Path] = true
	}
	pathsChanged := len(paths) != len(old)
	for p := range paths {
		if _, ok := old[p]; !ok {
			pathsChanged = true
		}
	}
	reuse := map[string]bool{}
	positions := map[string]int{}
	texts := map[string]string{}
	for _, ch := range idx.Chunks {
		i, exists := positions[ch.Path]
		if !exists {
			i = len(idx.Map)
			positions[ch.Path] = i
			entry := MapEntry{Container: ch.Container, NoteID: ch.NoteID, Title: ch.Title, Path: ch.Path, BlobID: ch.BlobID, Component: path.Dir(ch.Path), Role: mapRole(ch.Path, ch)}
			if previous, ok := old[ch.Path]; ok && previous.BlobID == ch.BlobID {
				entry.Label = previous.Label
				if !pathsChanged && prior.MapVersion == mapVersion && prior.Chunker == idx.Chunker {
					entry.Symbols, entry.Links = previous.Symbols, previous.Links
					reuse[ch.Path] = true
				}
			}
			idx.Map = append(idx.Map, entry)
		}
		entry := &idx.Map[i]
		if reuse[ch.Path] {
			continue
		}
		if len(entry.Symbols) < 12 && ch.Symbol != "" && (len(entry.Symbols) == 0 || entry.Symbols[len(entry.Symbols)-1] != ch.Symbol) {
			entry.Symbols = append(entry.Symbols, clipUTF8(ch.Symbol, 160))
		}
		if len(texts[ch.Path]) < 8192 {
			texts[ch.Path] += clipUTF8(ch.Text, 8192-len(texts[ch.Path]))
		}
	}
	for i := range idx.Map {
		entry := &idx.Map[i]
		if !reuse[entry.Path] {
			entry.Links = mapReferences(entry.Path, texts[entry.Path], positions)
		}
		if strings.EqualFold(path.Base(entry.Path), "Procfile") {
			for _, target := range entry.Links {
				idx.Map[positions[target]].Role = "startup"
			}
		}
	}
	// A changed manifest can alter a file's role without altering its blob.
	for i := range idx.Map {
		entry := &idx.Map[i]
		previous := old[entry.Path]
		if previous.Role != entry.Role || strings.Join(previous.Links, "\n") != strings.Join(entry.Links, "\n") {
			entry.Label = nil
		}
	}
	idx.MapVersion = mapVersion
}

var mapTopics = map[string]string{
	"startup":        "startup runtime launch boot application entry point entrypoint",
	"build":          "build compilation packaging image dependencies",
	"configuration":  "configuration settings environment options",
	"tests":          "tests testing fixtures verification",
	"documentation":  "documentation docs guide manual",
	"persistence":    "persistence storage database migrations models",
	"background":     "background workers queues jobs scheduling tasks",
	"api":            "api HTTP routes endpoints requests handlers",
	"authentication": "authentication authorization identity permissions login",
	"observability":  "observability health readiness liveness metrics logging tracing",
	"interface":      "interface UI presentation components views",
}

func MapTopics() map[string]string {
	out := make(map[string]string, len(mapTopics)+1)
	for key, value := range mapTopics {
		out[key] = value
	}
	out["other"] = "Other, mixed, or insufficient evidence to identify one primary responsibility"
	return out
}

func mapTerms(entry MapEntry) string {
	terms := mapTopics[entry.Role]
	if entry.Label != nil {
		terms += " " + mapTopics[entry.Label.Topic]
	}
	return terms
}

type mapCursor struct {
	Map   string `json:"m"`
	Query string `json:"q"`
	Path  string `json:"p"`
}

func mapQueryScore(entry MapEntry, query string, tokens map[string]bool) int {
	if len(tokens) == 0 {
		if query != "" {
			return -1
		}
		return 0
	}
	matched := map[string]bool{}
	for _, token := range retrieval.SourceTokens(entry.Path + " " + strings.Join(entry.Symbols, " ") + " " + mapTerms(entry)) {
		if tokens[token] {
			matched[token] = true
		}
	}
	if len(matched) == 0 {
		return -1
	}
	score := len(matched)
	// An explicit responsibility outranks incidental words in large documentation
	// trees. These hints select files to read; they do not establish code behavior.
	if tokens[entry.Role] || entry.Label != nil && tokens[entry.Label.Topic] {
		score += len(tokens) + 1
	}
	if strings.EqualFold(strings.TrimSpace(query), entry.Path) {
		score += 2 * (len(tokens) + 1)
	}
	return score
}

func (m *Manager) Map(ctx context.Context, project string, q MapQuery) (MapResult, error) {
	out := MapResult{Entries: []MapEntry{}}
	if len(q.Query) > 2000 || q.Limit < 0 || q.Limit > 40 || q.PathPrefix != "" && !safePath(strings.TrimSuffix(q.PathPrefix, "/")) || q.After != "" && (len(q.After) > 8192 || q.Revision == "") {
		return out, ErrDenied
	}
	var cursor mapCursor
	if q.After != "" {
		raw, err := base64.RawURLEncoding.DecodeString(q.After)
		if err != nil || json.Unmarshal(raw, &cursor) != nil || !validHash(cursor.Map) || !validHash(cursor.Query) || !safePath(cursor.Path) {
			return out, ErrDenied
		}
	}
	scope := notebookScope(q.Scope)
	if err := scope.Validate(); err != nil {
		return out, err
	}
	queryID := hash([]byte(q.Query + "\x00" + q.PathPrefix + "\x00" + scope.ID()))
	tokens := map[string]bool{}
	for _, token := range retrieval.SourceTokens(q.Query) {
		if !retrieval.Framing(token) {
			tokens[token] = true
		}
	}
	if q.Limit == 0 {
		q.Limit = 24
	}
	err := m.read(ctx, project, q.Source, func(c *cache, s State) error {
		if q.Revision != "" && q.Revision != s.Downloaded {
			return ErrConflict
		}
		m.indexMu.Lock()
		defer m.indexMu.Unlock()
		idx, err := m.lexicalIndex(ctx, c, s)
		if err != nil {
			return err
		}
		if idx.Commit != s.Downloaded || idx.ConfigID != s.indexID() || idx.MapVersion != mapVersion {
			return ErrUnavailable
		}
		if q.After != "" && (cursor.Map != idx.MapHash || cursor.Query != queryID) {
			return ErrConflict
		}
		out.Project, out.Source, out.Repository, out.Revision = project, s.ID, s.Repository.FullName, idx.Commit
		type candidate struct {
			entry MapEntry
			score int
		}
		var matches []candidate
		for _, entry := range idx.Map {
			if s.NotebookLayout && scope.Tier(workspace.Metadata{Container: entry.Container}) < 0 {
				continue
			}
			if !strings.HasPrefix(entry.Path, q.PathPrefix) {
				continue
			}
			if score := mapQueryScore(entry, q.Query, tokens); score >= 0 {
				matches = append(matches, candidate{entry, score})
			}
		}
		sort.SliceStable(matches, func(i, j int) bool { return matches[i].score > matches[j].score })
		out.Total = len(matches)
		used, after := 0, q.After == ""
		for _, match := range matches {
			entry := match.entry
			if !after {
				after = entry.Path == cursor.Path
				continue
			}
			raw, _ := json.Marshal(entry)
			if len(raw) > 8000 {
				return ErrLimit
			}
			if len(out.Entries) == q.Limit || used+len(raw) > 8000 {
				if len(out.Entries) > 0 {
					next, _ := json.Marshal(mapCursor{idx.MapHash, queryID, out.Entries[len(out.Entries)-1].Path})
					out.Next = base64.RawURLEncoding.EncodeToString(next)
				}
				break
			}
			used += len(raw)
			out.Entries = append(out.Entries, entry)
		}
		if !after {
			return ErrConflict
		}
		return nil
	})
	return out, err
}

func validateMap(idx sourceIndex) error {
	if idx.MapVersion != mapVersion || len(idx.Map) > maxChunks() {
		return ErrUnavailable
	}
	previous := ""
	for _, entry := range idx.Map {
		if !safePath(entry.Path) || entry.Path <= previous || !revision.MatchString(entry.BlobID) || entry.Component != path.Dir(entry.Path) || len(entry.Symbols) > 12 || len(entry.Links) > 8 || entry.Role != "source" && mapTopics[entry.Role] == "" {
			return ErrUnavailable
		}
		for _, symbol := range entry.Symbols {
			if len(symbol) > 160 {
				return ErrUnavailable
			}
		}
		for _, link := range entry.Links {
			if !safePath(link) {
				return ErrUnavailable
			}
		}
		if label := entry.Label; label != nil {
			if !validHash(label.InputHash) || label.Model == "" || len(label.Model) > 200 || label.Topic != "" && mapTopics[label.Topic] == "" || label.Probability < 0 || label.Probability > 1 || label.Unavailable && (label.Topic != "" || label.Probability != 0) {
				return ErrUnavailable
			}
		}
		previous = entry.Path
	}
	return nil
}

func mapFingerprint(entry MapEntry, text string) string {
	entry.Label = nil
	raw, _ := json.Marshal(entry)
	return hash([]byte(fmt.Sprintf("%s\n%s\n%s", mapVersion, raw, text)))
}
