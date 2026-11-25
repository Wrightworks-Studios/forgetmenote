package knowledge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"path"
	"slices"
	"strings"

	"forgetmenote/internal/capacity"
	"forgetmenote/internal/filestate"
	"forgetmenote/internal/pg"
	"forgetmenote/internal/workspace"

	"github.com/jackc/pgx/v5"
)

// The file-based release kept each notebook's knowledge in its engine
// directory: knowledge.json, retrieval.json, proposal-archive/<id>.json and
// cache/semantic.bin, or the older cache/semantic.json it converts when no
// semantic.bin exists. The note map (note-map.json) and history catalog
// (cache/history.json) there are derived: they stay behind, and each gateway
// rebuilds its own.
const (
	stateFile        = "knowledge.json"
	retrievalFile    = "retrieval.json"
	archiveDir       = "proposal-archive"
	vectorFile       = "cache/semantic.bin"
	legacyVectorFile = "cache/semantic.json"
	// That release read no more of retrieval.json than this.
	retrievalFileLimit = 16384
)

// engineDirs maps each notebook of the catalog document to the engine
// directory the file-based release opens for it: owner-state/<id> for one the
// catalog records on an owner, projects/<id> otherwise. Knowledge rows are
// keyed by notebook alone, so both directions take the placement from the
// catalog; an import reads it from its own transaction, after the catalog
// area wrote it. A missing catalog document returns nil.
func engineDirs(ctx context.Context, db *pg.DB, q pg.Querier) (map[string]string, error) {
	version, raw, err := db.ReadDocument(ctx, q, "catalog", "state", -1)
	if err != nil || version == 0 {
		return nil, err
	}
	var envelope struct {
		Data struct {
			Projects       map[string]json.RawMessage `json:"projects"`
			RemoteProjects map[string]string          `json:"remoteProjects"`
		} `json:"data"`
	}
	if err = json.Unmarshal(raw, &envelope); err != nil {
		return nil, err
	}
	dirs := map[string]string{}
	for id := range envelope.Data.Projects {
		if !workspace.ValidNotebookID(id) {
			return nil, fmt.Errorf("invalid notebook ID %q in the catalog", id)
		}
		dirs[id] = path.Join("projects", id)
		if _, remote := envelope.Data.RemoteProjects[id]; remote {
			dirs[id] = path.Join("owner-state", id)
		}
	}
	return dirs, nil
}

// ImportFiles copies each catalog notebook's knowledge files into its
// documents and vector rows, checking them as the file-based release does when
// it opens the notebook's engine, and converting semantic.json as it does. It
// refuses what that release refuses or never reads, rather than drop it. It
// runs after the catalog area, whose document places each notebook.
func ImportFiles(ctx context.Context, db *pg.DB, tx pgx.Tx, src *filestate.Source) (filestate.Sum, error) {
	var tally filestate.Tally
	dirs, err := engineDirs(ctx, db, tx)
	if err != nil {
		return filestate.Sum{}, err
	}
	local, err := src.Entries("projects")
	if err != nil {
		return filestate.Sum{}, err
	}
	if dirs == nil {
		remote, err := src.Entries("owner-state")
		if err != nil || len(local)+len(remote) > 0 {
			return filestate.Sum{}, errors.Join(errors.New("notebook directories exist without a catalog; import the catalog first"), err)
		}
		return tally.Sum(), nil
	}
	for _, id := range slices.Sorted(maps.Keys(dirs)) {
		if strings.HasPrefix(dirs[id], "owner-state/") && slices.ContainsFunc(local, func(e fs.DirEntry) bool { return e.Name() == id }) {
			return filestate.Sum{}, fmt.Errorf("notebook %s is recorded on an owner but still has local project data, which the file-based release refuses to open", id)
		}
		if err = importNotebook(ctx, db, tx, src, id, dirs[id], &tally); err != nil {
			return filestate.Sum{}, fmt.Errorf("notebook %s: %w", id, err)
		}
	}
	return tally.Sum(), nil
}

func importNotebook(ctx context.Context, db *pg.DB, tx pgx.Tx, src *filestate.Source, id, dir string, tally *filestate.Tally) error {
	var config Config
	rel := path.Join(dir, stateFile)
	raw, err := src.Read(rel)
	if err == nil {
		var next state
		if next, err = decodeState(raw); err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}
		config = next.Config
		err = importDocument(ctx, db, tx, stateKind, id, rel, next, tally)
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	rel = path.Join(dir, retrievalFile)
	if raw, err = src.Read(rel); err == nil {
		var settings RetrievalSettings
		if len(raw) > retrievalFileLimit {
			return fmt.Errorf("%s: %w: exceeds 16 KiB", rel, ErrRetrievalSettings)
		}
		if settings, err = decodeRetrieval(raw); err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}
		err = importDocument(ctx, db, tx, retrievalKind, id, rel, settings, tally)
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	archive := path.Join(dir, archiveDir)
	entries, err := src.Entries(archive)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		rel = path.Join(archive, entry.Name())
		// The file-based release looks up only archived IDs that are content
		// hashes, so any other entry is nothing it would read.
		proposal, ok := strings.CutSuffix(entry.Name(), ".json")
		if !ok || !validVectorHash(proposal) {
			return fmt.Errorf("%s is not an archived proposal the file-based release reads; remove it", rel)
		}
		if raw, err = src.Read(rel); err != nil {
			return err
		}
		p, err := decodeProposal(raw, proposal)
		if err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}
		if err = importDocument(ctx, db, tx, archiveKind, id+"/"+proposal, rel, p, tally); err != nil {
			return err
		}
	}
	return importVectors(ctx, db, tx, src, id, dir, config, tally)
}

// importDocument stores value as the store writes it, and tallies it in the
// canonical form an export of the document tallies.
func importDocument(ctx context.Context, db *pg.DB, tx pgx.Tx, kind, id, rel string, value any, tally *filestate.Tally) error {
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if kind == stateKind && len(body) > capacity.StateBytes.Limit() {
		return fmt.Errorf("%s: %w", rel, capacity.StateBytes.Error())
	}
	if _, err = db.WriteDocument(ctx, tx, kind, id, 0, body); err != nil {
		return fmt.Errorf("%s: %w", rel, err)
	}
	return tallyJSON(tally, rel, body)
}

// importVectors stores the notebook's semantic.bin bit for bit as the store's
// first write would: one set row at version 1 and one row per vector written
// in it. Without semantic.bin the file-based release converts semantic.json
// and writes semantic.bin, so the import stores and tallies the set it would
// have written. While semantic.bin exists that release never reads
// semantic.json, which its next vector write removes, and neither does this.
func importVectors(ctx context.Context, db *pg.DB, tx pgx.Tx, src *filestate.Source, id, dir string, config Config, tally *filestate.Tally) error {
	rel := path.Join(dir, vectorFile)
	raw, err := src.Read(rel)
	var cache vectorCache
	var dimensions int
	switch {
	case err == nil:
		if len(raw) > capacity.IndexBytes.Limit() {
			return fmt.Errorf("%s: %w", rel, capacity.IndexBytes.Error())
		}
		cache, dimensions, err = decodeVectorFile(bytes.NewReader(raw), int64(len(raw)))
		if err == nil && cache.ConfigID == config.ID() && dimensions != config.Dimensions {
			err = errors.New("semantic vector dimensions disagree with the active configuration")
		}
		if err != nil {
			return fmt.Errorf("%s: %w; the file-based release refuses it too: restore it from a full-instance backup, or remove it to embed again", rel, err)
		}
	case errors.Is(err, fs.ErrNotExist):
		legacy := path.Join(dir, legacyVectorFile)
		if raw, err = src.Read(legacy); errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if cache, err = convertLegacyVectors(raw, config); err != nil {
			return fmt.Errorf("%s: %w", legacy, err)
		}
		dimensions = config.Dimensions
	default:
		return err
	}
	// The canonical form is the SHA-256 of the semantic.bin the file-based
	// release writes for this set, which encodes equal sets to equal bytes.
	digest := sha256.New()
	if err = encodeVectorFile(digest, cache, dimensions); err != nil {
		return fmt.Errorf("%s: %w", rel, err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO knowledge_vector_sets (org, notebook, version, config_id, revision, dimensions)
		VALUES ($1, $2, 1, $3, $4, $5)`, db.Org, id, cache.ConfigID, cache.Revision, dimensions); err != nil {
		return err
	}
	hashes := slices.Sorted(maps.Keys(cache.Vectors))
	if _, err = tx.CopyFrom(ctx, pgx.Identifier{"knowledge_vectors"}, []string{"org", "notebook", "hash", "written", "vector"}, pgx.CopyFromSlice(len(hashes), func(i int) ([]any, error) {
		return []any{db.Org, id, hashes[i], int64(1), encodeVector(cache.Vectors[hashes[i]])}, nil
	})); err != nil {
		return err
	}
	tally.Add(rel, digest.Sum(nil))
	return nil
}

// convertLegacyVectors converts semantic.json as the file-based release does
// when it opens a notebook without semantic.bin: only under the active
// configuration, normalizing each vector, dropping keys and vectors that do not
// validate and forgetting a damaged revision. That release ignores the file
// otherwise and removes it at its next vector write; the import refuses it, so
// the operator removes it rather than the import dropping it unseen.
func convertLegacyVectors(raw []byte, config Config) (vectorCache, error) {
	var legacy vectorCache
	if len(raw) > capacity.IndexBytes.Limit() || json.Unmarshal(raw, &legacy) != nil {
		return vectorCache{}, errors.New("the legacy semantic vectors are unreadable, so the file-based release ignores them and embeds again; remove the file")
	}
	if legacy.ConfigID != config.ID() || config.Dimensions < 1 || config.Dimensions > 4096 {
		return vectorCache{}, errors.New("the legacy semantic vectors belong to another configuration, so the file-based release never reads them; remove the file")
	}
	cache := vectorCache{ConfigID: legacy.ConfigID, Revision: legacy.Revision, Vectors: map[string][]float32{}}
	if !historyRevisionValid(cache.Revision) {
		cache.Revision = ""
	}
	for hash, vector := range legacy.Vectors {
		if normalized, err := normalize(vector, config.Dimensions); err == nil && validVectorHash(hash) {
			cache.Vectors[hash] = normalized
		}
	}
	return cache, nil
}

// ExportFiles writes each notebook's knowledge documents and vector set into
// the engine directory the file-based release opens for it, in its current
// formats. Document and set versions and written markers have no file form and
// are dropped.
func ExportFiles(ctx context.Context, db *pg.DB, q pg.Querier, out *filestate.Target) (filestate.Sum, error) {
	var tally filestate.Tally
	dirs, err := engineDirs(ctx, db, q)
	if err != nil {
		return filestate.Sum{}, err
	}
	engineDir := func(id string) (string, error) {
		if dir, ok := dirs[id]; ok {
			return dir, nil
		}
		// Rows of a notebook the catalog does not hold go where that release
		// keeps a local notebook's, so nothing stored is left behind.
		if !workspace.ValidNotebookID(id) {
			return "", fmt.Errorf("invalid knowledge notebook %q", id)
		}
		return path.Join("projects", id), nil
	}
	for _, kind := range []string{stateKind, retrievalKind, archiveKind} {
		ids, err := db.DocumentIDs(ctx, q, kind)
		if err != nil {
			return filestate.Sum{}, err
		}
		for _, id := range ids {
			_, body, err := db.ReadDocument(ctx, q, kind, id, -1)
			if err != nil {
				return filestate.Sum{}, err
			}
			notebook, proposal := id, ""
			if kind == archiveKind {
				notebook, proposal, _ = strings.Cut(id, "/")
			}
			dir, err := engineDir(notebook)
			if err != nil {
				return filestate.Sum{}, err
			}
			var rel string
			var value any
			switch kind {
			case stateKind:
				rel = path.Join(dir, stateFile)
				value, err = decodeState(body)
			case retrievalKind:
				rel = path.Join(dir, retrievalFile)
				value, err = decodeRetrieval(body)
			default:
				rel = path.Join(dir, archiveDir, proposal+".json")
				if !validVectorHash(proposal) {
					return filestate.Sum{}, fmt.Errorf("%s: invalid archived proposal ID", rel)
				}
				value, err = decodeProposal(body, proposal)
			}
			if err != nil {
				return filestate.Sum{}, fmt.Errorf("%s: %w", rel, err)
			}
			raw, err := json.Marshal(value)
			if err != nil {
				return filestate.Sum{}, err
			}
			if kind == retrievalKind && len(raw) > retrievalFileLimit {
				return filestate.Sum{}, fmt.Errorf("%s: %w: exceeds 16 KiB", rel, ErrRetrievalSettings)
			}
			if err = out.Write(rel, raw); err != nil {
				return filestate.Sum{}, err
			}
			if err = tallyJSON(&tally, rel, raw); err != nil {
				return filestate.Sum{}, err
			}
		}
	}
	if err = exportVectors(ctx, db, q, out, engineDir, &tally); err != nil {
		return filestate.Sum{}, err
	}
	return tally.Sum(), nil
}

// exportVectors writes each vector set as the semantic.bin the file-based
// release would have written for it, vectors bit for bit.
func exportVectors(ctx context.Context, db *pg.DB, q pg.Querier, out *filestate.Target, engineDir func(string) (string, error), tally *filestate.Tally) error {
	type vectorSet struct {
		notebook, configID, revision string
		dimensions                   int
	}
	rows, err := q.Query(ctx, `SELECT notebook, config_id, revision, dimensions FROM knowledge_vector_sets WHERE org = $1 ORDER BY notebook`, db.Org)
	if err != nil {
		return err
	}
	sets, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (vectorSet, error) {
		var set vectorSet
		err := row.Scan(&set.notebook, &set.configID, &set.revision, &set.dimensions)
		return set, err
	})
	if err != nil {
		return err
	}
	for _, set := range sets {
		dir, err := engineDir(set.notebook)
		if err != nil {
			return err
		}
		rel := path.Join(dir, vectorFile)
		if set.configID == "" {
			// The store's mark of a set that a new configuration or rebuild
			// dropped, where the file-based release removed semantic.bin.
			continue
		}
		cache := vectorCache{ConfigID: set.configID, Revision: set.revision, Vectors: map[string][]float32{}}
		if !historyRevisionValid(cache.Revision) {
			// Revision metadata is advisory, and the store reads a damaged one
			// as none.
			cache.Revision = ""
		}
		vectors, err := q.Query(ctx, `SELECT hash, vector FROM knowledge_vectors WHERE org = $1 AND notebook = $2`, db.Org, set.notebook)
		if err != nil {
			return err
		}
		var hash string
		var raw []byte
		_, err = pgx.ForEachRow(vectors, []any{&hash, &raw}, func() error {
			vector, valid := decodeVector(raw, set.dimensions)
			if !valid || !validVectorHash(hash) {
				return fmt.Errorf("%s: a stored semantic vector is damaged", rel)
			}
			cache.Vectors[hash] = vector
			return nil
		})
		if err != nil {
			return err
		}
		var file bytes.Buffer
		if err = encodeVectorFile(&file, cache, set.dimensions); err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}
		if err = out.Write(rel, file.Bytes()); err != nil {
			return err
		}
		digest := sha256.Sum256(file.Bytes())
		tally.Add(rel, digest[:])
	}
	return nil
}

func tallyJSON(tally *filestate.Tally, rel string, raw []byte) error {
	canonical, err := filestate.JSON(raw)
	if err == nil {
		tally.Add(rel, canonical)
	}
	return err
}
