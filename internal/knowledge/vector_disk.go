package knowledge

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"

	"forgetmenote/internal/capacity"
)

const vectorMagic = "FMNVEC2\n"
const vectorDiskVersion = 2

type vectorDiskHeader struct {
	Version    int    `json:"version"`
	ConfigID   string `json:"configId"`
	Revision   string `json:"revision"`
	Dimensions int    `json:"dimensions"`
	Count      int    `json:"count"`
}

func validVectorHash(hash string) bool {
	if len(hash) != 64 {
		return false
	}
	for _, c := range hash {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func (h vectorDiskHeader) validate() error {
	if h.Version != vectorDiskVersion || h.Dimensions < 1 || h.Dimensions > 4096 || h.Count < 0 || h.Count > maxVectorValues()/h.Dimensions || !validVectorHash(h.ConfigID) || h.Revision != "" && !historyRevisionValid(h.Revision) {
		return errors.New("invalid semantic vector header")
	}
	return nil
}

type vectorBudgetWriter struct {
	output    io.Writer
	remaining int
}

func (w *vectorBudgetWriter) Write(raw []byte) (int, error) {
	if len(raw) > w.remaining {
		return 0, capacity.IndexBytes.Error()
	}
	n, err := w.output.Write(raw)
	w.remaining -= n
	if n != len(raw) && err == nil {
		err = io.ErrShortWrite
	}
	return n, err
}

func writeVectorCache(path string, cache vectorCache, dimensions int, syncDirectory func(string) error) error {
	metadata := vectorDiskHeader{Version: vectorDiskVersion, ConfigID: cache.ConfigID, Revision: cache.Revision, Dimensions: dimensions, Count: len(cache.Vectors)}
	if err := metadata.validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), vectorsTemporary)
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if err = encodeVectorFile(file, cache, dimensions); err != nil {
		return err
	}
	if err = file.Sync(); err != nil {
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	if err = os.Rename(file.Name(), path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

// encodeVectorFile writes cache in the semantic.bin layout, keys in byte order,
// so equal vector sets encode to equal bytes. A SHA-256 of everything before it
// ends the file, and the whole stays within the index byte budget.
func encodeVectorFile(output io.Writer, cache vectorCache, dimensions int) error {
	metadata := vectorDiskHeader{Version: vectorDiskVersion, ConfigID: cache.ConfigID, Revision: cache.Revision, Dimensions: dimensions, Count: len(cache.Vectors)}
	if err := metadata.validate(); err != nil {
		return err
	}
	hasher := sha256.New()
	buffered := bufio.NewWriterSize(io.MultiWriter(output, hasher), 64<<10)
	writer := &vectorBudgetWriter{output: buffered, remaining: capacity.IndexBytes.Limit() - sha256.Size}
	header, err := json.Marshal(metadata)
	if err != nil || len(header) > 1024 {
		return errors.New("invalid semantic vector header")
	}
	if _, err = io.WriteString(writer, vectorMagic); err != nil {
		return err
	}
	if err = binary.Write(writer, binary.LittleEndian, uint32(len(header))); err != nil {
		return err
	}
	if _, err = writer.Write(header); err != nil {
		return err
	}
	keys := make([]string, 0, len(cache.Vectors))
	for key := range cache.Vectors {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	encoded := make([]byte, dimensions*4)
	for _, key := range keys {
		vector := cache.Vectors[key]
		if !validVectorHash(key) {
			return errors.New("invalid semantic vector key")
		}
		if _, err = normalize(vector, dimensions); err != nil {
			return err
		}
		if err = binary.Write(writer, binary.LittleEndian, uint16(len(key))); err != nil {
			return err
		}
		if _, err = io.WriteString(writer, key); err != nil {
			return err
		}
		for i, value := range vector {
			binary.LittleEndian.PutUint32(encoded[i*4:], math.Float32bits(value))
		}
		if _, err = writer.Write(encoded); err != nil {
			return err
		}
	}
	if err = buffered.Flush(); err != nil {
		return err
	}
	_, err = output.Write(hasher.Sum(nil))
	return err
}

func readVectorCache(path string, config Config) (vectorCache, os.FileInfo, error) {
	cache, dimensions, info, err := readStoredVectors(path)
	if err != nil {
		return vectorCache{}, nil, err
	}
	if cache.ConfigID == config.ID() && dimensions != config.Dimensions {
		return vectorCache{}, nil, errors.New("semantic vector dimensions disagree with the active configuration")
	}
	return cache, info, nil
}

func readStoredVectors(path string) (vectorCache, int, os.FileInfo, error) {
	file, info, err := openKnowledgeFile(path, capacity.IndexBytes.Limit())
	if err != nil {
		return vectorCache{}, 0, nil, err
	}
	defer file.Close()
	cache, dimensions, err := decodeVectorFile(file, info.Size())
	if err != nil {
		return vectorCache{}, 0, nil, err
	}
	after, err := file.Stat()
	if err != nil || !sameKnowledgeFile(info, after) {
		return vectorCache{}, 0, nil, errors.New("semantic vector file changed while reading")
	}
	return cache, dimensions, info, nil
}

// decodeVectorFile reads length bytes in the semantic.bin layout, checking its
// header, every key and vector, and the trailing digest.
func decodeVectorFile(file io.Reader, length int64) (vectorCache, int, error) {
	var cache vectorCache
	var err error
	if length < int64(len(vectorMagic)+4+sha256.Size) {
		return cache, 0, errors.New("incomplete semantic vector file")
	}
	hasher := sha256.New()
	reader := bufio.NewReaderSize(io.TeeReader(io.LimitReader(file, length-sha256.Size), hasher), 64<<10)
	magic := make([]byte, len(vectorMagic))
	if _, err = io.ReadFull(reader, magic); err != nil || string(magic) != vectorMagic {
		return cache, 0, errors.New("unsupported semantic vector file version")
	}
	var size uint32
	if err = binary.Read(reader, binary.LittleEndian, &size); err != nil || size > 1024 {
		return cache, 0, errors.New("invalid semantic vector header size")
	}
	raw := make([]byte, size)
	if _, err = io.ReadFull(reader, raw); err != nil {
		return cache, 0, err
	}
	var header vectorDiskHeader
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&header) != nil || decoder.Decode(new(any)) != io.EOF || header.validate() != nil {
		return cache, 0, errors.New("invalid semantic vector header")
	}
	if int64(header.Count)*(2+64+int64(header.Dimensions)*4) > length-int64(len(vectorMagic)+4+len(raw)+sha256.Size) {
		return cache, 0, errors.New("semantic vector count exceeds file bounds")
	}
	cache = vectorCache{ConfigID: header.ConfigID, Revision: header.Revision, Vectors: map[string][]float32{}}
	encoded := make([]byte, header.Dimensions*4)
	for row := 0; row < header.Count; row++ {
		var size uint16
		if err = binary.Read(reader, binary.LittleEndian, &size); err != nil || size != 64 {
			return vectorCache{}, 0, errors.New("invalid semantic vector key size")
		}
		key := make([]byte, size)
		if _, err = io.ReadFull(reader, key); err != nil {
			return vectorCache{}, 0, err
		}
		if !validVectorHash(string(key)) {
			return vectorCache{}, 0, errors.New("invalid semantic vector content hash")
		}
		if _, exists := cache.Vectors[string(key)]; exists {
			return vectorCache{}, 0, errors.New("duplicate semantic vector key")
		}
		if _, err = io.ReadFull(reader, encoded); err != nil {
			return vectorCache{}, 0, err
		}
		vector := make([]float32, header.Dimensions)
		for i := range vector {
			vector[i] = math.Float32frombits(binary.LittleEndian.Uint32(encoded[i*4:]))
		}
		if _, err = normalize(vector, header.Dimensions); err != nil {
			return vectorCache{}, 0, err
		}
		cache.Vectors[string(key)] = vector
	}
	if _, err = reader.ReadByte(); err != io.EOF {
		return vectorCache{}, 0, errors.New("trailing semantic vector data")
	}
	digest := make([]byte, sha256.Size)
	if _, err = io.ReadFull(file, digest); err != nil || !bytes.Equal(digest, hasher.Sum(nil)) {
		return vectorCache{}, 0, errors.New("semantic vector integrity check failed")
	}
	return cache, header.Dimensions, nil
}

var ErrVectorCache = errors.New("semantic vector cache unavailable")
