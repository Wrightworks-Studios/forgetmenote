package reposource

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"
)

type vectorManifest struct {
	Config     string         `json:"config"`
	Dimensions int            `json:"dimensions"`
	Rows       map[string]int `json:"rows"`
	Hash       string         `json:"hash"`
}

func sortChunks(chunks []SourceChunk) {
	sort.Slice(chunks, func(i, j int) bool {
		if chunks[i].Path != chunks[j].Path {
			return chunks[i].Path < chunks[j].Path
		}
		return chunks[i].StartByte < chunks[j].StartByte
	})
}
func vectors(idx sourceIndex, config IndexConfig) (vectorManifest, *os.File, error) {
	m, f, err := readVectors(idx.dir)
	if err == nil && (m.Config != config.embeddingID() || m.Dimensions != config.Dimensions || m.Dimensions < 1) {
		f.Close()
		return m, nil, ErrUnavailable
	}
	return m, f, err
}

// readVectors opens the vectors of the generation in dir, checked against
// their own manifest. A generation without embeddings has none.
func readVectors(dir string) (vectorManifest, *os.File, error) {
	var m vectorManifest
	b, err := regularRead(filepath.Join(dir, "vectors.json"), int64(maxIndexBytes()))
	if err != nil {
		return m, nil, err
	}
	if json.Unmarshal(b, &m) != nil || m.Dimensions < 0 || m.Dimensions > 4096 || m.Dimensions == 0 && len(m.Rows) > 0 || len(m.Rows) > maxChunks() {
		return m, nil, ErrUnavailable
	}
	p := filepath.Join(dir, "vectors.f32")
	st, err := os.Lstat(p)
	if err != nil || !st.Mode().IsRegular() || st.Size() != int64(len(m.Rows)*m.Dimensions*4) {
		return m, nil, ErrUnavailable
	}
	seen := map[int]bool{}
	for key, row := range m.Rows {
		if !validHash(key) || row < 0 || row >= len(m.Rows) || seen[row] {
			return m, nil, ErrUnavailable
		}
		seen[row] = true
	}
	f, err := os.Open(p)
	if err != nil {
		return m, nil, err
	}
	hasher := sha256.New()
	if _, err = io.Copy(hasher, f); err != nil || hex.EncodeToString(hasher.Sum(nil)) != m.Hash {
		f.Close()
		return m, nil, ErrUnavailable
	}
	return m, f, nil
}
func readVector(f *os.File, row, dimensions int) ([]float32, error) {
	b := make([]byte, dimensions*4)
	if _, err := f.ReadAt(b, int64(row*dimensions*4)); err != nil {
		return nil, err
	}
	v := make([]float32, dimensions)
	var norm float64
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
		if math.IsNaN(float64(v[i])) || math.IsInf(float64(v[i]), 0) {
			return nil, ErrUnavailable
		}
		norm += float64(v[i]) * float64(v[i])
	}
	if math.Abs(norm-1) > .01 {
		return nil, ErrUnavailable
	}
	return v, nil
}

// publishIndex writes idx as a new generation, reusing prior's files where
// they are unchanged, and stores it in blob storage. The caller then saves a
// source state naming it, which retires the generation it replaced.
func (c *cache) publishIndex(ctx context.Context, idx *sourceIndex, prior sourceIndex, newVectors map[string][]float32, config IndexConfig) error {
	base := filepath.Join(c.dir, "index", "generations")
	if err := os.MkdirAll(base, 0700); err != nil {
		return err
	}
	stage, err := newGeneration(base)
	if err != nil {
		return err
	}
	published, stored := false, false
	defer func() {
		if !published {
			if stored {
				// Cleanup outlives a canceled publication.
				cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
				c.deleteGeneration(cleanup, filepath.Base(stage))
				cancel()
			}
			_ = os.RemoveAll(stage)
		}
	}()
	if prior.dir != "" && idx.Commit == prior.Commit && idx.ConfigID == prior.ConfigID && idx.ChunkHash == prior.ChunkHash && idx.Count == prior.Count {
		if err = os.Link(filepath.Join(prior.dir, "chunks.jsonl"), filepath.Join(stage, "chunks.jsonl")); err != nil {
			return err
		}
	} else {
		var b bytes.Buffer
		enc := json.NewEncoder(&b)
		for _, ch := range idx.Chunks {
			if err = enc.Encode(ch); err != nil {
				return err
			}
			if b.Len() > maxChunkBytes() {
				return ErrLimit
			}
		}
		idx.Count = len(idx.Chunks)
		idx.ChunkHash = hash(b.Bytes())
		if err = atomicFile(filepath.Join(stage, "chunks.jsonl"), b.Bytes()); err != nil {
			return err
		}
	}
	if idx.MapVersion != "" {
		raw, err := json.Marshal(idx.Map)
		if err != nil || len(raw) > maxMapBytes {
			return ErrLimit
		}
		idx.MapHash = hash(raw)
		if prior.dir != "" && prior.MapHash == idx.MapHash {
			err = os.Link(filepath.Join(prior.dir, "map.json"), filepath.Join(stage, "map.json"))
		} else {
			err = atomicFile(filepath.Join(stage, "map.json"), raw)
		}
		if err != nil {
			return err
		}
	}
	manifest, _ := json.Marshal(idx)
	if err = atomicFile(filepath.Join(stage, "manifest.json"), manifest); err != nil {
		return err
	}
	if prior.dir != "" && idx.ChunkHash == prior.ChunkHash && idx.EmbeddingID == prior.EmbeddingID && len(newVectors) == 0 {
		for _, name := range []string{"vectors.json", "vectors.f32"} {
			if err = os.Link(filepath.Join(prior.dir, name), filepath.Join(stage, name)); err != nil {
				return err
			}
		}
	} else {
		vm := vectorManifest{Config: config.embeddingID(), Dimensions: config.Dimensions, Rows: map[string]int{}}
		old, oldFile, _ := vectors(prior, config)
		if oldFile != nil {
			defer oldFile.Close()
		}
		vf, err := os.OpenFile(filepath.Join(stage, "vectors.f32"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		hasher := sha256.New()
		writer := bufio.NewWriterSize(io.MultiWriter(vf, hasher), 64<<10)
		for _, ch := range idx.Chunks {
			if _, ok := vm.Rows[ch.EmbeddingHash]; ok {
				continue
			}
			v := newVectors[ch.EmbeddingHash]
			if v == nil && oldFile != nil {
				if row, ok := old.Rows[ch.EmbeddingHash]; ok {
					v, _ = readVector(oldFile, row, config.Dimensions)
				}
			}
			if len(v) == 0 {
				continue
			}
			if len(v) != config.Dimensions {
				vf.Close()
				return ErrUnavailable
			}
			if err = binary.Write(writer, binary.LittleEndian, v); err != nil {
				vf.Close()
				return err
			}
			vm.Rows[ch.EmbeddingHash] = len(vm.Rows)
		}
		err = writer.Flush()
		if err == nil {
			err = vf.Sync()
		}
		closeErr := vf.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		vm.Hash = hex.EncodeToString(hasher.Sum(nil))
		data, _ := json.Marshal(vm)
		if err = atomicFile(filepath.Join(stage, "vectors.json"), data); err != nil {
			return err
		}
	}
	size, err := diskBytes(stage)
	if err != nil {
		return err
	}
	if size > int64(maxIndexBytes()) {
		return ErrLimit
	}
	if err = syncDir(stage); err != nil {
		return err
	}
	if c.blobs != nil {
		stored = true
		if err = c.storeGeneration(ctx, stage, filepath.Base(stage)); err != nil {
			return err
		}
	}
	published = true
	idx.dir = stage
	return nil
}
