package reposource

// Each source's directory, repository-sources/<notebook>/<source>/ under the
// data directory, holds two kinds of files.
//
// Durable files are paid for or authoritative. They live in the instance's
// blob storage under their path relative to the data directory, and a
// gateway's directory holds copies of those it has used:
//
//   - index/generations/<generation>/manifest.json, chunks.jsonl, map.json,
//     vectors.json and vectors.f32: one published index generation. Its
//     vectors are paid embeddings and its map labels paid classifications;
//     the manifest's hashes bind them to the chunks and map they describe, so
//     a generation is stored and served whole rather than derived again by
//     each gateway.
//   - evidence/refs/<id>.json, evidence/blobs/<sha256> and
//     evidence/notebooks/<commit>.json: retained citation evidence and the
//     notebook snapshots an authority handoff pinned.
//
// Everything else is a cache a gateway rebuilds without a paid call: the Git
// mirror source.git, with source.old, fetch-* and askpass from fetching into
// it; index/current.json, which points the directory at the generation the
// source state names; staging directories and copies of superseded
// generations; and, in memory, the lexical index and a listing of the stored
// evidence.
//
// The gateway holding the refresh lease publishes a generation's files before
// the source state names it, uploading its manifest last, and retires the
// generation it replaced once the state names the new one. A gateway serves
// the generation the state names, fetching it whole, checked against its
// manifest, when its directory lacks it. When blob storage is the data
// directory itself, a single host's layout, the directory's files are the
// objects and nothing is copied.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"forgetmenote/internal/blob"
)

// errBlobs is a failure to reach blob storage, unlike a missing or damaged
// file: work that would otherwise rebuild, and so pay again, stops instead.
var errBlobs = errors.New("repository source blob storage is unavailable")

func blobFailure(err error) error { return errors.Join(errBlobs, err) }

// holds reports whether store keeps each durable file at its path under root,
// as the data directory's own store does.
func holds(store blob.Store, root string) bool {
	if store == nil {
		return true
	}
	dir, ok := store.(*blob.Dir)
	if !ok {
		return false
	}
	a, err := os.Stat(filepath.Join(dir.Path(), sourcesDir))
	if err != nil {
		return false
	}
	b, err := os.Stat(root)
	return err == nil && os.SameFile(a, b)
}

// The files of a generation, in the order a publication uploads them: its
// manifest last, so a complete manifest names only stored files.
var generationFiles = []string{"chunks.jsonl", "map.json", "vectors.f32", "vectors.json", "manifest.json"}

// newGeneration names a generation by its creation time, so retirement can
// tell one another gateway may still be publishing from an abandoned one.
func newGeneration(base string) (string, error) {
	return os.MkdirTemp(base, "gen-"+strconv.FormatInt(time.Now().Unix(), 36)+"-")
}

// recent reports whether generation name was created within the last hour,
// longer than any publication takes.
func recent(name string) bool {
	parts := strings.Split(name, "-")
	if len(parts) != 3 || parts[0] != "gen" {
		return false
	}
	created, err := strconv.ParseInt(parts[1], 36, 64)
	return err == nil && time.Since(time.Unix(created, 0)) < time.Hour
}

func (c *cache) generationKey(name string) string {
	return c.key + "/index/generations/" + name + "/"
}

// storeGeneration uploads the staged generation name.
func (c *cache) storeGeneration(ctx context.Context, stage, name string) error {
	for _, file := range generationFiles {
		f, err := os.Open(filepath.Join(stage, file))
		if errors.Is(err, fs.ErrNotExist) && file == "map.json" {
			continue
		}
		if err != nil {
			return err
		}
		err = c.blobs.Put(ctx, c.generationKey(name)+file, f)
		f.Close()
		if err != nil {
			return blobFailure(err)
		}
	}
	return nil
}

func (c *cache) deleteGeneration(ctx context.Context, name string) {
	for _, file := range generationFiles {
		_ = c.blobs.Delete(ctx, c.generationKey(name)+file)
	}
}

// fetchGeneration copies the stored generation name into this directory. It
// checks the copy as a load does, against the manifest's chunk and map hashes
// and the vectors' own hash and size, before the copy can be served.
func (c *cache) fetchGeneration(ctx context.Context, name string) error {
	base := filepath.Join(c.dir, "index", "generations")
	if err := os.MkdirAll(base, 0700); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(base, "fetch-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	var total int64
	copyFile := func(file string) error {
		r, err := c.blobs.Get(ctx, c.generationKey(name)+file)
		if errors.Is(err, fs.ErrNotExist) {
			return errors.Join(ErrUnavailable, err)
		}
		if err != nil {
			return blobFailure(err)
		}
		defer r.Close()
		f, err := os.OpenFile(filepath.Join(stage, file), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		n, err := io.Copy(f, io.LimitReader(r, int64(maxIndexBytes())-total+1))
		if err != nil {
			err = blobFailure(err)
		} else {
			err = f.Sync()
		}
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
		if total += n; err == nil && total > int64(maxIndexBytes()) {
			err = ErrLimit
		}
		return err
	}
	if err = copyFile("manifest.json"); err != nil {
		return err
	}
	idx, err := readManifest(stage)
	if err != nil {
		return err
	}
	for _, file := range generationFiles[:4] {
		if file == "map.json" && idx.MapVersion == "" {
			continue
		}
		if err = copyFile(file); err != nil {
			return err
		}
	}
	if _, err = idx.load(); err != nil {
		return err
	}
	if _, f, err := readVectors(stage); err != nil {
		return err
	} else {
		f.Close()
	}
	if err = syncDir(stage); err != nil {
		return err
	}
	// What a damaged copy left goes; the checked copy replaces it.
	if err = os.RemoveAll(filepath.Join(base, name)); err != nil {
		return err
	}
	if err = os.Rename(stage, filepath.Join(base, name)); err != nil {
		return err
	}
	if err = syncDir(base); err != nil {
		return err
	}
	c.point(name)
	// This gateway's directory keeps only the generation the source names.
	entries, _ := os.ReadDir(base)
	for _, entry := range entries {
		if entry.Name() != name {
			_ = os.RemoveAll(filepath.Join(base, entry.Name()))
		}
	}
	return nil
}

func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// point records in index/current.json the generation the source state names,
// where the file-based release looks for it. This release reads it only for a
// state saved before states named their generation.
func (c *cache) point(name string) {
	if name == "" {
		return
	}
	raw, _ := json.Marshal(indexPointer{name})
	file := filepath.Join(c.dir, "index", "current.json")
	if old, err := regularRead(file, 4096); err == nil && bytes.Equal(old, raw) {
		return
	}
	_ = atomicFile(file, raw)
}

// retire runs once the source state names generation current. It removes the
// generation the state named before, replaced, and any other that a
// publication abandoned long enough ago that no gateway can still be writing
// it. Removal is best effort: what remains goes at a later retirement.
func (c *cache) retire(ctx context.Context, current, replaced string) {
	superseded := func(name string) bool { return name != current && (name == replaced || !recent(name)) }
	if c.blobs != nil {
		prefix := c.key + "/index/generations/"
		if keys, err := c.blobs.List(ctx, prefix); err == nil {
			for _, key := range keys {
				if name, _, _ := strings.Cut(strings.TrimPrefix(key, prefix), "/"); superseded(name) {
					_ = c.blobs.Delete(ctx, key)
				}
			}
		}
	}
	base := filepath.Join(c.dir, "index", "generations")
	entries, _ := os.ReadDir(base)
	for _, entry := range entries {
		// With blob storage elsewhere, this directory is this gateway's own
		// cache and keeps only the current generation.
		if entry.Name() != current && (c.blobs != nil || superseded(entry.Name())) {
			_ = os.RemoveAll(filepath.Join(base, entry.Name()))
		}
	}
}

// discard removes generation name, published but left unnamed because
// another gateway's write refused the save that would have named it.
func (c *cache) discard(ctx context.Context, name string) {
	if c.blobs != nil {
		c.deleteGeneration(ctx, name)
	}
	_ = os.RemoveAll(filepath.Join(c.dir, "index", "generations", name))
}

// saveIndexed saves s, whose index names a generation c has published, then
// points c at it and retires the generation it replaced. A save refused for
// another gateway's write, such as a webhook delivery, is made again over it
// while that write left the source's configuration, commit and index as s
// found them; otherwise the new generation stays unnamed and goes. The caller
// holds n's lock.
func (n *notebook) saveIndexed(ctx context.Context, c *cache, s State, replaced string) error {
	err := n.save(s)
	for attempt := 1; errors.Is(err, ErrConflict) && attempt < 3; attempt++ {
		if n.refresh() != nil {
			break
		}
		now, ok := n.state.Sources[s.ID]
		if !ok || now.Generation != s.Generation || now.Downloaded != s.Downloaded || now.Index.Generation != replaced {
			break
		}
		now.Index = s.Index
		err = n.save(now)
	}
	current := s.Index.Generation
	switch {
	case err == nil:
		c.point(current)
		c.retire(ctx, current, replaced)
	case errors.Is(err, ErrConflict) && current != replaced:
		c.discard(ctx, current)
	}
	return err
}

// evidenceListing is a source's stored evidence as this gateway last listed
// it, after seeing the given number of other gateways' state changes.
type evidenceListing struct {
	seen uint64
	keys map[string]bool
}

// storedEvidence lists source id's stored evidence keys, again whenever
// another gateway changed the notebook's state since the last listing, as it
// does when it retains evidence. The caller holds n's lock.
func (n *notebook) storedEvidence(ctx context.Context, store blob.Store, key, id string) (map[string]bool, error) {
	if l := n.evidence[id]; l != nil && l.seen == n.seen {
		return l.keys, nil
	}
	keys, err := store.List(ctx, key+"/evidence/")
	if err != nil {
		return nil, blobFailure(err)
	}
	l := &evidenceListing{seen: n.seen, keys: make(map[string]bool, len(keys))}
	for _, k := range keys {
		l.keys[k] = true
	}
	if n.evidence == nil {
		n.evidence = map[string]*evidenceListing{}
	}
	n.evidence[id] = l
	return l.keys, nil
}

// readEvidence returns evidence file rel: this directory's copy, or else the
// stored object, then copied here. Callers verify what they read.
func (c *cache) readEvidence(ctx context.Context, rel string, limit int64) ([]byte, error) {
	file := filepath.Join(c.dir, "evidence", filepath.FromSlash(rel))
	b, err := regularRead(file, limit)
	if c.blobs == nil || !errors.Is(err, fs.ErrNotExist) {
		return b, err
	}
	key := c.key + "/evidence/" + rel
	stored, err := c.evidence(ctx)
	if err != nil {
		return nil, err
	}
	if !stored[key] {
		return nil, fmt.Errorf("%s: %w", key, fs.ErrNotExist)
	}
	b, err = blob.Read(ctx, c.blobs, key, limit)
	switch {
	case errors.Is(err, blob.ErrTooLarge):
		return nil, ErrLimit
	case errors.Is(err, fs.ErrNotExist):
		return nil, err
	case err != nil:
		return nil, blobFailure(err)
	}
	// A copy spares the next read a request; failing to keep one costs only that.
	_ = atomicFile(file, b)
	return b, nil
}

// keepEvidence stores evidence file rel in blob storage, then keeps a copy here.
func (c *cache) keepEvidence(ctx context.Context, rel string, raw []byte) error {
	file := filepath.Join(c.dir, "evidence", filepath.FromSlash(rel))
	if c.blobs == nil {
		return atomicFile(file, raw)
	}
	stored, err := c.evidence(ctx)
	if err != nil {
		return err
	}
	key := c.key + "/evidence/" + rel
	if err = blob.Write(ctx, c.blobs, key, raw); err != nil {
		return blobFailure(err)
	}
	stored[key] = true
	_ = atomicFile(file, raw)
	return nil
}

// addEvidence counts bytes just stored as source id's retained evidence. They
// are stored whatever another gateway wrote meanwhile, so a save refused for
// that write counts them again over the state it left. The caller holds n's
// lock.
func (n *notebook) addEvidence(id string, added int64) error {
	if added == 0 {
		return nil
	}
	for attempt := 1; ; attempt++ {
		s, ok := n.state.Sources[id]
		if !ok {
			return ErrConflict
		}
		s.EvidenceBytes += added
		err := n.save(s)
		if err == nil || !errors.Is(err, ErrConflict) || attempt == 3 {
			return err
		}
		if err = n.refresh(); err != nil {
			return err
		}
	}
}

// mirrored is a commit a gateway's Git mirror was found to hold.
type mirrored struct {
	commit string
	dir    os.FileInfo
}

// mirror makes c's Git mirror hold the commit s names: a refresh on another
// gateway advances the source without touching this gateway's mirror. The
// caller holds n's lock.
func (n *notebook) mirror(ctx context.Context, c *cache, s State, token string) error {
	if s.Downloaded == "" {
		return nil
	}
	dir := filepath.Join(c.dir, "source.git")
	info, err := os.Stat(dir)
	if known, ok := n.mirrors[s.ID]; ok && err == nil && known.commit == s.Downloaded && os.SameFile(known.dir, info) {
		return nil
	}
	if err == nil {
		_, err = c.git(ctx, dir, "", 128, "cat-file", "-e", s.Downloaded+"^{tree}")
	}
	if err != nil {
		if err = c.refresh(ctx, s.Repository, s.Downloaded, token); err != nil {
			return err
		}
	}
	return n.remember(c, s.ID, s.Downloaded)
}

// remember records that c's mirror holds commit.
func (n *notebook) remember(c *cache, id, commit string) error {
	info, err := os.Stat(filepath.Join(c.dir, "source.git"))
	if err != nil {
		return err
	}
	if n.mirrors == nil {
		n.mirrors = map[string]mirrored{}
	}
	n.mirrors[id] = mirrored{commit, info}
	return nil
}
