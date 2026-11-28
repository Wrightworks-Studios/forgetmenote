package capture

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"

	"forgetmenote/internal/blob"
	"forgetmenote/internal/notestore"
)

// blockWorkers is how many blocks are copied at once, so a store that
// answers each request slowly, such as S3, is not read one block at a time.
const blockWorkers = 8

// copyBlocks copies every content block the snapshot tx uses into
// blobs/blocks/, checking each one's size and hash. The caller holds the
// blocks (notestore.LockBlocks) from before the snapshot, so none is
// removed meanwhile, and none another file version or a purge freed is
// copied.
func (c *capture) copyBlocks(ctx context.Context, tx pgx.Tx) error {
	blocks, err := notestore.ReferencedBlocks(ctx, tx, c.o.DB.Org)
	if err != nil {
		return err
	}
	if len(blocks) == 0 {
		return nil
	}
	if err := os.MkdirAll(filepath.Join(c.out, "blobs", "blocks"), 0o700); err != nil {
		return err
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	work := make(chan notestore.Block)
	var workers sync.WaitGroup
	for range min(blockWorkers, len(blocks)) {
		workers.Go(func() {
			for b := range work {
				if err := c.copyBlock(ctx, b); err != nil {
					cancel(err)
				}
			}
		})
	}
	for _, b := range blocks {
		select {
		case work <- b:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
	}
	close(work)
	workers.Wait()
	if err := context.Cause(ctx); err != nil {
		return err
	}
	c.blocks = len(blocks)
	for _, b := range blocks {
		c.bytes += b.Size
	}
	c.included = append(c.included, "blobs/blocks")
	return nil
}

func (c *capture) copyBlock(ctx context.Context, b notestore.Block) error {
	r, err := c.o.Blobs.Get(ctx, b.Key)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("content block %s, which the snapshot uses, is missing from the blob store", b.Key)
	}
	if err != nil {
		return err
	}
	defer r.Close()
	f, err := os.OpenFile(filepath.Join(c.out, "blobs", filepath.FromSlash(b.Key)), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(r, b.Size+1))
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("content block %s: %w", b.Key, err)
	}
	if n != b.Size || [32]byte(h.Sum(nil)) != b.SHA256 {
		return fmt.Errorf("content block %s in the blob store does not match its hash and size", b.Key)
	}
	return nil
}

// Repository sources keep their durable files in blob storage under
// repository-sources/<notebook>/<source>/ (internal/reposource/blobs.go owns
// that layout): the index generation the notebook's source state names,
// whose vectors and map labels were paid for, and retained evidence. A
// generation is stored whole before the state names it and removed only
// once the state names its successor; evidence is stored before the state
// counts it and never removed. So both are copied after the snapshot: what
// the snapshot names is there unless a refresh during the capture retired
// it, which is detected and reported. Each gateway's Git mirror and caches
// beside them are not copied.
const sourcesPrefix = "repository-sources"

var (
	sourceID        = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
	evidenceKey     = regexp.MustCompile(`/evidence/(refs/[0-9a-f]{64}\.json|blobs/[0-9a-f]{64}|notebooks/[0-9a-f]{40}\.json)$`)
	generationFiles = []string{"chunks.jsonl", "vectors.f32", "vectors.json", "map.json"}
)

// sourceState is the part of a notebook's repository source state document
// that names each source's index generation.
type sourceState struct {
	Sources map[string]struct {
		Index struct {
			Generation string `json:"generation"`
		} `json:"index"`
	} `json:"sources"`
}

// copySources copies the durable files of every repository source the
// snapshot tx's source states list.
func (c *capture) copySources(ctx context.Context, tx pgx.Tx) error {
	const kind = "repository-sources"
	projects, err := c.o.DB.DocumentIDs(ctx, tx, kind)
	if err != nil {
		return err
	}
	copied := 0
	for _, project := range projects {
		if !sourceID.MatchString(project) {
			return fmt.Errorf("invalid repository source notebook %q", project)
		}
		_, body, err := c.o.DB.ReadDocument(ctx, tx, kind, project, -1)
		if err != nil {
			return err
		}
		var state sourceState
		if err := json.Unmarshal(body, &state); err != nil {
			return fmt.Errorf("the repository sources of %s: %w", project, err)
		}
		ids := make([]string, 0, len(state.Sources))
		for id := range state.Sources {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		for _, id := range ids {
			if !sourceID.MatchString(id) {
				return fmt.Errorf("invalid repository source %q of %s", id, project)
			}
			dir := sourcesPrefix + "/" + project + "/" + id
			n, err := c.copySource(ctx, dir, state.Sources[id].Index.Generation)
			if err != nil {
				return fmt.Errorf("repository source %s of %s: %w", id, project, err)
			}
			copied += n
		}
	}
	if copied > 0 {
		c.included = append(c.included, "blobs/"+sourcesPrefix)
	}
	return nil
}

// copySource copies the source under dir's generation and evidence and
// returns how many files it copied.
func (c *capture) copySource(ctx context.Context, dir, generation string) (int, error) {
	copied := 0
	pointer := ""
	if generation == "" {
		// A state saved before states named their generation: the source's
		// directory in a blob store at the data directory names it.
		raw, err := blob.Read(ctx, c.o.Blobs, dir+"/index/current.json", 4096)
		var named struct {
			Generation string `json:"generation"`
		}
		if err == nil && json.Unmarshal(raw, &named) == nil {
			generation, pointer = named.Generation, dir+"/index/current.json"
		} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return 0, err
		}
	}
	if generation != "" {
		n, err := c.copyGeneration(ctx, dir, generation)
		if err != nil {
			return 0, err
		}
		if n > 0 && pointer != "" {
			if _, err := c.copyObject(ctx, pointer); err != nil {
				return 0, err
			}
			n++
		}
		copied += n
	}
	keys, err := c.o.Blobs.List(ctx, dir+"/evidence/")
	if err != nil {
		return 0, err
	}
	for _, key := range keys {
		if !evidenceKey.MatchString(key) {
			continue
		}
		ok, err := c.copyObject(ctx, key)
		if err != nil {
			return 0, err
		}
		if !ok {
			return 0, fmt.Errorf("retained evidence %s disappeared while it was copied", key)
		}
		copied++
	}
	return copied, nil
}

// copyGeneration copies generation's files, manifest first, and returns how
// many it copied. A generation that a refresh retired during the capture,
// so that a file is missing, is left out whole and reported.
func (c *capture) copyGeneration(ctx context.Context, dir, generation string) (int, error) {
	prefix := dir + "/index/generations/" + generation + "/"
	if !blob.ValidKey(prefix+"manifest.json") || strings.Contains(generation, "/") {
		return 0, fmt.Errorf("invalid index generation %q", generation)
	}
	missing := func() (int, error) {
		c.excluded = append(c.excluded, Exclusion{"blobs/" + strings.TrimSuffix(prefix, "/"), "The index generation the snapshot's source state names was retired by a refresh during the capture. The restored instance builds the source's index again, which embedding and map classification charge for when they are enabled."})
		return 0, os.RemoveAll(filepath.Join(c.out, "blobs", filepath.FromSlash(prefix)))
	}
	ok, err := c.copyObject(ctx, prefix+"manifest.json")
	if err != nil || !ok {
		if err == nil {
			return missing()
		}
		return 0, err
	}
	raw, err := os.ReadFile(filepath.Join(c.out, "blobs", filepath.FromSlash(prefix+"manifest.json")))
	if err != nil {
		return 0, err
	}
	var manifest struct {
		MapVersion string `json:"mapVersion"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return 0, fmt.Errorf("index generation %s: %w", generation, err)
	}
	copied := 1
	for _, file := range generationFiles {
		ok, err := c.copyObject(ctx, prefix+file)
		if err != nil {
			return 0, err
		}
		if !ok && (file != "map.json" || manifest.MapVersion != "") {
			return missing()
		}
		if ok {
			copied++
		}
	}
	return copied, nil
}

// copyObject copies the blob store's object key to blobs/<key>, reporting
// false for a missing one.
func (c *capture) copyObject(ctx context.Context, key string) (bool, error) {
	if !blob.ValidKey(key) {
		return false, fmt.Errorf("invalid blob key %q", key)
	}
	r, err := c.o.Blobs.Get(ctx, key)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer r.Close()
	target := filepath.Join(c.out, "blobs", filepath.FromSlash(key))
	if err := os.MkdirAll(filepath.Join(c.out, "blobs", filepath.FromSlash(path.Dir(key))), 0o700); err != nil {
		return false, err
	}
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return false, err
	}
	_, err = io.Copy(f, r)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return err == nil, err
}
