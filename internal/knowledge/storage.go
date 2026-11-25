package knowledge

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"path/filepath"
	"time"

	"forgetmenote/internal/capacity"
	"forgetmenote/internal/pg"
	"forgetmenote/internal/workspace"

	"github.com/jackc/pgx/v5"
)

// A notebook's knowledge state, retrieval settings and archived proposals are
// versioned documents and its vectors are rows, so every gateway's engine reads
// what any of them wrote. The note map and history cache are derived from the
// owner and stay in the engine's directory on each gateway.
const (
	stateKind     = "knowledge"
	retrievalKind = "knowledge-retrieval"
	archiveKind   = "knowledge-proposal"
)

// ErrUnavailable reports that the database could not be read or written.
var ErrUnavailable = errors.New("knowledge storage is unavailable; retry")

const storageTimeout = 30 * time.Second

// Rewriting a whole vector set can move the full index budget.
const vectorTimeout = 5 * time.Minute

// Every catalog layout keeps a notebook's engine directory at projects/<id> or
// owner-state/<id>, so its base name is the notebook ID.
func notebookKey(store *workspace.Store) string {
	return filepath.Base(filepath.Dir(store.Root))
}

func unavailable(err error) error {
	if err == nil || errors.Is(err, ErrUnavailable) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrUnavailable, err)
}

// refresh adopts the state another engine wrote since this one last read or
// wrote it. A changed configuration resets the derived index as Configure does
// and loads the vectors stored for the new configuration.
func (e *Engine) refresh(ctx context.Context) error {
	// Like the file reads they replace, storage reads finish under their own
	// bound even when the caller has given up.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), storageTimeout)
	defer cancel()
	version, raw, err := e.db.ReadDocument(ctx, e.db.Pool, stateKind, e.notebook, e.stateVersion)
	if err != nil {
		return unavailable(err)
	}
	if version == e.stateVersion {
		return nil
	}
	next, err := decodeState(raw)
	if err != nil {
		return err
	}
	loaded := e.stateVersion >= 0
	changed := next.Config.ID() != e.state.Config.ID()
	purged := next.Purge != e.state.Purge
	e.state, e.stateVersion = next, version
	if loaded && purged {
		e.forgetDerived()
		if err = e.loadVectorCache(ctx); err != nil {
			return err
		}
		e.signal()
		return nil
	}
	if loaded && changed {
		e.resetIndex()
		if err = e.loadVectorCache(ctx); err != nil {
			return err
		}
		e.signal()
	}
	return nil
}

// decodeState reads a knowledge state as the file-based release read
// knowledge.json, so an import accepts exactly what both of them accept.
func decodeState(raw []byte) (state, error) {
	next := state{}
	if len(raw) > capacity.StateBytes.Limit() {
		return next, capacity.StateBytes.Error()
	}
	if err := json.Unmarshal(raw, &next); err != nil {
		return next, errors.New("knowledge state is unreadable; restore it from a full-instance backup")
	}
	if next.Checkpoints == nil {
		next.Checkpoints = map[string]Checkpoint{}
	}
	if next.Proposals == nil {
		next.Proposals = map[string]Proposal{}
	}
	return next, next.Config.Validate()
}

// refreshStored adopts another engine's state, then its vectors.
func (e *Engine) refreshStored(ctx context.Context) error {
	if err := e.refresh(ctx); err != nil {
		return err
	}
	return e.refreshVectors(ctx)
}

// refreshVectors adopts vectors another engine stored since this one last read
// or wrote the set, so it neither asks to embed them again nor rewrites the set
// without them. Rows another engine removed make it read the set again.
func (e *Engine) refreshVectors(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), vectorTimeout)
	defer cancel()
	var version int64
	var configID, revision string
	var dimensions int
	err := e.db.Pool.QueryRow(ctx, `SELECT version, config_id, revision, dimensions FROM knowledge_vector_sets
		WHERE org = $1 AND notebook = $2`, e.db.Org, e.notebook).Scan(&version, &configID, &revision, &dimensions)
	if errors.Is(err, pgx.ErrNoRows) {
		version = 0
	} else if err != nil {
		return fmt.Errorf("%w: %w", ErrVectorCache, unavailable(err))
	}
	if version == e.vectorVersion {
		return nil
	}
	if configID == "" && e.stored != nil && e.cache.ConfigID == e.state.Config.ID() {
		// Another engine rebuilt the index under the same configuration.
		e.resetIndex()
		e.signal()
	}
	if version == 0 || configID != e.state.Config.ID() || dimensions != e.state.Config.Dimensions || e.stored == nil {
		return e.loadVectorCache(ctx)
	}
	rows, err := e.db.Pool.Query(ctx, `SELECT hash, vector FROM knowledge_vectors WHERE org = $1 AND notebook = $2 AND written > $3`, e.db.Org, e.notebook, e.vectorVersion)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrVectorCache, unavailable(err))
	}
	defer rows.Close()
	vectors := maps.Clone(e.cache.Vectors)
	stored := maps.Clone(e.stored)
	var hash string
	var raw []byte
	for rows.Next() {
		if err = rows.Scan(&hash, &raw); err != nil {
			return fmt.Errorf("%w: %w", ErrVectorCache, unavailable(err))
		}
		vector, valid := decodeVector(raw, dimensions)
		if _, err = normalize(vector, dimensions); !valid || err != nil || !validVectorHash(hash) {
			return fmt.Errorf("%w: a stored semantic vector is damaged; restore the database from a backup, or rebuild the index", ErrVectorCache)
		}
		vectors[hash], stored[hash] = vector, struct{}{}
		if len(stored) > maxVectorValues()/dimensions {
			return e.loadVectorCache(ctx)
		}
	}
	if err = rows.Err(); err != nil {
		return fmt.Errorf("%w: %w", ErrVectorCache, unavailable(err))
	}
	var count int
	if err = e.db.Pool.QueryRow(ctx, `SELECT count(*) FROM knowledge_vectors WHERE org = $1 AND notebook = $2`, e.db.Org, e.notebook).Scan(&count); err != nil {
		return fmt.Errorf("%w: %w", ErrVectorCache, unavailable(err))
	}
	if count != len(stored) {
		return e.loadVectorCache(ctx)
	}
	if revision != "" && !historyRevisionValid(revision) {
		revision = ""
	}
	e.cache = vectorCache{ConfigID: configID, Revision: revision, Vectors: vectors}
	e.stored, e.vectorVersion = stored, version
	return nil
}

// resetIndex forgets everything derived under the previous configuration.
func (e *Engine) resetIndex() {
	e.cache = vectorCache{ConfigID: e.state.Config.ID(), Vectors: map[string][]float32{}}
	e.snapshot = workspace.Snapshot{}
	e.snapshotVisibility = ""
	e.passages = nil
	e.historySplits = nil
	e.failure = ""
	e.historyEmbeddingFailure = ""
}

// saveState writes next over the version this engine last read, with an
// archived proposal in the same transaction, then adopts it. Another engine's
// newer state is a conflict, and the next call rereads it. dropVectors removes
// the stored vector set, for a new configuration or rebuild.
func (e *Engine) saveState(ctx context.Context, next state, archived *Proposal, dropVectors bool) error {
	raw, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if len(raw) > capacity.StateBytes.Limit() {
		return capacity.StateBytes.Error()
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), storageTimeout)
	defer cancel()
	tx, err := e.db.Pool.Begin(ctx)
	if err != nil {
		return unavailable(err)
	}
	defer tx.Rollback(ctx)
	if archived != nil {
		body, err := json.Marshal(archived)
		if err != nil {
			return err
		}
		id := e.notebook + "/" + archived.ID
		known, _, err := e.db.LockDocument(ctx, tx, archiveKind, id, -1)
		if err != nil {
			return unavailable(err)
		}
		if _, err = e.db.WriteDocument(ctx, tx, archiveKind, id, known, body); err != nil {
			return storageError(err)
		}
	}
	version, err := e.db.WriteDocument(ctx, tx, stateKind, e.notebook, e.stateVersion, raw)
	if err != nil {
		return storageError(err)
	}
	vectorVersion := e.vectorVersion
	if dropVectors {
		// The set keeps its row and version, so no engine mistakes a later set
		// for the one it read. An empty configuration matches none.
		if _, err = tx.Exec(ctx, `DELETE FROM knowledge_vectors WHERE org = $1 AND notebook = $2`, e.db.Org, e.notebook); err != nil {
			return unavailable(err)
		}
		err = tx.QueryRow(ctx, `UPDATE knowledge_vector_sets SET version = version + 1, config_id = '', revision = ''
			WHERE org = $1 AND notebook = $2 RETURNING version`, e.db.Org, e.notebook).Scan(&vectorVersion)
		if errors.Is(err, pgx.ErrNoRows) {
			vectorVersion, err = 0, nil
		}
		if err != nil {
			return unavailable(err)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return unavailable(err)
	}
	e.state, e.stateVersion = next, version
	if dropVectors {
		e.vectorVersion, e.stored = vectorVersion, nil
	}
	return nil
}

func storageError(err error) error {
	switch {
	case errors.Is(err, pg.ErrConflict):
		return fmt.Errorf("knowledge state changed on another gateway; retry: %w", workspace.ErrConflict)
	case errors.Is(err, pg.ErrNUL):
		return err
	}
	return unavailable(err)
}

// archivedProposal reads a completed proposal kept outside the active state.
func (e *Engine) archivedProposal(ctx context.Context, id string) (Proposal, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, storageTimeout)
	defer cancel()
	version, raw, err := e.db.ReadDocument(ctx, e.db.Pool, archiveKind, e.notebook+"/"+id, -1)
	if err != nil {
		return Proposal{}, false, unavailable(err)
	}
	if version == 0 {
		return Proposal{}, false, nil
	}
	p, err := decodeProposal(raw, id)
	return p, err == nil, err
}

func decodeProposal(raw []byte, id string) (Proposal, error) {
	var p Proposal
	if err := json.Unmarshal(raw, &p); err != nil {
		return p, err
	}
	if p.ID != id {
		return p, errors.New("invalid archived proposal")
	}
	return p, nil
}

// refreshRetrieval adopts retrieval settings another engine wrote. Settings
// decide what search and AI may disclose, so a failed read refuses the caller.
func (e *Engine) refreshRetrieval(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), storageTimeout)
	defer cancel()
	version, raw, err := e.db.ReadDocument(ctx, e.db.Pool, retrievalKind, e.notebook, e.retrievalVersion)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrRetrievalSettings, unavailable(err))
	}
	if version == e.retrievalVersion {
		return nil
	}
	settings := DefaultRetrievalSettings()
	if version != 0 {
		if settings, err = decodeRetrieval(raw); err != nil {
			return err
		}
	}
	e.retrievalSettings, e.retrievalVersion = settings, version
	return nil
}

// decodeRetrieval reads complete version 1 settings and nothing else, as the
// file-based release read retrieval.json.
func decodeRetrieval(raw []byte) (RetrievalSettings, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var settings RetrievalSettings
	if decoder.Decode(&settings) != nil || decoder.Decode(new(any)) != io.EOF || settings.Validate() != nil {
		return settings, fmt.Errorf("%w: invalid; restore complete version 1 retrieval settings", ErrRetrievalSettings)
	}
	return settings, nil
}

func (e *Engine) saveRetrieval(settings RetrievalSettings) error {
	raw, err := json.Marshal(settings)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), storageTimeout)
	defer cancel()
	version, err := e.db.WriteDocument(ctx, e.db.Pool, retrievalKind, e.notebook, e.retrievalVersion, raw)
	if err != nil {
		return storageError(err)
	}
	e.retrievalSettings, e.retrievalVersion = settings, version
	return nil
}

// loadVectorCache reads the vectors stored for the active configuration, bit for
// bit. A row that no longer validates fails closed for this notebook, as a
// damaged cache file did, rather than silently paying to embed it again.
func (e *Engine) loadVectorCache(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, vectorTimeout)
	defer cancel()
	var version int64
	var configID, revision string
	var dimensions int
	err := e.db.Pool.QueryRow(ctx, `SELECT version, config_id, revision, dimensions FROM knowledge_vector_sets
		WHERE org = $1 AND notebook = $2`, e.db.Org, e.notebook).Scan(&version, &configID, &revision, &dimensions)
	if errors.Is(err, pgx.ErrNoRows) {
		e.vectorVersion, e.stored = 0, nil
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: %w", ErrVectorCache, unavailable(err))
	}
	e.vectorVersion, e.stored = version, nil
	if configID != e.state.Config.ID() {
		return nil
	}
	if dimensions != e.state.Config.Dimensions {
		return fmt.Errorf("%w: semantic vector dimensions disagree with the active configuration; rebuild the index", ErrVectorCache)
	}
	rows, err := e.db.Pool.Query(ctx, `SELECT hash, vector FROM knowledge_vectors WHERE org = $1 AND notebook = $2`, e.db.Org, e.notebook)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrVectorCache, unavailable(err))
	}
	defer rows.Close()
	cache := vectorCache{ConfigID: configID, Revision: revision, Vectors: map[string][]float32{}}
	if cache.Revision != "" && !historyRevisionValid(cache.Revision) {
		// Revision metadata is advisory; the next owner snapshot rebinds it.
		cache.Revision = ""
	}
	stored := map[string]struct{}{}
	var hash string
	var raw []byte
	for rows.Next() {
		if err = rows.Scan(&hash, &raw); err != nil {
			return fmt.Errorf("%w: %w", ErrVectorCache, unavailable(err))
		}
		if len(stored) >= maxVectorValues()/dimensions {
			return fmt.Errorf("%w: %w", ErrVectorCache, capacity.IndexBytes.Error())
		}
		vector, valid := decodeVector(raw, dimensions)
		if _, err = normalize(vector, dimensions); !valid || err != nil || !validVectorHash(hash) {
			return fmt.Errorf("%w: a stored semantic vector is damaged; restore the database from a backup, or rebuild the index", ErrVectorCache)
		}
		stored[hash] = struct{}{}
		cache.Vectors[hash] = vector
	}
	if err = rows.Err(); err != nil {
		return fmt.Errorf("%w: %w", ErrVectorCache, unavailable(err))
	}
	e.cache, e.stored, e.cacheDirty = cache, stored, false
	return nil
}

func encodeVector(vector []float32) []byte {
	raw := make([]byte, len(vector)*4)
	for i, value := range vector {
		binary.LittleEndian.PutUint32(raw[i*4:], math.Float32bits(value))
	}
	return raw
}

func decodeVector(raw []byte, dimensions int) ([]float32, bool) {
	if len(raw) != dimensions*4 {
		return nil, false
	}
	vector := make([]float32, dimensions)
	for i := range vector {
		vector[i] = math.Float32frombits(binary.LittleEndian.Uint32(raw[i*4:]))
	}
	return vector, true
}

// writeVectors publishes next as the notebook's vector set. When the stored set
// is the one this engine last read or wrote, only the rows that changed are
// added and removed; otherwise the set is rewritten whole.
func (e *Engine) writeVectors(next vectorCache) error {
	dimensions := e.state.Config.Dimensions
	header := vectorDiskHeader{Version: vectorDiskVersion, ConfigID: next.ConfigID, Revision: next.Revision, Dimensions: dimensions, Count: len(next.Vectors)}
	if err := header.validate(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), vectorTimeout)
	defer cancel()
	tx, err := e.db.Pool.Begin(ctx)
	if err != nil {
		return unavailable(err)
	}
	defer tx.Rollback(ctx)
	var prior int64
	var priorConfig string
	var priorDimensions int
	err = tx.QueryRow(ctx, `SELECT version, config_id, dimensions FROM knowledge_vector_sets
		WHERE org = $1 AND notebook = $2 FOR UPDATE`, e.db.Org, e.notebook).Scan(&prior, &priorConfig, &priorDimensions)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return unavailable(err)
	}
	incremental := err == nil && prior == e.vectorVersion && e.stored != nil && !e.cacheDirty && priorConfig == next.ConfigID && priorDimensions == dimensions
	var version int64
	if err = tx.QueryRow(ctx, `INSERT INTO knowledge_vector_sets (org, notebook, version, config_id, revision, dimensions)
		VALUES ($1, $2, 1, $3, $4, $5)
		ON CONFLICT (org, notebook) DO UPDATE SET version = knowledge_vector_sets.version + 1,
			config_id = EXCLUDED.config_id, revision = EXCLUDED.revision, dimensions = EXCLUDED.dimensions
		RETURNING version`, e.db.Org, e.notebook, next.ConfigID, next.Revision, dimensions).Scan(&version); err != nil {
		return unavailable(err)
	}
	added := []string{}
	if incremental {
		removed := []string{}
		for hash := range e.stored {
			if _, ok := next.Vectors[hash]; !ok {
				removed = append(removed, hash)
			}
		}
		if len(removed) > 0 {
			if _, err = tx.Exec(ctx, `DELETE FROM knowledge_vectors WHERE org = $1 AND notebook = $2 AND hash = ANY($3)`, e.db.Org, e.notebook, removed); err != nil {
				return unavailable(err)
			}
		}
		for hash := range next.Vectors {
			if _, ok := e.stored[hash]; !ok {
				added = append(added, hash)
			}
		}
	} else {
		if _, err = tx.Exec(ctx, `DELETE FROM knowledge_vectors WHERE org = $1 AND notebook = $2`, e.db.Org, e.notebook); err != nil {
			return unavailable(err)
		}
		for hash := range next.Vectors {
			added = append(added, hash)
		}
	}
	for _, hash := range added {
		if !validVectorHash(hash) {
			return errors.New("invalid semantic vector key")
		}
		if _, err = normalize(next.Vectors[hash], dimensions); err != nil {
			return err
		}
	}
	if len(added) > 0 {
		if _, err = tx.CopyFrom(ctx, pgx.Identifier{"knowledge_vectors"}, []string{"org", "notebook", "hash", "written", "vector"}, pgx.CopyFromSlice(len(added), func(i int) ([]any, error) {
			return []any{e.db.Org, e.notebook, added[i], version, encodeVector(next.Vectors[added[i]])}, nil
		})); err != nil {
			return unavailable(err)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return unavailable(err)
	}
	stored := make(map[string]struct{}, len(next.Vectors))
	for hash := range next.Vectors {
		stored[hash] = struct{}{}
	}
	e.vectorVersion, e.stored = version, stored
	return nil
}

// vectorsUnchanged reports whether the stored set is still the one this engine
// last read or wrote.
func (e *Engine) vectorsUnchanged(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, storageTimeout)
	defer cancel()
	var version int64
	err := e.db.Pool.QueryRow(ctx, `SELECT version FROM knowledge_vector_sets WHERE org = $1 AND notebook = $2`, e.db.Org, e.notebook).Scan(&version)
	return err == nil && version == e.vectorVersion && e.stored != nil
}

// storedVersions identifies everything an idle engine would load from the
// database: its state, retrieval settings and vector set.
func storedVersions(ctx context.Context, db *pg.DB, notebook string) ([3]int64, error) {
	ctx, cancel := context.WithTimeout(ctx, storageTimeout)
	defer cancel()
	var versions [3]int64
	err := db.Pool.QueryRow(ctx, `SELECT
		coalesce((SELECT version FROM documents WHERE org = $1 AND kind = $3 AND id = $2), 0),
		coalesce((SELECT version FROM documents WHERE org = $1 AND kind = $4 AND id = $2), 0),
		coalesce((SELECT version FROM knowledge_vector_sets WHERE org = $1 AND notebook = $2), 0)`,
		db.Org, notebook, stateKind, retrievalKind).Scan(&versions[0], &versions[1], &versions[2])
	return versions, err
}
