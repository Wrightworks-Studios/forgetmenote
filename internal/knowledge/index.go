package knowledge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"forgetmenote/internal/capacity"
	"forgetmenote/internal/durable"
	"forgetmenote/internal/pg"
	"forgetmenote/internal/workspace"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Checkpoint struct {
	Consumer    string `json:"consumer"`
	User        string `json:"user"`
	Revision    string `json:"revision"`
	CompletedAt string `json:"completedAt"`
	Summary     string `json:"summary"`
}
type state struct {
	Config      Config                `json:"config"`
	Checkpoints map[string]Checkpoint `json:"checkpoints"`
	Proposals   map[string]Proposal   `json:"proposals"`
	// Purge is the latest purge whose derived data was cleared; an engine
	// that finds another forgets what it derived (purge.go).
	Purge string `json:"purge,omitempty"`
}
type vectorCache struct {
	ConfigID string               `json:"configId"`
	Revision string               `json:"revision"`
	Vectors  map[string][]float32 `json:"vectors"`
}

var errStaleEmbeddingConfiguration = errors.New("embedding configuration changed; request current passages")

type Engine struct {
	*Processing
	batchWork           sync.Mutex
	mapCache            *noteMapCache
	writingCache        map[string]WritingAnalysis
	writingCacheBytes   int
	retrievalSettings   RetrievalSettings
	referenceRevision   string
	referenceVisibility string
	referenceLinks      map[string][]Reference
	started             bool
	lifecycle           sync.Mutex
	closed              bool

	mu                 sync.Mutex
	Store              *workspace.Store
	db                 *pg.DB
	notebook           string
	dir                string
	state              state
	stateVersion       int64
	retrievalVersion   int64
	cache              vectorCache
	vectorVersion      int64
	stored             map[string]struct{}
	cacheDirty         bool
	snapshot           workspace.Snapshot
	snapshotVisibility string
	passages           []Passage
	retrieval          *bm25Index
	provider           atomic.Pointer[Endpoint]
	history            historyCache
	historySplits      map[string][]Passage
	historyScan        sync.Mutex
	historyDirty       bool
	historyLoaded      bool
	historySync        func(string) error
	wake               chan struct{}
	stop               chan struct{}
	done               chan struct{}
	// batchDeadline bounds one worker batch, including owner reads; tests
	// shorten it.
	batchDeadline time.Duration
}
type IndexStatus struct {
	Config          Config         `json:"config"`
	ConfigID        string         `json:"configId"`
	Revision        string         `json:"revision"`
	IndexedRevision string         `json:"indexedRevision"`
	Total           int            `json:"total"`
	Indexed         int            `json:"indexed"`
	State           string         `json:"state"`
	Message         string         `json:"message,omitempty"`
	BrowserSearch   bool           `json:"browserSearch"`
	History         *HistoryStatus `json:"history,omitempty"`
}
type Pending struct {
	Status   IndexStatus `json:"status"`
	Passages []Passage   `json:"passages"`
}
type VectorSubmission struct {
	ID       string    `json:"id"`
	Hash     string    `json:"hash"`
	NoteHash string    `json:"noteHash"`
	Vector   []float32 `json:"vector"`
}
type Submission struct {
	Scope    string             `json:"scope,omitempty"`
	Revision string             `json:"revision"`
	ConfigID string             `json:"configId"`
	Vectors  []VectorSubmission `json:"vectors"`
}
type SearchQuery struct {
	ContainerSelection
	contentScope workspace.ContentScope
	IncludeTrash bool      `json:"includeTrash,omitempty"`
	Scope        string    `json:"scope,omitempty"`
	Text         string    `json:"text,omitempty"`
	Literal      string    `json:"literal,omitempty"`
	Path         string    `json:"path,omitempty"`
	Tag          string    `json:"tag,omitempty"`
	Status       string    `json:"status,omitempty"`
	ConfigID     string    `json:"configId,omitempty"`
	Vector       []float32 `json:"vector,omitempty"`
	Limit        int       `json:"limit,omitempty"`
}
type Match struct {
	Passage
	Score float64 `json:"score"`
}

func Open(db *pg.DB, store *workspace.Store) (*Engine, error) {
	return OpenWithProcessing(db, store, nil)
}

// OpenWithProcessing preserves the notebook's small processing lifetime while
// its derived corpus and caches are evicted. The caller must close and drain the
// previous engine before sharing this state with another engine.
func OpenWithProcessing(db *pg.DB, store *workspace.Store, processing *Processing) (*Engine, error) {
	if processing == nil {
		processing = NewProcessing(nil)
	}
	e := &Engine{historySync: syncKnowledgeDirectory, Processing: processing, Store: store, db: db, notebook: notebookKey(store), dir: filepath.Dir(store.Root), state: state{Checkpoints: map[string]Checkpoint{}, Proposals: map[string]Proposal{}}, stateVersion: -1, retrievalVersion: -1, cache: vectorCache{Vectors: map[string][]float32{}}, wake: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{}), batchDeadline: 35 * time.Second}
	ctx := context.Background()
	if err := e.refresh(ctx); err != nil {
		return nil, err
	}
	// Preserve fatal knowledge configuration errors even when retrieval settings
	// also need repair and eager gateway initialization isolates that failure.
	if err := e.refreshRetrieval(ctx); err != nil {
		return nil, err
	}
	if err := e.loadVectorCache(ctx); err != nil {
		return nil, err
	}
	_, err := os.Stat(filepath.Join(e.dir, "cache", "history.json"))
	e.historyResume = e.historyResume || err == nil
	return e, nil
}
func persist(path string, v any) error {
	return persistWithDirectorySync(path, v, syncKnowledgeDirectory)
}

// Tests may report a final publication-sync failure while keeping the actual
// file write, file sync and rename intact at this filesystem boundary.
func persistWithDirectorySync(path string, v any, syncDirectory func(string) error) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	return durable.WriteFile(path, knowledgeTemporary+"*", raw, syncDirectory)
}
func syncKnowledgeDirectory(path string) error { return durable.SyncDir(path) }

type vectorPersistenceError struct{ error }

func (e *vectorPersistenceError) Unwrap() error { return e.error }

// A historical vector-write error is separate from catalogue/owner/provider
// errors. Both scopes write the same cache, so either can prove storage recovered.
// Failed historical work must not make a complete current index unavailable.
func (e *Engine) persistVectors(next vectorCache, historical bool) error {
	if err := e.writeVectors(next); err != nil {
		if historical {
			fresh := e.historyVectorPersistFailure == ""
			e.historyVectorPersistFailure = "Semantic vector cache could not be saved: " + err.Error()
			if fresh {
				e.signalContinuation()
			}
			return &vectorPersistenceError{err}
		}
		fresh := e.currentVectorPersistFailure == ""
		e.currentVectorPersistFailure = "Semantic vector cache could not be saved: " + err.Error()
		if fresh {
			e.signalContinuation()
		}
		return err
	}
	e.currentVectorPersistFailure = ""
	e.historyVectorPersistFailure = ""
	e.cacheDirty = false
	return nil
}

// Allow for float storage, serialization and map overhead while retaining
// the existing portable cache format.
func maxVectorValues() int { return capacity.IndexBytes.Limit() / 16 }

// ConfigureAgent supplies initial splitting defaults and preserves operator choices.
// Reading the existing configuration and updating it share the same lock.
func (e *Engine) ConfigureAgent(ctx context.Context, model string, dimensions int, rebuild bool) (IndexStatus, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.refresh(ctx); err != nil {
		return IndexStatus{}, err
	}
	config := e.state.Config
	config.Enabled, config.Model, config.Dimensions = true, model, dimensions
	if config.PassageRunes == 0 {
		config.PassageRunes = 1400
	}
	if config.Splitter == "" {
		config.Splitter = "markdown-heading-v1"
	}
	return e.configure(ctx, config, rebuild)
}
func (e *Engine) Configure(ctx context.Context, config Config, rebuild bool) (IndexStatus, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.refresh(ctx); err != nil {
		return IndexStatus{}, err
	}
	return e.configure(ctx, config, rebuild)
}

// Config reports the last configuration this engine read, rereading it first
// when the database answers.
func (e *Engine) Config() Config {
	e.mu.Lock()
	defer e.mu.Unlock()
	_ = e.refresh(context.Background())
	return e.state.Config
}
func (e *Engine) configure(ctx context.Context, config Config, rebuild bool) (IndexStatus, error) {
	if err := config.Validate(); err != nil {
		return IndexStatus{}, err
	}
	if e.state.Config.Model != "" && e.state.Config.ID() != config.ID() && !rebuild {
		return IndexStatus{}, errors.New("model, dimensions or splitter changed; confirm a rebuild")
	}
	next := e.state
	next.Config = config
	reset := config.ID() != e.state.Config.ID() || rebuild
	if err := e.saveState(ctx, next, nil, reset); err != nil {
		return IndexStatus{}, err
	}
	if reset {
		e.resetIndex()
	}
	e.signal()
	if err := e.sync(ctx); err != nil {
		return IndexStatus{}, err
	}
	return e.status(), nil
}
func (e *Engine) signal()             { e.wakeProcessing(true) }
func (e *Engine) signalContinuation() { e.wakeProcessing(false) }
func (e *Engine) wakeProcessing(reset bool) {
	if e.notify != nil {
		e.notify(reset)
		return
	}
	select {
	case e.wake <- struct{}{}:
	default:
	}
}
func (e *Engine) sync(ctx context.Context) error {
	if err := e.refreshStored(ctx); err != nil {
		return err
	}
	if !e.state.Config.Enabled {
		return nil
	}
	visibility, err := e.Store.HistoryVisibilityID(ctx)
	if err != nil {
		return err
	}
	status, err := e.Store.Status(ctx)
	if err != nil {
		return err
	}
	if status.Revision == e.snapshot.Revision && visibility == e.snapshotVisibility {
		if e.currentVectorPersistFailure != "" && e.complete() {
			if err := e.persistVectors(e.cache, false); err != nil {
				return err
			}
		}
		e.syncFailure = ""
		if e.complete() {
			e.failure = ""
		}
		return nil
	}
	snapshot, err := e.Store.Snapshot(ctx, status.Revision)
	if err != nil {
		return err
	}
	currentVisibility, err := e.Store.HistoryVisibilityID(ctx)
	if err != nil {
		return err
	}
	if snapshot.Visibility != visibility || currentVisibility != visibility {
		return fmt.Errorf("note visibility changed while loading the index; retry: %w", workspace.ErrConflict)
	}
	passages, err := Split(snapshot, e.state.Config)
	if err != nil {
		return err
	}
	currentHashes := map[string]bool{}
	for _, p := range passages {
		currentHashes[p.Hash] = true
	}
	if len(currentHashes)*e.state.Config.Dimensions > maxVectorValues() {
		return capacity.IndexBytes.Error()
	}
	vectors := map[string][]float32{}
	changed := e.cacheDirty
	for hash, v := range e.cache.Vectors {
		if normalized, validErr := normalize(v, e.state.Config.Dimensions); validErr == nil {
			vectors[hash] = normalized
			changed = changed || !slices.Equal(v, normalized)
		} else {
			changed = true
		}
	}
	// Current notes retain capacity even when history fills the reusable cache.
	spare := maxVectorValues()/e.state.Config.Dimensions - len(currentHashes)
	for hash := range vectors {
		if !currentHashes[hash] {
			if spare == 0 {
				delete(vectors, hash)
				changed = true
			} else {
				spare--
			}
		}
	}
	next := e.cache
	next.Vectors = vectors
	next.ConfigID = e.state.Config.ID()
	complete := true
	for hash := range currentHashes {
		if _, ok := vectors[hash]; !ok {
			complete = false
			break
		}
	}
	if complete {
		next.Revision = snapshot.Revision
		// Reopening an unchanged complete index must not rewrite its paid
		// vectors. A changed file or unresolved publication failure still
		// follows the durable write path before installing the snapshot.
		unchanged := e.vectorsUnchanged(ctx)
		if changed || next.ConfigID != e.cache.ConfigID || next.Revision != e.cache.Revision || !unchanged || e.failure != "" || e.syncFailure != "" || e.historyVectorPersistFailure != "" || e.currentVectorPersistFailure != "" {
			if err = e.persistVectors(next, false); err != nil {
				return err
			}
		}
	}
	// A failed cache write must be retried, not published as a successful hit.
	e.snapshot = snapshot
	e.snapshotVisibility = visibility
	e.passages = passages
	e.cache = next
	e.syncFailure = ""
	if complete {
		// A successful owner sync with no outstanding indexing work supersedes
		// prior current-index failures; pending work keeps its provider failure.
		e.failure = ""
	}
	return nil
}
func (e *Engine) complete() bool {
	for _, p := range e.passages {
		if _, ok := e.cache.Vectors[p.Hash]; !ok {
			return false
		}
	}
	return true
}
func (e *Engine) status() IndexStatus {
	c := e.state.Config
	r := IndexStatus{Config: c, ConfigID: c.ID(), Revision: e.snapshot.Revision, IndexedRevision: e.cache.Revision, Total: len(e.passages), State: "disabled", BrowserSearch: c.Enabled && c.Endpoint && e.provider.Load() != nil}
	if e.historyLoaded || e.historyResume {
		h := e.historyStatus()
		r.History = &h
	}
	if !c.Enabled {
		return r
	}
	for _, p := range e.passages {
		if _, ok := e.cache.Vectors[p.Hash]; ok {
			r.Indexed++
		}
	}
	r.State = "agent-required"
	r.Message = "Use a connected agent to supply compatible document and query embeddings."
	if r.Indexed == r.Total {
		r.State = "ready"
		r.Message = ""
	}
	if c.Endpoint {
		if e.provider.Load() == nil {
			r.State = "unavailable"
			r.Message = "The operator must configure an embedding endpoint on this server."
		} else if r.Indexed < r.Total {
			r.State = "indexing"
			r.Message = "Committed passages are waiting for embeddings."
		}
	}
	if e.failure != "" {
		r.State = "unavailable"
		r.Message = e.failure
	}
	if e.currentVectorPersistFailure != "" {
		r.State, r.Message = "unavailable", e.currentVectorPersistFailure
	}
	if e.syncFailure != "" {
		r.State = "unavailable"
		r.Message = e.syncFailure
	}
	return r
}
func (e *Engine) Pending(ctx context.Context, limit int) (Pending, error) {
	return e.PendingScope(ctx, limit, "")
}
func (e *Engine) PendingScope(ctx context.Context, limit int, scope string) (Pending, error) {
	if scope != "" && scope != "current" && scope != "history" {
		return Pending{}, errors.New("search scope must be current or history")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.sync(ctx); err != nil {
		return Pending{}, err
	}
	if scope == "history" {
		if err := e.syncHistory(ctx, 16); err != nil {
			return Pending{}, err
		}
		// Both the current snapshot and historical policy have now been
		// validated. Neither vector coverage nor unrelated errors prove this.
		e.historySyncFailure = ""
	}
	if limit < 1 || limit > 64 {
		limit = 32
	}
	result := Pending{Status: e.status(), Passages: []Passage{}}
	if scope == "history" {
		result.Status.Revision = e.history.Target
	}
	if !e.state.Config.Enabled {
		return result, nil
	}
	seen := map[string]bool{}
	reserved := e.reservedHashes(e.cache.Vectors)
	passages := e.passages
	if scope == "history" {
		var err error
		passages, err = e.historyPassages(ctx)
		if err != nil {
			return Pending{}, err
		}
	}
	for _, p := range passages {
		if _, ok := e.cache.Vectors[p.Hash]; !ok && !seen[p.Hash] {
			if scope == "history" && !reserved[p.Hash] {
				if (len(reserved)+1)*e.state.Config.Dimensions > maxVectorValues() {
					continue
				}
				reserved[p.Hash] = true
			}
			result.Passages = append(result.Passages, p)
			seen[p.Hash] = true
			if len(result.Passages) >= limit {
				break
			}
		}
	}
	return result, nil
}

func (e *Engine) reservedHashes(vectors map[string][]float32) map[string]bool {
	hashes := map[string]bool{}
	for hash := range vectors {
		hashes[hash] = true
	}
	for _, p := range e.passages {
		hashes[p.Hash] = true
	}
	return hashes
}
func normalize(vector []float32, dimensions int) ([]float32, error) {
	if len(vector) != dimensions || dimensions < 1 || dimensions > 4096 {
		return nil, errors.New("embedding dimensions do not match the active configuration")
	}
	norm := float64(0)
	for _, v := range vector {
		f := float64(v)
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, errors.New("embeddings must contain finite numbers")
		}
		norm += f * f
	}
	if norm == 0 || math.IsInf(norm, 0) {
		return nil, errors.New("embedding must have a finite nonzero magnitude")
	}
	norm = math.Sqrt(norm)
	result := make([]float32, len(vector))
	for i, v := range vector {
		result[i] = float32(float64(v) / norm)
	}
	return result, nil
}
func (e *Engine) Submit(ctx context.Context, submission Submission) (IndexStatus, error) {
	if submission.Scope != "" && submission.Scope != "current" && submission.Scope != "history" {
		return IndexStatus{}, errors.New("search scope must be current or history")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.sync(ctx); err != nil {
		return IndexStatus{}, err
	}
	if !e.state.Config.Enabled || submission.ConfigID != e.state.Config.ID() {
		return IndexStatus{}, errStaleEmbeddingConfiguration
	}
	if submission.Scope == "history" {
		if err := e.syncHistory(ctx, 0); err != nil {
			return IndexStatus{}, err
		}
		if !e.state.Config.Enabled || submission.ConfigID != e.state.Config.ID() {
			return IndexStatus{}, errStaleEmbeddingConfiguration
		}
		if err := e.Store.AssertSnapshot(ctx, submission.Revision); err != nil {
			return IndexStatus{}, err
		}
	} else if submission.Revision != e.snapshot.Revision {
		return IndexStatus{}, workspace.ErrConflict
	}
	if len(submission.Vectors) < 1 || len(submission.Vectors) > 64 {
		return IndexStatus{}, errors.New("submit 1–64 vectors per batch")
	}
	byID := map[string]Passage{}
	passages := e.passages
	if submission.Scope == "history" {
		var err error
		passages, err = e.historyPassages(ctx)
		if err != nil {
			return IndexStatus{}, err
		}
	}
	for _, p := range passages {
		byID[p.ID] = p
	}
	added := map[string][]float32{}
	values := 0
	for _, v := range submission.Vectors {
		p, ok := byID[v.ID]
		if !ok || p.Hash != v.Hash || p.NoteHash != v.NoteHash {
			return IndexStatus{}, workspace.ErrConflict
		}
		values += len(v.Vector)
		if values > 131072 {
			return IndexStatus{}, errors.New("embedding batch exceeds 131072 vector values")
		}
		normalized, err := normalize(v.Vector, e.state.Config.Dimensions)
		if err != nil {
			return IndexStatus{}, err
		}
		if _, exists := added[p.Hash]; exists {
			return IndexStatus{}, errors.New("duplicate passage content in embedding batch")
		}
		added[p.Hash] = normalized
	}
	// All inputs validate before any shared index state changes.
	next := vectorCache{ConfigID: e.cache.ConfigID, Revision: e.cache.Revision, Vectors: map[string][]float32{}}
	for hash, v := range e.cache.Vectors {
		next.Vectors[hash] = v
	}
	for hash, v := range added {
		next.Vectors[hash] = v
	}
	if submission.Scope == "history" {
		if len(e.reservedHashes(next.Vectors))*e.state.Config.Dimensions > maxVectorValues() {
			return IndexStatus{}, errors.New("historical embedding capacity reached; text history remains available")
		}
	} else if len(next.Vectors)*e.state.Config.Dimensions > maxVectorValues() {
		current := map[string]bool{}
		for _, p := range e.passages {
			current[p.Hash] = true
		}
		for hash := range next.Vectors {
			if !current[hash] {
				delete(next.Vectors, hash)
			}
			if len(next.Vectors)*e.state.Config.Dimensions <= maxVectorValues() {
				break
			}
		}
	}
	complete := true
	for _, p := range e.passages {
		if _, ok := next.Vectors[p.Hash]; !ok {
			complete = false
			break
		}
	}
	if complete {
		next.Revision = e.snapshot.Revision
	}
	if err := e.persistVectors(next, submission.Scope == "history"); err != nil {
		return IndexStatus{}, err
	}
	e.cache = next
	e.failure = ""
	// status validates historical coverage against these persisted vectors too;
	// a current submission can recover a missing historical content hash.
	return e.status(), nil
}
func matches(file workspace.SnapshotFile, query SearchQuery) bool {
	if workspace.IsTrash(file.Path) && !query.IncludeTrash {
		return false
	}
	if query.Path != "" && !strings.HasPrefix(file.Path, query.Path) {
		return false
	}
	if query.Literal != "" && !strings.Contains(strings.ToLower(file.Path+"\n"+workspace.SearchableText(file.Content)), strings.ToLower(query.Literal)) {
		return false
	}
	if query.Status != "" && !strings.EqualFold(query.Status, file.Metadata.Status) {
		return false
	}
	if query.Tag != "" {
		found := false
		for _, tag := range file.Metadata.Tags {
			if strings.EqualFold(tag, query.Tag) {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return true
}
func (e *Engine) Search(ctx context.Context, query SearchQuery) ([]Match, error) {
	var err error
	query, err = e.resolveSearch(query)
	if err != nil {
		return nil, err
	}
	if query.Scope == "history" {
		return e.historySemantic(ctx, query)
	}
	if query.Scope != "" && query.Scope != "current" {
		return nil, errors.New("search scope must be current or history")
	}
	return e.search(ctx, query, false)
}

func (e *Engine) search(ctx context.Context, query SearchQuery, distinctNotes bool) ([]Match, error) {
	if len(query.Text) > 2000 || len(query.Literal) > 200 || len(query.Path) > 512 || len(query.Tag) > 200 || len(query.Status) > 200 {
		return nil, errors.New("search input exceeds limits")
	}
	snapshot, err := e.Store.SnapshotScoped(ctx, "", query.contentScope)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	config, provider := e.state.Config, e.provider.Load()
	e.mu.Unlock()
	if !config.Enabled {
		return nil, errors.New("semantic search is disabled")
	}
	if len(query.Vector) == 0 {
		if !config.Endpoint || provider == nil {
			return nil, errors.New("browser semantic search needs a compatible query-embedding endpoint; literal search remains available")
		}
		if strings.TrimSpace(query.Text) == "" {
			return nil, errors.New("enter search text")
		}
		vectors, err := provider.Embed(ctx, config, []string{query.Text})
		if err != nil {
			return nil, err
		}
		query.Vector = vectors[0]
		query.ConfigID = config.ID()
	}
	vector, err := normalize(query.Vector, config.Dimensions)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err = e.refreshStored(ctx); err != nil {
		return nil, err
	}
	if query.ConfigID != e.state.Config.ID() {
		return nil, errors.New("query embedding configuration does not match the project")
	}
	allowed := map[string]bool{}
	for _, f := range snapshot.Files {
		if matches(f, query) {
			allowed[f.Path] = true
		}
	}
	passages, err := Split(snapshot, config)
	if err != nil {
		return nil, err
	}
	result := []Match{}
	for _, p := range passages {
		if !allowed[p.Path] {
			continue
		}
		v, ok := e.cache.Vectors[p.Hash]
		if !ok {
			continue
		}
		score := float64(0)
		for i, x := range vector {
			score += float64(x) * float64(v[i])
		}
		result = append(result, Match{Passage: p, Score: score})
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].Priority != result[j].Priority {
			return result[i].Priority < result[j].Priority
		}
		return result[i].Score > result[j].Score
	})
	if distinctNotes {
		seen := map[string]bool{}
		notes := result[:0]
		for _, match := range result {
			if !seen[match.Path] {
				notes = append(notes, match)
				seen[match.Path] = true
			}
		}
		result = notes
	}
	limit := query.Limit
	if limit < 1 || limit > 50 {
		limit = 20
	}
	if len(result) > limit {
		result = result[:limit]
	}
	if err = e.validateSearchSnapshot(ctx, snapshot, query.contentScope); err != nil {
		return nil, err
	}
	return result, nil
}
