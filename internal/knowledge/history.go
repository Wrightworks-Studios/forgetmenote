package knowledge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"forgetmenote/internal/capacity"
	"forgetmenote/internal/workspace"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

func maxHistoryBytes() int    { return capacity.HistoryBytes.Limit() }
func maxHistoryVersions() int { return capacity.HistoryBytes.Limit() / 128 }

const historyCacheVersion = 3 // Rebuild projections made before container classification.

func ownerSyncError(err error) bool {
	return errors.Is(err, workspace.ErrOwnerUnavailable) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}

func historyRevisionValid(revision string) bool {
	if len(revision) != 40 {
		return false
	}
	for _, c := range revision {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

type HistoryStatus struct {
	Revision          string `json:"revision"`
	TargetRevision    string `json:"targetRevision"`
	CompletedRevision string `json:"completedRevision"`
	State             string `json:"state"`
	Commits           int    `json:"commits"`
	TotalCommits      int    `json:"totalCommits"`
	Versions          int    `json:"versions"`
	Total             int    `json:"total"`
	Indexed           int    `json:"indexed"`
	Message           string `json:"message,omitempty"`
}
type HistoryOccurrence struct {
	Container   string `json:"container,omitempty"`
	Priority    int    `json:"priority,omitempty"`
	Path        string `json:"path"`
	Revision    string `json:"revision"`
	CommittedAt string `json:"committedAt"`
	NoteHash    string `json:"noteHash"`
	Current     bool   `json:"current"`
}
type historyVersion struct {
	Path      string                      `json:"path"`
	Blob      string                      `json:"blob"`
	Revisions []workspace.HistoryRevision `json:"revisions"`
}
type historyCache struct {
	Version      int                               `json:"version"`
	Visibility   string                            `json:"visibility"`
	Head         string                            `json:"head"`
	Target       string                            `json:"target"`
	Completed    string                            `json:"completed"`
	Queue        []workspace.HistoryRevision       `json:"queue"`
	Cursor       int                               `json:"cursor"`
	ChangeCursor int                               `json:"changeCursor"`
	Scanned      int                               `json:"scanned"`
	Blobs        map[string]workspace.SnapshotFile `json:"blobs"`
	Versions     map[string]historyVersion         `json:"versions"`
	// Purge is the notebook's latest purge when the cache was written; a
	// cache from before a later purge holds purged text and is not loaded.
	Purge string `json:"purge,omitempty"`
}

func (e *Engine) loadHistory(visibility string) {
	if e.historyLoaded && e.history.Visibility == visibility {
		return
	}
	e.historyLoaded = true
	e.historyFailure = ""
	e.history = historyCache{Version: historyCacheVersion, Visibility: visibility, Blobs: map[string]workspace.SnapshotFile{}, Versions: map[string]historyVersion{}}
	f, err := os.Open(filepath.Join(e.dir, "cache", "history.json"))
	if err != nil {
		return
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, int64(maxHistoryBytes())*2+1))
	var cache historyCache
	if err != nil || len(raw) > maxHistoryBytes()*2 || json.Unmarshal(raw, &cache) != nil {
		return
	}
	if cache.Purge != e.state.Purge {
		// Written before a purge, as on a gateway that was down during it: it
		// holds purged text.
		_ = os.Remove(filepath.Join(e.dir, "cache", "history.json"))
		return
	}
	if cache.Version != historyCacheVersion || cache.Visibility != e.history.Visibility || cache.Blobs == nil || cache.Versions == nil || len(cache.Versions) > maxHistoryVersions() || cache.Cursor < 0 || cache.Cursor > len(cache.Queue) {
		return
	}
	if cache.ChangeCursor < 0 || len(cache.Queue) > maxHistoryBytes()/64 {
		return
	}
	for _, revision := range []string{cache.Head, cache.Target, cache.Completed} {
		if revision != "" && !historyRevisionValid(revision) {
			return
		}
	}
	for _, revision := range cache.Queue {
		if !historyRevisionValid(revision.Revision) {
			return
		}
	}
	total := 0
	for id, blob := range cache.Blobs {
		total += len(blob.Content)
		if !historyRevisionValid(id) || total > maxHistoryBytes() || workspace.ContentHash([]byte(blob.Content)) != blob.Hash {
			return
		}
		blob.Metadata, _ = workspace.ParseMetadata([]byte(blob.Content))
		blob.Priority = 0
		cache.Blobs[id] = blob
	}
	records := 0
	for id, version := range cache.Versions {
		records += len(version.Revisions)
		if len(version.Revisions) == 0 || records > maxHistoryBytes()/64 || id != workspace.ContentHash([]byte(version.Path+"\x00"+version.Blob)) {
			return
		}
		for _, revision := range version.Revisions {
			if !historyRevisionValid(revision.Revision) {
				return
			}
		}
		if _, ok := cache.Blobs[version.Blob]; !ok {
			return
		}
	}
	e.history = cache
}

// syncHistory advances a fixed target in bounded commits and persists the cursor
// together with its discoveries. A failed batch never advances its checkpoint.
// Derived-history failures are reported separately so current search stays usable.
func (e *Engine) syncHistory(ctx context.Context, batch int) error {
	// Requested history remains scheduled while its cache awaits validation.
	if !e.historyResume {
		e.historyResume = true
		e.signalContinuation()
	}
	if err := e.refreshStored(ctx); err != nil {
		return err
	}
	// Serialize catalogue scans without holding the general index lock while
	// another request waits, or while immutable Git objects are being read.
	e.mu.Unlock()
	e.historyScan.Lock()
	e.mu.Lock()
	defer e.historyScan.Unlock()
	visibility, err := e.Store.HistoryVisibilityID(ctx)
	if err != nil {
		return err
	}
	e.loadHistory(visibility)
	changed := false
	status, err := e.Store.Status(ctx)
	if err != nil {
		return err
	}
	if e.history.Head != status.Revision {
		if e.history.Target != "" {
			if err := e.Store.AssertSnapshot(ctx, e.history.Target); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if !errors.Is(err, workspace.ErrConflict) {
					return err
				}
				e.history = historyCache{Version: historyCacheVersion, Visibility: visibility, Blobs: map[string]workspace.SnapshotFile{}, Versions: map[string]historyVersion{}}
			}
		}
		e.history.Head = status.Revision
		changed = true
	}
	if e.history.Target == "" || e.history.Cursor == len(e.history.Queue) && e.history.Completed != status.Revision {
		after := e.history.Completed
		e.mu.Unlock()
		queue, err := e.Store.HistoryRevisions(ctx, status.Revision, after)
		e.mu.Lock()
		if err != nil {
			if ownerSyncError(err) {
				return err
			}
			e.historyFailure = err.Error()
			return nil
		}
		e.history.Target, e.history.Queue, e.history.Cursor = status.Revision, queue, 0
		e.historyFailure = ""
		changed = true
	}
	for n := 0; n < batch && e.history.Cursor < len(e.history.Queue); n++ {
		revision := e.history.Queue[e.history.Cursor]
		e.mu.Unlock()
		snapshot, err := e.Store.HistoryNotes(ctx, revision.Revision, e.history.ChangeCursor, e.history.Blobs)
		e.mu.Lock()
		if err != nil {
			if ownerSyncError(err) {
				return err
			}
			e.historyFailure = err.Error()
			break
		}
		if snapshot.Visibility != visibility {
			e.historyLoaded = false
			return fmt.Errorf("note visibility changed during a history batch; retry: %w", workspace.ErrConflict)
		}
		total := 0
		for _, blob := range e.history.Blobs {
			total += len(blob.Content)
		}
		added := map[string]workspace.SnapshotFile{}
		versions := map[string]historyVersion{}
		for _, file := range snapshot.Files {
			blob := file.Blob
			if _, exists := e.history.Blobs[blob]; !exists {
				if _, exists = added[blob]; !exists {
					total += len(file.Content)
					added[blob] = file.SnapshotFile
				}
			}
			id := workspace.ContentHash([]byte(file.Path + "\x00" + blob))
			version, exists := e.history.Versions[id]
			if !exists {
				version = historyVersion{Path: file.Path, Blob: blob}
			}
			version.Revisions = append(append([]workspace.HistoryRevision(nil), version.Revisions...), revision)
			versions[id] = version
		}
		newVersions := len(e.history.Versions)
		records := len(snapshot.Files)
		for _, version := range e.history.Versions {
			records += len(version.Revisions)
		}
		for id := range versions {
			if _, exists := e.history.Versions[id]; !exists {
				newVersions++
			}
		}
		if total > maxHistoryBytes() || newVersions > maxHistoryVersions() || records > maxHistoryBytes()/64 {
			e.historyFailure = capacity.HistoryBytes.Error().Error() + "; historical results cover the scanned commits only"
			break
		}
		for id, blob := range added {
			e.history.Blobs[id] = blob
		}
		for id, version := range versions {
			e.history.Versions[id] = version
		}
		e.history.ChangeCursor = snapshot.Next
		if snapshot.Done {
			e.history.Cursor++
			e.history.Scanned++
			e.history.ChangeCursor = 0
		}
		e.historyFailure = ""
		changed = true
	}
	if e.history.Cursor == len(e.history.Queue) {
		e.history.Completed = e.history.Target
	}
	if changed || e.historyPersistFailure != "" {
		// Owner configuration can change between the immutable Git reads above.
		// An unvalidated scan must never become a reusable completion checkpoint.
		currentVisibility, err := e.Store.HistoryVisibilityID(ctx)
		if err == nil && currentVisibility != visibility {
			err = fmt.Errorf("note visibility changed while scanning history; retry: %w", workspace.ErrConflict)
		}
		if err != nil {
			e.historyLoaded = false
			return err
		}
		e.historyDirty = true
	}
	if e.historyDirty {
		e.history.Purge = e.state.Purge
		e.mu.Unlock()
		err := persistWithDirectorySync(filepath.Join(e.dir, "cache", "history.json"), e.history, e.historySync)
		e.mu.Lock()
		if err != nil {
			e.historyPersistFailure = "Historical index checkpoint could not be saved: " + err.Error()
		} else {
			e.historyDirty = false
			e.historyPersistFailure = ""
		}
	}
	if e.history.Completed != status.Revision && e.historyFailure == "" {
		e.signalContinuation()
	}
	return nil
}

func (e *Engine) historyConfig() Config {
	c := e.state.Config
	if c.PassageRunes < 128 || c.PassageRunes > 4096 {
		c.PassageRunes = 1400
	}
	return c
}
func (e *Engine) historyParts(version historyVersion) []Passage {
	if e.historySplits == nil {
		e.historySplits = map[string][]Passage{}
	}
	config := e.historyConfig()
	key := config.ID() + "\x00" + version.Blob
	if parts, ok := e.historySplits[key]; ok {
		return parts
	}
	file := e.history.Blobs[version.Blob]
	file.Path = "history.md"
	parts, err := Split(workspace.Snapshot{Files: []workspace.SnapshotFile{file}}, config)
	if err != nil {
		e.historyFailure = err.Error()
		return nil
	}
	e.historySplits[key] = parts
	return parts
}
func (e *Engine) historyPassages(ctx context.Context) ([]Passage, error) {
	return e.historyPassagesFor(ctx, e.history.Versions, nil)
}
func (e *Engine) historyPassagesFor(ctx context.Context, versions map[string]historyVersion, scope *workspace.ContentScope) ([]Passage, error) {
	if e.historySplits == nil {
		e.historySplits = map[string][]Passage{}
	}
	paths := []string{}
	for _, version := range versions {
		paths = append(paths, version.Path)
	}
	allowed, err := e.Store.VisibleHistoryPaths(ctx, paths)
	if err != nil {
		return nil, err
	}
	result := []Passage{}
	ids := []string{}
	for id := range versions {
		ids = append(ids, id)
	}
	e.sortHistoryVersions(ids, versions, scope)
	for _, id := range ids {
		version := versions[id]
		if !allowed[version.Path] {
			continue
		}
		config := e.historyConfig()
		passages := e.historyParts(version)
		for _, p := range passages {
			if len(result) >= maxHistoryBytes()/64 {
				e.historyFailure = "Historical passage occurrence limit reached; results are partial."
				return result, nil
			}
			p.Path, p.Revision = version.Path, version.Revisions[0].Revision
			if scope != nil {
				p.Priority = scope.Tier(e.history.Blobs[version.Blob].Metadata)
			}
			p.ID = workspace.ContentHash([]byte(id + "\x00" + p.ID))
			citation := url.URL{Path: "/" + p.Path, Fragment: p.Anchor, RawQuery: url.Values{"fmn-rev": {p.Revision}, "fmn-passage": {p.Hash}, "fmn-chars": {strconv.Itoa(config.PassageRunes)}}.Encode()}
			p.Citation = citation.String()
			result = append(result, p)
		}
	}
	return result, nil
}
func (e *Engine) historyStatus() HistoryStatus {
	h := HistoryStatus{Revision: e.history.Head, TargetRevision: e.history.Target, CompletedRevision: e.history.Completed, State: "scanning", Commits: e.history.Scanned, TotalCommits: e.history.Scanned + len(e.history.Queue) - e.history.Cursor, Versions: len(e.history.Versions)}
	if !e.historyLoaded {
		h = HistoryStatus{Revision: h.Revision, State: "scanning"}
		if e.historySyncFailure != "" {
			h.Message = e.historySyncFailure
		}
		if e.historyOwnerFailure != "" {
			h.State, h.Message = "unavailable", e.historyOwnerFailure
		}
		return h
	}
	seen := map[string]bool{}
	paths := []string{}
	for _, version := range e.history.Versions {
		paths = append(paths, version.Path)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	visibility, err := e.Store.HistoryVisibilityID(ctx)
	if err != nil {
		return HistoryStatus{Revision: h.Revision, State: "unavailable", Message: err.Error()}
	}
	if visibility != e.history.Visibility {
		e.signalContinuation()
		return HistoryStatus{Revision: h.Revision, TargetRevision: h.Revision, State: "scanning", Message: "Note visibility changed; history must be scanned again."}
	}
	allowed, err := e.Store.VisibleHistoryPaths(ctx, paths)
	if err != nil {
		h.State = "unavailable"
		h.Message = err.Error()
		return h
	}
	// A successful owner visibility check supersedes its transport failure.
	// Catalogue limits, persistence and provider failures have different owners.
	e.historyOwnerFailure = ""
	count := 0
	allIndexed := true
	for _, version := range e.history.Versions {
		parts := e.historyParts(version)
		for _, p := range parts {
			if _, exists := e.cache.Vectors[p.Hash]; !exists {
				allIndexed = false
			}
		}
		if !allowed[version.Path] {
			continue
		}
		count += len(parts)
		for _, p := range parts {
			seen[p.Hash] = true
		}
	}
	if count > maxHistoryBytes()/64 {
		e.historyFailure = "Historical passage occurrence limit reached; results are partial."
	}
	h.Total = len(seen)
	for hash := range seen {
		if _, exists := e.cache.Vectors[hash]; exists {
			h.Indexed++
		}
	}
	if e.history.Completed == e.history.Head {
		h.State = "ready"
	}
	if e.state.Config.Enabled && h.Indexed < h.Total && h.State == "ready" {
		h.State = "indexing"
	}
	// Both submission scopes persist into the same content-addressed cache.
	// Clear recoverable provider/submission failures only after validating
	// compatible persisted coverage, never because a filter omitted pending work.
	if e.state.Config.Enabled && e.cache.ConfigID == e.state.Config.ID() && h.State == "ready" && allIndexed && e.historySyncFailure == "" && e.historyFailure == "" && e.historyPersistFailure == "" && e.historyVectorPersistFailure == "" {
		e.historyEmbeddingFailure = ""
		e.historySubmissionFailure = ""
	}
	if e.historySyncFailure != "" {
		h.State, h.Message = "scanning", e.historySyncFailure
	}
	if e.state.Config.Enabled && e.historySubmissionFailure != "" {
		if h.State == "ready" {
			h.State = "indexing"
		}
		h.Message = e.historySubmissionFailure
	}
	if e.historyEmbeddingFailure != "" {
		h.State = "limited"
		h.Message = e.historyEmbeddingFailure
	}
	if e.historyFailure != "" {
		h.State = "limited"
		h.Message = e.historyFailure
	}
	if e.historyPersistFailure != "" {
		h.State = "unavailable"
		h.Message = e.historyPersistFailure
	}
	if e.historyVectorPersistFailure != "" {
		h.State, h.Message = "unavailable", strings.TrimSpace(h.Message+"\n"+e.historyVectorPersistFailure)
	}
	if e.state.Config.Enabled && h.Indexed < h.Total && (len(e.reservedHashes(e.cache.Vectors))+1)*e.state.Config.Dimensions > maxVectorValues() {
		h.State = "limited"
		h.Message = "Historical embedding capacity reached; text history remains available."
	}
	return h
}

func (e *Engine) historySearch(ctx context.Context, query HybridQuery) (HybridResult, error) {
	r := HybridResult{Matches: []SearchHit{}, Mode: query.Mode, Scope: "history", ContentScope: query.contentScope, ScopeID: query.contentScope.ID()}
	if _, err := e.Store.SnapshotScoped(ctx, "", query.contentScope); err != nil {
		return r, err
	}
	if r.Mode == "" {
		r.Mode = "all"
	}
	if r.Mode != "all" && r.Mode != "text" && r.Mode != "meaning" {
		return r, errors.New("search mode must be all, text, or meaning")
	}
	query.Text = strings.TrimSpace(query.Text)
	if query.Text == "" || len(query.Text) > 200 || len(query.Literal) > 200 || len(query.Path) > 512 || len(query.Tag) > 200 || len(query.Status) > 200 {
		return r, errors.New("search input exceeds limits or search text is empty")
	}
	e.mu.Lock()
	if err := e.syncHistory(ctx, 16); err != nil {
		e.mu.Unlock()
		return r, err
	}
	config, provider, target := e.state.Config, e.provider.Load(), e.history.Target
	e.mu.Unlock()
	vector := []float32(nil)
	if r.Mode != "text" {
		if len(query.Vector) > 0 {
			if query.ConfigID != config.ID() {
				return r, errors.New("query embedding configuration does not match the project")
			}
			var err error
			vector, err = normalize(query.Vector, config.Dimensions)
			if err != nil {
				return r, err
			}
		} else if config.Enabled && config.Endpoint && provider != nil {
			searchCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			if r.Mode == "meaning" {
				cancel()
				searchCtx, cancel = context.WithTimeout(ctx, 30*time.Second)
			}
			vectors, err := provider.Embed(searchCtx, config, []string{query.Text})
			cancel()
			if ctx.Err() != nil {
				return r, ctx.Err()
			}
			if err == nil {
				vector = vectors[0]
			} else {
				r.Message = "Semantic search is temporarily unavailable."
			}
		}
		if !config.Enabled {
			vector = nil
			r.Message = "Semantic search is off."
		}
		if vector == nil {
			if r.Message == "" {
				r.Message = "Semantic search needs a server embedding endpoint; agents can supply query vectors."
			}
			if r.Mode == "all" {
				r.Mode = "text"
				r.Message += " Showing text matches."
			}
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.syncHistory(ctx, 0); err != nil {
		return r, err
	}
	if target != e.history.Target || config.ID() != e.state.Config.ID() || config.Enabled != e.state.Config.Enabled {
		vector = nil
		r.Message = "History or model changed during search. Search again for meaning matches."
		if r.Mode == "all" {
			r.Mode = "text"
		}
	}
	h := e.historyStatus()
	r.History = &h
	if h.CompletedRevision != h.Revision {
		r.Message = strings.TrimSpace(r.Message + " Historical scan is incomplete.")
	}
	if h.Message != "" {
		r.Message = strings.TrimSpace(r.Message + " " + h.Message)
	}
	if vector != nil && h.Indexed < h.Total {
		r.Message = strings.TrimSpace(r.Message + " Historical semantic indexing is incomplete.")
	}
	current, err := e.Store.SnapshotScoped(ctx, "", query.contentScope)
	if err != nil {
		return r, err
	}
	r.Visibility = current.Visibility
	currentHashes := map[string]string{}
	for _, file := range current.Files {
		currentHashes[file.Path] = file.Hash
	}
	versions, trashIDs, err := e.searchableHistory(ctx, query.IncludeTrash, &query.contentScope)
	if err != nil {
		return r, err
	}
	passages, err := e.historyPassagesFor(ctx, versions, &query.contentScope)
	if err != nil {
		return r, err
	}
	byVersion := map[string][]Passage{}
	for _, p := range passages {
		byVersion[p.Path+"\x00"+p.NoteHash] = append(byVersion[p.Path+"\x00"+p.NoteHash], p)
	}
	lexical, semantic := map[string]SearchHit{}, map[string]SearchHit{}
	filterPaths, filterTags, filterStatuses := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, version := range versions {
		file := e.history.Blobs[version.Blob]
		if len(byVersion[version.Path+"\x00"+file.Hash]) == 0 {
			continue
		}
		filterPaths[version.Path] = true
		for _, tag := range file.Metadata.Tags {
			filterTags[tag] = true
		}
		if file.Metadata.Status != "" {
			filterStatuses[file.Metadata.Status] = true
		}
	}
	keys := func(values map[string]bool) []string {
		result := make([]string, 0, len(values))
		for value := range values {
			result = append(result, value)
		}
		sort.Strings(result)
		return result
	}
	r.Filters = &HistoryFilters{Paths: keys(filterPaths), Tags: keys(filterTags), Statuses: keys(filterStatuses)}
	queryText := strings.ToLower(query.Text)
	scores := map[string]float64{}
	for versionID, version := range versions {
		file := e.history.Blobs[version.Blob]
		file.Path = version.Path
		if !matches(file, query.SearchQuery) {
			continue
		}
		parts := byVersion[file.Path+"\x00"+file.Hash]
		if len(parts) == 0 {
			continue
		}
		title := noteTitle(file)
		content := workspace.SearchableText(file.Content)
		textMatch := strings.Contains(strings.ToLower(file.Path+"\n"+title+"\n"+content), queryText)
		selected := false
		versionIDs := []string{versionID}
		for _, p := range parts {
			p.Revision = version.Revisions[0].Revision
			citation, err := url.Parse(p.Citation)
			if err != nil {
				return r, err
			}
			values := citation.Query()
			values.Set("fmn-rev", p.Revision)
			citation.RawQuery = values.Encode()
			p.Citation = citation.String()
			hit := SearchHit{Container: file.Metadata.Container, Priority: query.contentScope.Tier(file.Metadata), TrashID: trashIDs[versionID], ID: p.Hash, Path: p.Path, Title: title, Text: p.Text, Revision: p.Revision, Hash: p.Hash, NoteHash: p.NoteHash, Heading: p.Heading, Citation: p.Citation, CommittedAt: version.Revisions[0].Time, Current: currentHashes[p.Path] == p.NoteHash}
			hit.displayPassage = &p
			hit.sourceVersions = versionIDs
			literalPart := strings.Contains(strings.ToLower(p.Text), queryText)
			if textMatch && literalPart {
				selected = true
				hit.Score = 1
				if strings.Contains(strings.ToLower(title+"\n"+file.Path), queryText) {
					hit.Score = 2
				}
				if strings.EqualFold(title, query.Text) {
					hit.Score = 3
				}
				mergeHistoryHit(lexical, hit)
			}
			if vector != nil {
				v, ok := e.cache.Vectors[p.Hash]
				if !ok {
					continue
				}
				score, done := scores[p.Hash]
				if !done {
					for j, x := range vector {
						score += float64(x) * float64(v[j])
					}
					scores[p.Hash] = score
				}
				hit.Score = score
				mergeHistoryHit(semantic, hit)
			}
		}
		if textMatch && !selected {
			// Cross-split and metadata matches cite the complete note. Their identity
			// must never claim the hash or anchor of an unrelated final passage.
			p := parts[0]
			p.Revision = version.Revisions[0].Revision
			citation := url.URL{Path: "/" + p.Path, RawQuery: url.Values{"fmn-rev": {p.Revision}}.Encode()}
			hit := SearchHit{Container: file.Metadata.Container, Priority: query.contentScope.Tier(file.Metadata), TrashID: trashIDs[versionID], ID: "note:" + workspace.ContentHash([]byte(file.Hash+"\x00"+queryText)), Path: p.Path, Title: title, Text: excerpt(content, queryText), Revision: p.Revision, NoteHash: p.NoteHash, Citation: citation.String(), CommittedAt: version.Revisions[0].Time, Current: currentHashes[p.Path] == p.NoteHash, Score: 1, sourceVersions: versionIDs}
			if strings.Contains(strings.ToLower(title+"\n"+file.Path), queryText) {
				hit.Score = 2
			}
			if strings.EqualFold(title, query.Text) {
				hit.Score = 3
			}
			mergeHistoryHit(lexical, hit)
		}
	}
	ranked := map[string]SearchHit{}
	if r.Mode != "meaning" {
		for i, hit := range sortedHistoryHits(lexical) {
			hit.Score = 1 / float64(61+i)
			ranked[hit.ID] = hit
		}
	}
	for i, hit := range sortedHistoryHits(semantic) {
		if old, ok := ranked[hit.ID]; ok {
			old.Score += 1 / float64(61+i)
			ranked[hit.ID] = old
		} else {
			hit.Score = 1 / float64(61+i)
			ranked[hit.ID] = hit
		}
	}
	r.Matches = sortedHistoryHits(ranked)
	limit := query.Limit
	if limit < 1 || limit > 50 {
		limit = 20
	}
	if len(r.Matches) > limit {
		r.Matches = r.Matches[:limit]
	}
	// Only retained hits expand provenance, with an explicit response bound.
	for i := range r.Matches {
		hit := &r.Matches[i]

		if hit.displayPassage != nil {
			var content string
			for _, versionID := range hit.sourceVersions {
				version := versions[versionID]
				file := e.history.Blobs[version.Blob]
				if file.Hash == hit.displayPassage.NoteHash {
					content = file.Content
					break
				}
			}
			excerpts, err := DisplayExcerpts([]byte(content), []Passage{*hit.displayPassage}, query.Text)
			if err != nil {
				return r, err
			}
			hit.Excerpt = excerpts[0]
		} else if len(hit.sourceVersions) > 0 {
			version := versions[hit.sourceVersions[0]]
			hit.Excerpt = workspace.SearchExcerpt([]byte(e.history.Blobs[version.Blob].Content), query.Text)
		}

		e.sortHistoryVersions(hit.sourceVersions, versions, &query.contentScope)
		for _, id := range hit.sourceVersions {
			version := versions[id]
			noteHash := e.history.Blobs[version.Blob].Hash
			hit.OccurrenceCount += len(version.Revisions)
			for _, rev := range version.Revisions {
				if len(hit.Occurrences) >= 256 {
					break
				}
				hit.Occurrences = append(hit.Occurrences, HistoryOccurrence{Container: e.history.Blobs[version.Blob].Metadata.Container, Priority: query.contentScope.Tier(e.history.Blobs[version.Blob].Metadata), Path: version.Path, Revision: rev.Revision, CommittedAt: rev.Time, NoteHash: noteHash, Current: currentHashes[version.Path] == noteHash})
			}
		}
		hit.OccurrencesTruncated = hit.OccurrenceCount > len(hit.Occurrences)
		sort.Slice(hit.Occurrences, func(i, j int) bool {
			a, b := hit.Occurrences[i], hit.Occurrences[j]
			if a.Priority != b.Priority {
				return a.Priority < b.Priority
			}
			if a.CommittedAt != b.CommittedAt {
				return a.CommittedAt > b.CommittedAt
			}
			if a.Path != b.Path {
				return a.Path < b.Path
			}
			return a.Revision < b.Revision
		})
	}
	if err := e.validateSearchSnapshot(ctx, current, query.contentScope); err != nil {
		return HybridResult{}, err
	}
	return r, nil
}

func mergeHistoryHit(hits map[string]SearchHit, hit SearchHit) {
	if previous, exists := hits[hit.ID]; exists {
		versions := previous.sourceVersions
		for _, id := range hit.sourceVersions {
			found := false
			for _, old := range versions {
				if old == id {
					found = true
					break
				}
			}
			if !found {
				versions = append(versions, id)
			}
		}
		if previous.Priority < hit.Priority || previous.Priority == hit.Priority && (previous.CommittedAt > hit.CommittedAt || previous.CommittedAt == hit.CommittedAt && previous.Path < hit.Path) {
			hit = previous
		}
		hit.sourceVersions = versions
		if previous.Priority == hit.Priority && previous.Score > hit.Score {
			hit.Score = previous.Score
		}
	}
	hits[hit.ID] = hit
}
func sortedHistoryHits(hits map[string]SearchHit) []SearchHit {
	result := make([]SearchHit, 0, len(hits))
	for _, hit := range hits {
		sort.Slice(hit.Occurrences, func(i, j int) bool {
			a, b := hit.Occurrences[i], hit.Occurrences[j]
			if a.Priority != b.Priority {
				return a.Priority < b.Priority
			}
			if a.CommittedAt != b.CommittedAt {
				return a.CommittedAt > b.CommittedAt
			}
			if a.Path != b.Path {
				return a.Path < b.Path
			}
			return a.Revision < b.Revision
		})
		result = append(result, hit)
	}
	sort.Slice(result, func(i, j int) bool {
		a, b := result[i], result[j]
		if a.Priority != b.Priority {
			return a.Priority < b.Priority
		}
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		if a.CommittedAt != b.CommittedAt {
			return a.CommittedAt > b.CommittedAt
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return a.ID < b.ID
	})
	return result
}
func (e *Engine) historySemantic(ctx context.Context, query SearchQuery) ([]Match, error) {
	// A vector-only query still uses the same historical permissions and filters.
	if query.Text == "" {
		query.Text = "semantic query"
	}
	result, err := e.historySearch(ctx, HybridQuery{SearchQuery: query, Mode: "meaning"})
	if err != nil {
		return nil, err
	}
	if h := result.History; h != nil && (h.CompletedRevision != h.Revision || h.State == "limited" || h.State == "unavailable") {
		return nil, errors.New("historical search coverage is partial; use hybrid_search to inspect its coverage and results")
	}
	matches := []Match{}
	for _, hit := range result.Matches {
		matches = append(matches, Match{Passage: Passage{Container: hit.Container, Priority: hit.Priority, ID: hit.ID, Path: hit.Path, Heading: hit.Heading, Hash: hit.Hash, NoteHash: hit.NoteHash, Text: hit.Text, Revision: hit.Revision, Citation: hit.Citation}, Score: hit.Score})
	}
	return matches, nil
}

// Stable tier ordering is applied before passage/occurrence bounds, not merely
// to the final response. Every historical version owns its classification.
func (e *Engine) sortHistoryVersions(ids []string, versions map[string]historyVersion, scope *workspace.ContentScope) {
	sort.Slice(ids, func(i, j int) bool {
		a, b := versions[ids[i]], versions[ids[j]]
		if scope != nil {
			ta, tb := scope.Tier(e.history.Blobs[a.Blob].Metadata), scope.Tier(e.history.Blobs[b.Blob].Metadata)
			if ta != tb {
				return ta < tb
			}
		}
		if a.Revisions[0].Time != b.Revisions[0].Time {
			return a.Revisions[0].Time > b.Revisions[0].Time
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return ids[i] < ids[j]
	})
}
