package reposource

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"forgetmenote/internal/capacity"
	"forgetmenote/internal/procgroup"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"forgetmenote/internal/retrieval"
	"forgetmenote/internal/workspace"
)

func maxIndexBytes() int { return capacity.IndexBytes.Limit() }

// Bound serialized excerpts separately from the estimated lexical cache budget.
// Process memory also includes temporary decoding and indexing allocations.
func maxChunkBytes() int  { return maxIndexBytes() }
func maxBlobBytes() int64 { return int64(workspace.MaxFileBytes()) }

// Each serialized chunk necessarily occupies more than one byte. This protects
// corrupt counts without imposing a separate product quota.
func maxChunks() int { return maxIndexBytes() }

type IndexConfig struct {
	Embeddings         bool     `json:"embeddings"`
	ExternalProcessing bool     `json:"externalProcessing"`
	Judgments          bool     `json:"jev"`
	MapJudgments       bool     `json:"mapJev,omitempty"`
	Model              string   `json:"model"`
	Dimensions         int      `json:"dimensions"`
	Include            []string `json:"include,omitempty"`
	Exclude            []string `json:"exclude,omitempty"`
}

func (c IndexConfig) embeddingID() string {
	return hash([]byte(fmt.Sprintf("%s\n%d", retrieval.EmbeddingModelKey(c.Model), c.Dimensions)))
}
func (c IndexConfig) id() string {
	c.Model = retrieval.EmbeddingModelKey(c.Model)
	b, _ := json.Marshal(c)
	return hash(b)
}
func (c IndexConfig) validate() error {
	if (c.Embeddings || c.Judgments || c.MapJudgments) && !c.ExternalProcessing {
		return errors.New("source external-processing permission is required")
	}
	if c.Embeddings && (strings.TrimSpace(c.Model) == "" || len(c.Model) > 200 || c.Dimensions < 1 || c.Dimensions > 4096) {
		return errors.New("select an embedding model and 1–4096 dimensions")
	}
	if len(c.Include)+len(c.Exclude) > 64 {
		return ErrLimit
	}
	for _, p := range append(append([]string{}, c.Include...), c.Exclude...) {
		if !safePath(strings.TrimSuffix(p, "/")) {
			return ErrDenied
		}
		if _, e := path.Match(p, ""); e != nil {
			return ErrDenied
		}
	}
	return nil
}

type Exclusion struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}
type IndexStatus struct {
	// Generation names the published index generation that every gateway
	// serves, fetching it from blob storage when its cache lacks it.
	Generation     string      `json:"generation,omitempty"`
	Commit         string      `json:"commit"`
	Chunks         int         `json:"chunks"`
	Embedded       int         `json:"embedded"`
	Files          int         `json:"files"`
	Exclusions     []Exclusion `json:"exclusions"`
	Bytes          int64       `json:"bytes"`
	Error          string      `json:"error,omitempty"`
	EmbeddingError string      `json:"embeddingError,omitempty"`
	Requests       int64       `json:"requests"`
	InputBytes     int64       `json:"inputBytes"`
	EmbeddedInputs int64       `json:"embeddedInputs"`
	MapEntries     int         `json:"mapEntries"`
	MapClassified  int         `json:"mapClassified"`
	MapError       string      `json:"mapError,omitempty"`
}
type sourceIndex struct {
	NotebookLayout bool          `json:"notebookLayout,omitempty"`
	Version        int           `json:"version"`
	Source         string        `json:"source"`
	Repository     int64         `json:"repository"`
	Commit         string        `json:"commit"`
	Chunker        string        `json:"chunker"`
	ConfigID       string        `json:"configId"`
	EmbeddingID    string        `json:"embeddingId"`
	Files          []Entry       `json:"files"`
	Exclusions     []Exclusion   `json:"exclusions"`
	Count          int           `json:"count"`
	ChunkHash      string        `json:"chunkHash"`
	Chunks         []SourceChunk `json:"-"`
	MapVersion     string        `json:"mapVersion,omitempty"`
	MapHash        string        `json:"mapHash,omitempty"`
	Map            []MapEntry    `json:"-"`
	dir            string
}
type indexPointer struct {
	Generation string `json:"generation"`
}

// readManifest reads the manifest of the generation in dir.
func readManifest(dir string) (sourceIndex, error) {
	var idx sourceIndex
	b, err := regularRead(filepath.Join(dir, "manifest.json"), int64(maxIndexBytes()))
	if err != nil {
		return idx, err
	}
	if json.Unmarshal(b, &idx) != nil || idx.Version != 1 || (idx.Chunker != chunkerID && idx.Chunker != "source-v1") || !revision.MatchString(idx.Commit) || idx.Count < 0 || idx.Count > maxChunks() {
		return sourceIndex{}, ErrUnavailable
	}
	idx.dir = dir
	return idx, nil
}

// loadIndexMetadata reads the manifest of generation name, which the source
// state names. A copy here missing or damaged is fetched again from blob
// storage. A state saved before states named generations names none, and
// then index/current.json points at it.
func (c *cache) loadIndexMetadata(ctx context.Context, name string) (sourceIndex, error) {
	if name == "" {
		var ptr indexPointer
		b, err := regularRead(filepath.Join(c.dir, "index", "current.json"), 4096)
		if err != nil {
			return sourceIndex{}, err
		}
		if json.Unmarshal(b, &ptr) != nil || !identifier.MatchString(ptr.Generation) {
			return sourceIndex{}, ErrUnavailable
		}
		return readManifest(filepath.Join(c.dir, "index", "generations", ptr.Generation))
	}
	if !identifier.MatchString(name) {
		return sourceIndex{}, ErrUnavailable
	}
	dir := filepath.Join(c.dir, "index", "generations", name)
	idx, err := readManifest(dir)
	if err == nil || c.blobs == nil {
		return idx, err
	}
	if err = c.fetchGeneration(ctx, name); err != nil {
		return sourceIndex{}, err
	}
	return readManifest(dir)
}
func (c *cache) loadIndex(ctx context.Context, name string) (sourceIndex, error) {
	idx, err := c.loadIndexMetadata(ctx, name)
	if err != nil {
		return idx, err
	}
	loaded, err := idx.load()
	if err != nil && c.blobs != nil && name != "" {
		// The stored generation replaces a damaged copy here.
		if err = os.RemoveAll(idx.dir); err != nil {
			return sourceIndex{}, err
		}
		if idx, err = c.loadIndexMetadata(ctx, name); err != nil {
			return idx, err
		}
		loaded, err = idx.load()
	}
	return loaded, err
}

// load reads and checks the chunks and map the manifest idx names.
func (idx sourceIndex) load() (sourceIndex, error) {
	dir := idx.dir
	b, err := regularRead(filepath.Join(dir, "chunks.jsonl"), int64(maxChunkBytes()))
	if err != nil || hash(b) != idx.ChunkHash {
		return sourceIndex{}, ErrUnavailable
	}
	scan := bufio.NewScanner(bytes.NewReader(b))
	scan.Buffer(make([]byte, 8192), 32768)
	for scan.Scan() {
		var ch SourceChunk
		if json.Unmarshal(scan.Bytes(), &ch) != nil || !safePath(ch.Path) || !revision.MatchString(ch.BlobID) || ch.StartLine < 1 || ch.EndLine < ch.StartLine || ch.EndLine-ch.StartLine >= 80 || ch.StartByte < 0 || ch.EndByte-ch.StartByte != len(ch.Text) || hash([]byte(ch.Text)) != ch.ContentHash || len(ch.payload()) > 4096 {
			return sourceIndex{}, ErrUnavailable
		}
		idx.Chunks = append(idx.Chunks, ch)
		if len(idx.Chunks) > idx.Count {
			return sourceIndex{}, ErrUnavailable
		}
	}
	if scan.Err() != nil || len(idx.Chunks) != idx.Count {
		return sourceIndex{}, ErrUnavailable
	}
	if err := idx.loadMap(); err != nil {
		return sourceIndex{}, err
	}
	idx.dir = dir
	return idx, nil
}
func pathMatches(pattern, p string) bool {
	if strings.HasSuffix(pattern, "/") {
		return strings.HasPrefix(p, pattern)
	}
	v, _ := path.Match(pattern, p)
	return v || p == pattern
}
func excluded(entry Entry, config IndexConfig) string {
	if !safePath(entry.Path) || !revision.MatchString(entry.BlobID) || (entry.Mode != "100644" && entry.Mode != "100755") {
		return "non-regular file"
	}
	if entry.Bytes > maxBlobBytes() {
		return "text blob exceeds FMN_LIMIT_FILE_BYTES; use range reads"
	}
	for _, v := range config.Exclude {
		if pathMatches(v, entry.Path) {
			return "owner exclusion"
		}
	}
	for _, v := range config.Include {
		if pathMatches(v, entry.Path) {
			return ""
		}
	}
	low := strings.ToLower(entry.Path)
	base := path.Base(low)
	if base == ".env" || strings.HasPrefix(base, ".env.") || strings.HasSuffix(base, ".pem") || strings.HasSuffix(base, ".key") || base == "id_rsa" || base == "id_ed25519" || strings.Contains(base, "credentials") || base == ".npmrc" || base == ".netrc" {
		return "credential filename"
	}
	for _, part := range strings.Split(low, "/") {
		switch part {
		case "node_modules", "vendor", "dist", "build", "__pycache__", ".venv", "venv", "coverage":
			return "dependency or generated directory"
		}
	}
	if strings.HasSuffix(low, ".min.js") || strings.HasSuffix(low, ".min.css") || strings.HasSuffix(low, ".map") {
		return "generated bundle"
	}
	return ""
}
func (c *cache) buildIndex(ctx context.Context, s State, authorize ...func() error) (sourceIndex, error) {
	prior, err := c.loadIndex(ctx, s.Index.Generation)
	if errors.Is(err, errBlobs) {
		// Rebuilding now would pay again for what blob storage holds.
		return sourceIndex{}, err
	}
	if prior.Source == s.ID && prior.Repository == s.Repository.ID && prior.Commit == s.Downloaded && prior.ConfigID == s.indexID() && prior.MapVersion == mapVersion && prior.Chunker == chunkerID {
		if c.blobs != nil {
			// A generation only this directory holds, such as one a state
			// saved before states named generations points at, is stored
			// before the state names it.
			name := filepath.Base(prior.dir)
			keys, err := c.blobs.List(ctx, c.generationKey(name))
			if err != nil {
				return sourceIndex{}, blobFailure(err)
			}
			if !slices.Contains(keys, c.generationKey(name)+"manifest.json") {
				if err = c.storeGeneration(ctx, prior.dir, name); err != nil {
					return sourceIndex{}, err
				}
			}
		}
		return prior, nil
	}
	entries, err := c.tree(ctx, s.Downloaded)
	if err != nil {
		return sourceIndex{}, err
	}
	idx := sourceIndex{NotebookLayout: s.NotebookLayout, Version: 1, Source: s.ID, Repository: s.Repository.ID, Commit: s.Downloaded, Chunker: chunkerID, ConfigID: s.indexID(), EmbeddingID: s.IndexConfig.embeddingID(), Files: entries, Chunks: []SourceChunk{}, Exclusions: []Exclusion{}}
	old := map[string][]SourceChunk{}
	oldEntries := map[string]Entry{}
	oldExcluded := map[string]string{}
	for _, entry := range prior.Files {
		oldEntries[entry.Path] = entry
	}
	for _, entry := range prior.Exclusions {
		oldExcluded[entry.Path] = entry.Reason
	}
	if prior.Source == s.ID && prior.Repository == s.Repository.ID && prior.Chunker == chunkerID && prior.NotebookLayout == s.NotebookLayout {
		for _, ch := range prior.Chunks {
			old[ch.Path] = append(old[ch.Path], ch)
		}
	}
	changed := []Entry{}
	for _, entry := range entries {
		if s.NotebookLayout && !notebookNote(entry.Path) {
			idx.Exclusions = append(idx.Exclusions, Exclusion{entry.Path, "not a notebook note"})
			continue
		}
		if reason := excluded(entry, s.IndexConfig); reason != "" {
			idx.Exclusions = append(idx.Exclusions, Exclusion{entry.Path, reason})
			continue
		}
		if chunks := old[entry.Path]; len(chunks) > 0 && chunks[0].BlobID == entry.BlobID {
			for _, ch := range chunks {
				ch.EmbeddingHash = ch.embeddingKey(s.IndexConfig)
				idx.Chunks = append(idx.Chunks, ch)
			}
		} else if previous, ok := oldEntries[entry.Path]; ok && previous.BlobID == entry.BlobID && (oldExcluded[entry.Path] == "binary or non-UTF-8" || oldExcluded[entry.Path] == "Git LFS pointer" || entry.Bytes == 0) {
			if reason := oldExcluded[entry.Path]; reason != "" {
				idx.Exclusions = append(idx.Exclusions, Exclusion{entry.Path, reason})
			}
		} else {
			changed = append(changed, entry)
		}
	}
	total := 0
	for _, ch := range idx.Chunks {
		total += len(ch.Text) + 768
	}
	err = c.readBlobs(ctx, changed, func(entry Entry, b []byte) error {
		reason := ""
		if !utf8.Valid(b) || bytes.IndexByte(b, 0) >= 0 {
			reason = "binary or non-UTF-8"
		} else if bytes.HasPrefix(b, []byte("version https://git-lfs.github.com/spec/v1")) {
			reason = "Git LFS pointer"
		}
		if reason != "" {
			idx.Exclusions = append(idx.Exclusions, Exclusion{entry.Path, reason})
			return nil
		}
		var metadata workspace.Metadata
		if s.NotebookLayout {
			var e error
			metadata, e = workspace.ParseMetadata(b)
			if e != nil || metadata.ContainerError != "" {
				idx.Exclusions = append(idx.Exclusions, Exclusion{entry.Path, "invalid notebook metadata"})
				return nil
			}
		}
		chunks := chunkFile(entry.Path, entry.BlobID, b, s.IndexConfig)
		if s.NotebookLayout {
			noteID, _ := workspace.NoteID(b)
			title, _ := workspace.NoteSummary(b)
			for i := range chunks {
				chunks[i].Container = metadata.Container
				chunks[i].NoteID = noteID
				chunks[i].Title = title
			}
		}
		for _, ch := range chunks {
			total += len(ch.Text) + 768
		}
		if len(idx.Chunks)+len(chunks) > maxChunks() || total > maxChunkBytes() {
			return fmt.Errorf("%w: index needs %d chunks / %d estimated bytes (FMN_LIMIT_INDEX_BYTES budgets %d / %d)", ErrLimit, len(idx.Chunks)+len(chunks), total, maxChunks(), maxChunkBytes())
		}
		idx.Chunks = append(idx.Chunks, chunks...)
		return nil
	})
	if err != nil {
		return sourceIndex{}, err
	}
	// Byte order makes tie breaks and cache validation independent of Git's walk.
	sortChunks(idx.Chunks)
	buildMap(&idx, prior)
	if err = ctx.Err(); err != nil {
		return sourceIndex{}, err
	}
	for _, check := range authorize {
		if err = check(); err != nil {
			return sourceIndex{}, err
		}
	}
	if err = c.publishIndex(ctx, &idx, prior, nil, s.IndexConfig); err != nil {
		return sourceIndex{}, err
	}
	return idx, nil
}

// One bounded cat-file process reads all changed blobs. Remote code never runs.
func (c *cache) readBlobs(ctx context.Context, entries []Entry, visit func(Entry, []byte) error) error {
	if len(entries) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(c.limits.Seconds)*time.Second)
	defer cancel()
	mirror, err := c.mirror(ctx)
	if err != nil {
		return err
	}
	cmd := c.command(ctx, mirror, "", "cat-file", "--batch")
	var input strings.Builder
	for _, e := range entries {
		input.WriteString(e.BlobID + "\n")
	}
	cmd.Stdin = strings.NewReader(input.String())
	cmd.Stderr = &procgroup.Output{Limit: 65536, Err: ErrLimit}
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err = cmd.Start(); err != nil {
		return ErrUnavailable
	}
	reader := bufio.NewReader(pipe)
	for _, entry := range entries {
		header, e := reader.ReadString('\n')
		if e != nil {
			err = ErrUnavailable
			break
		}
		fields := strings.Fields(header)
		if len(fields) != 3 || fields[0] != entry.BlobID || fields[1] != "blob" {
			err = ErrUnavailable
			break
		}
		size, e := strconv.ParseInt(fields[2], 10, 64)
		if e != nil || size != entry.Bytes || size < 0 || size > maxBlobBytes() {
			err = ErrLimit
			break
		}
		b := make([]byte, int(size)+1)
		if _, err = io.ReadFull(reader, b); err != nil {
			break
		}
		if b[len(b)-1] != '\n' {
			err = ErrUnavailable
			break
		}
		if err = visit(entry, b[:len(b)-1]); err != nil {
			break
		}
	}
	if err != nil {
		cancel()
	}
	waitErr := cmd.Wait()
	if err != nil {
		return err
	}
	if waitErr != nil {
		return ErrUnavailable
	}
	return ctx.Err()
}
func indexStatus(c *cache, idx sourceIndex, old IndexStatus, config IndexConfig) IndexStatus {
	if old.Commit != idx.Commit {
		old.MapError = ""
	}
	old.Commit = idx.Commit
	old.Chunks = len(idx.Chunks)
	old.Files = len(idx.Files)
	old.Exclusions = idx.Exclusions
	old.Error = ""
	old.Embedded = 0
	old.MapEntries, old.MapClassified = len(idx.Map), 0
	for _, entry := range idx.Map {
		if entry.Label != nil {
			old.MapClassified++
		}
	}
	vm, f, e := vectors(idx, config)
	if e == nil {
		f.Close()
		for _, ch := range idx.Chunks {
			if _, ok := vm.Rows[ch.EmbeddingHash]; ok {
				old.Embedded++
			}
		}
	}
	old.Generation = filepath.Base(idx.dir)
	old.Bytes, _ = diskBytes(idx.dir)
	return old
}
func (m *Manager) ConfigureIndex(project, actor string, generation uint64, config IndexConfig) (State, error) {
	if err := m.check(project, actor); err != nil {
		return State{}, ErrDenied
	}
	n, err := m.book(project)
	if err != nil {
		return State{}, err
	}
	if err := n.lock(); err != nil {
		return State{}, err
	}
	defer n.mu.Unlock()
	s := n.state.Sources[n.state.Active]
	if !s.Connected || s.HistoricalOnly || s.Handoff != "" {
		return s, ErrDenied
	}
	if s.Generation != generation {
		return s, ErrConflict
	}
	mapActor := ""
	if config.MapJudgments {
		mapActor = actor
	}
	if m.indexPolicy != nil {
		next := s
		next.IndexConfig, next.MapActor = config, mapActor
		config, mapActor = m.indexPolicy(next)
	}
	if err := config.validate(); err != nil {
		return State{}, err
	}
	if err = m.check(project, actor); err != nil {
		return State{}, ErrDenied
	}
	m.mu.Lock()
	if cancel := m.processingJobs[project]; cancel != nil {
		cancel()
	}
	m.mu.Unlock()
	s.Generation++
	s.IndexConfig = config
	s.MapActor = mapActor
	s.Pending = true
	s.Index.EmbeddingError = ""
	s.Index.MapError = ""
	s.Index.Error = "Source index rebuild pending."
	return s, n.save(s)
}

// SetIndexPolicy is installed before Start. It supplies the current instance
// policy; path filters and pause state remain notebook-owned.
func (m *Manager) SetIndexPolicy(policy func(State) (IndexConfig, string)) { m.indexPolicy = policy }

func (m *Manager) syncIndexPolicy(n *notebook, state *State) error {
	if m.indexPolicy == nil || !state.Connected || state.HistoricalOnly || state.Handoff != "" {
		return nil
	}
	config, actor := m.indexPolicy(*state)
	if config.id() == state.IndexConfig.id() && actor == state.MapActor {
		return nil
	}
	if err := config.validate(); err != nil {
		return err
	}
	m.mu.Lock()
	if cancel := m.processingJobs[state.Project]; cancel != nil {
		cancel()
	}
	m.mu.Unlock()
	state.IndexConfig, state.MapActor = config, actor
	state.Generation++
	state.Pending = true
	state.Index.EmbeddingError, state.Index.MapError = "", ""
	state.Index.Error = "Source index rebuild pending."
	return n.save(*state)
}

// AuthorizeProcessing applies the current index policy to explicit chat reads as
// well as retrieved excerpts. A tool cannot bypass exclusions by guessing a path.
func (m *Manager) AuthorizeProcessing(ctx context.Context, project, source, commit string, paths []string) error {
	return m.authorizeIndexedRead(ctx, project, source, commit, paths, true)
}

// AuthorizeIndexedRead applies exclusions to local package reads without
// treating a deterministic, local read as consent to an external provider.
func (m *Manager) AuthorizeIndexedRead(ctx context.Context, project, source, commit string, paths []string) error {
	return m.authorizeIndexedRead(ctx, project, source, commit, paths, false)
}

func (m *Manager) authorizeIndexedRead(ctx context.Context, project, source, commit string, paths []string, external bool) error {
	return m.read(ctx, project, source, func(c *cache, s State) error {
		if external && !s.IndexConfig.ExternalProcessing || s.Downloaded != commit {
			return ErrDenied
		}
		if len(paths) == 0 {
			return nil
		}
		idx, err := c.loadIndexMetadata(ctx, s.Index.Generation)
		if err != nil || idx.Commit != commit || idx.Source != source || idx.ConfigID != s.indexID() {
			return ErrUnavailable
		}
		allowed := make(map[string]bool, len(idx.Files))
		for _, entry := range idx.Files {
			allowed[entry.Path] = excluded(entry, s.IndexConfig) == ""
		}
		for _, entry := range idx.Exclusions {
			allowed[entry.Path] = false
		}
		for _, path := range paths {
			if !allowed[path] {
				return ErrDenied
			}
		}
		return nil
	})
}
