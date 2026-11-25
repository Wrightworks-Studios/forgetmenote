package notestore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"slices"
	"sort"
	"strings"
	"sync"

	"forgetmenote/internal/blob"
	"forgetmenote/internal/gitobj"

	"github.com/jackc/pgx/v5"
)

// Content layout. A file under inlineLimit is stored in its file_versions
// row. Markdown is cut after blank lines ("\n\n"): paragraphs are packed into
// a block until it holds at least mdMinBlock bytes, and a paragraph that would
// pass mdMaxBlock is cut there. Because a block closes at the first paragraph
// end past the minimum, an edit changes the blocks around it and the
// boundaries soon fall where they were. Other files are cut every
// binaryBlock bytes. The block list is itself cut into segments after a block
// whose hash has its low bits clear, so one edit stores one new segment rather
// than a new list of every block.
const (
	inlineLimit = 8 << 10
	mdMinBlock  = 1 << 10
	mdMaxBlock  = 64 << 10
	binaryBlock = 4 << 20
	segmentMask = 0x0f
	segmentMin  = 8
	segmentMax  = 256
	blockRecord = 36 // SHA-256 and a 4-byte size
	segRecord   = 40 // SHA-256 and an 8-byte length
)

type blockRef struct {
	sha  [32]byte
	size uint32
}

type segmentRef struct {
	sha    [32]byte
	length int64
}

func blockKey(sha [32]byte) string { return "blocks/" + hex.EncodeToString(sha[:]) }

func isMarkdown(p string) bool { return strings.EqualFold(path.Ext(p), ".md") }

// splitBlocks returns content's blocks; joined, they are content.
func splitBlocks(p string, content []byte) [][]byte {
	var blocks [][]byte
	if !isMarkdown(p) {
		for len(content) > binaryBlock {
			blocks = append(blocks, content[:binaryBlock])
			content = content[binaryBlock:]
		}
		if len(content) > 0 {
			blocks = append(blocks, content)
		}
		return blocks
	}
	start := 0
	for pos := 0; pos < len(content); {
		end := len(content)
		if i := bytes.Index(content[pos:], []byte("\n\n")); i >= 0 {
			end = pos + i + 2
		}
		for end-start > mdMaxBlock {
			blocks = append(blocks, content[start:start+mdMaxBlock])
			start += mdMaxBlock
		}
		if end-start >= mdMinBlock {
			blocks = append(blocks, content[start:end])
			start = end
		}
		pos = end
	}
	if start < len(content) {
		blocks = append(blocks, content[start:])
	}
	return blocks
}

// A version is a file's content prepared for storing.
type version struct {
	blob     gitobj.ID
	data     []byte
	blocks   [][]byte
	refs     []blockRef
	segments []segmentRef
	bodies   [][]byte // each segment's stored body
}

func prepare(p string, data []byte) *version {
	v := &version{blob: gitobj.BlobID(data), data: data}
	if len(data) < inlineLimit {
		return v
	}
	v.blocks = splitBlocks(p, data)
	v.refs = make([]blockRef, len(v.blocks))
	for i, b := range v.blocks {
		v.refs[i] = blockRef{sha: sha256.Sum256(b), size: uint32(len(b))}
	}
	for start := 0; start < len(v.refs); {
		end := start
		for end < len(v.refs) {
			end++
			n := end - start
			if n >= segmentMax || n >= segmentMin && v.refs[end-1].sha[31]&segmentMask == 0 {
				break
			}
		}
		body := make([]byte, 0, (end-start)*blockRecord)
		var length int64
		for _, r := range v.refs[start:end] {
			body = append(body, r.sha[:]...)
			body = binary.BigEndian.AppendUint32(body, r.size)
			length += int64(r.size)
		}
		v.segments = append(v.segments, segmentRef{sha: sha256.Sum256(body), length: length})
		v.bodies = append(v.bodies, body)
		start = end
	}
	return v
}

func (v *version) inline() bool { return v.refs == nil }

func encodeSegments(segments []segmentRef) []byte {
	b := make([]byte, 0, len(segments)*segRecord)
	for _, s := range segments {
		b = append(b, s.sha[:]...)
		b = binary.BigEndian.AppendUint64(b, uint64(s.length))
	}
	return b
}

func decodeSegments(raw []byte) ([]segmentRef, error) {
	if len(raw)%segRecord != 0 {
		return nil, errors.New("corrupt segment list")
	}
	segments := make([]segmentRef, len(raw)/segRecord)
	for i := range segments {
		r := raw[i*segRecord:]
		copy(segments[i].sha[:], r[:32])
		segments[i].length = int64(binary.BigEndian.Uint64(r[32:40]))
	}
	return segments, nil
}

func decodeBlocks(raw []byte) ([]blockRef, error) {
	if len(raw)%blockRecord != 0 {
		return nil, errors.New("corrupt segment")
	}
	refs := make([]blockRef, len(raw)/blockRecord)
	for i := range refs {
		r := raw[i*blockRecord:]
		copy(refs[i].sha[:], r[:32])
		refs[i].size = binary.BigEndian.Uint32(r[32:36])
	}
	return refs, nil
}

// A manifest says where a file version's bytes are.
type manifest struct {
	size     int64
	inline   []byte
	segments []segmentRef
	blocks   []blockRef
	offsets  []int64 // of each block
}

func (m *manifest) bytes() int64 {
	return 96 + int64(len(m.inline)) + int64(len(m.segments))*segRecord + int64(len(m.blocks))*(blockRecord+8)
}

// manifest returns where blob's bytes are, or an fs.ErrNotExist error.
func (s *Store) manifest(ctx context.Context, q querier, id gitobj.ID) (*manifest, error) {
	if m, ok := s.manifests.get(id); ok {
		return m, nil
	}
	m := &manifest{}
	var segments []byte
	err := q.QueryRow(ctx, `SELECT size, inline, segments FROM file_versions WHERE org = $1 AND blob_sha = $2`, s.db.Org, id[:]).Scan(&m.size, &m.inline, &segments)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("content %s: %w", id, fs.ErrNotExist)
	}
	if err != nil {
		return nil, err
	}
	if segments != nil {
		if m.segments, err = decodeSegments(segments); err != nil {
			return nil, err
		}
		keys := make([][]byte, len(m.segments))
		for i, seg := range m.segments {
			keys[i] = seg.sha[:]
		}
		rows, err := q.Query(ctx, `SELECT segment_sha, blocks FROM content_segments WHERE org = $1 AND segment_sha = ANY($2)`, s.db.Org, keys)
		if err != nil {
			return nil, err
		}
		bodies := map[[32]byte][]byte{}
		for rows.Next() {
			var sha, body []byte
			if err := rows.Scan(&sha, &body); err != nil {
				rows.Close()
				return nil, err
			}
			bodies[[32]byte(sha)] = body
		}
		rows.Close()
		if err = rows.Err(); err != nil {
			return nil, err
		}
		var offset int64
		for _, seg := range m.segments {
			body, ok := bodies[seg.sha]
			if !ok || sha256.Sum256(body) != seg.sha {
				return nil, fmt.Errorf("content %s: segment %x is missing or corrupt", id, seg.sha)
			}
			refs, err := decodeBlocks(body)
			if err != nil {
				return nil, err
			}
			for _, r := range refs {
				m.blocks = append(m.blocks, r)
				m.offsets = append(m.offsets, offset)
				offset += int64(r.size)
			}
		}
		if offset != m.size {
			return nil, fmt.Errorf("content %s: blocks hold %d bytes, not %d", id, offset, m.size)
		}
	}
	s.manifests.put(id, m, m.bytes())
	return m, nil
}

func (s *Store) block(ctx context.Context, r blockRef) ([]byte, error) {
	if b, ok := s.blocks.get(r.sha); ok {
		return b, nil
	}
	raw, err := blob.Read(ctx, s.blobs, blockKey(r.sha), int64(r.size))
	if err != nil {
		return nil, fmt.Errorf("content block %x: %w", r.sha, err)
	}
	if len(raw) != int(r.size) || sha256.Sum256(raw) != r.sha {
		return nil, fmt.Errorf("content block %x is corrupt", r.sha)
	}
	s.blocks.put(r.sha, raw, int64(len(raw)))
	return raw, nil
}

// fetchBlocks loads blocks not yet cached, a few at a time, so a cold read of
// a long note does not wait on each block in turn.
func (s *Store) fetchBlocks(ctx context.Context, refs []blockRef) ([][]byte, error) {
	out := make([][]byte, len(refs))
	var missing []int
	for i, r := range refs {
		if b, ok := s.blocks.get(r.sha); ok {
			out[i] = b
		} else {
			missing = append(missing, i)
		}
	}
	if len(missing) == 0 {
		return out, nil
	}
	workers := min(8, len(missing))
	next := make(chan int)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				b, err := s.block(ctx, refs[i])
				if err != nil {
					errs <- err
					return
				}
				out[i] = b
			}
		}()
	}
	for _, i := range missing {
		select {
		case next <- i:
		case err := <-errs:
			close(next)
			wg.Wait()
			return nil, err
		}
	}
	close(next)
	wg.Wait()
	select {
	case err := <-errs:
		return nil, err
	default:
	}
	return out, nil
}

func (s *Store) readContent(ctx context.Context, q querier, id gitobj.ID) ([]byte, error) {
	m, err := s.manifest(ctx, q, id)
	if err != nil {
		return nil, err
	}
	if m.inline != nil {
		return bytes.Clone(m.inline), nil
	}
	blocks, err := s.fetchBlocks(ctx, m.blocks)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, m.size)
	for _, b := range blocks {
		out = append(out, b...)
	}
	return out, nil
}

func (s *Store) readRange(ctx context.Context, id gitobj.ID, off, length int64) ([]byte, error) {
	m, err := s.manifest(ctx, s.db.Pool, id)
	if err != nil {
		return nil, err
	}
	if off < 0 || length < 0 || off > m.size || length > m.size-off {
		return nil, invalid("range %d+%d of a %d-byte file", off, length, m.size)
	}
	if m.inline != nil {
		return bytes.Clone(m.inline[off : off+length]), nil
	}
	out := make([]byte, 0, length)
	first := sort.Search(len(m.offsets), func(i int) bool { return m.offsets[i]+int64(m.blocks[i].size) > off })
	for i := first; i < len(m.blocks) && int64(len(out)) < length; i++ {
		r, start := m.blocks[i], m.offsets[i]
		from := max(off-start, 0)
		to := min(int64(r.size), off+length-start)
		if b, ok := s.blocks.get(r.sha); ok {
			out = append(out, b[from:to]...)
			continue
		}
		// A small part of a large block is read alone and not cached.
		if to-from < int64(r.size)/2 && r.size > mdMaxBlock {
			rc, err := s.blobs.GetRange(ctx, blockKey(r.sha), from, to-from)
			if err != nil {
				return nil, err
			}
			part, err := io.ReadAll(io.LimitReader(rc, to-from))
			rc.Close()
			if err != nil {
				return nil, err
			}
			if int64(len(part)) != to-from {
				return nil, fmt.Errorf("content block %x is short", r.sha)
			}
			out = append(out, part...)
			continue
		}
		b, err := s.block(ctx, r)
		if err != nil {
			return nil, err
		}
		out = append(out, b[from:to]...)
	}
	return out, nil
}

// A pendingBlock is a block a write must put before it commits.
type pendingBlock struct {
	ref  blockRef
	data []byte
}

// An unstagedError lists the blocks a write's transaction found it must put:
// blocks, those the files its new content replaces lack, and all, every block
// of that content not staged yet (blocks among them). The write puts them
// outside the transaction, which never waits for a second connection, and
// tries again.
type unstagedError struct{ blocks, all []pendingBlock }

func (e *unstagedError) Error() string {
	return fmt.Sprintf("%d content blocks are not stored yet", len(e.blocks))
}

// stage records blocks in content_blocks, then puts them, outside any
// transaction, so the sweep can find a block whose write then fails, and adds
// them to staged.
func (s *Store) stage(ctx context.Context, blocks []pendingBlock, staged map[[32]byte]bool) error {
	if len(blocks) == 0 {
		return nil
	}
	// Rows are locked in key order, so writers recording the same blocks at
	// once cannot deadlock.
	slices.SortFunc(blocks, func(a, b pendingBlock) int { return bytes.Compare(a.ref.sha[:], b.ref.sha[:]) })
	keys, sizes := make([][]byte, len(blocks)), make([]int32, len(blocks))
	refs, data := make([]blockRef, len(blocks)), make([][]byte, len(blocks))
	for i, b := range blocks {
		keys[i], sizes[i] = bytes.Clone(b.ref.sha[:]), int32(b.ref.size)
		refs[i], data[i] = b.ref, b.data
	}
	if _, err := s.db.Pool.Exec(ctx, `INSERT INTO content_blocks (org, block_sha, size) SELECT $1, * FROM unnest($2::bytea[], $3::integer[])
		ON CONFLICT (org, block_sha) DO UPDATE SET touched = now()`, s.db.Org, keys, sizes); err != nil {
		return err
	}
	if err := s.putBlocks(ctx, refs, data); err != nil {
		return err
	}
	for _, r := range refs {
		staged[r.sha] = true
	}
	return nil
}

// storeVersions gives every blob in refs a file_versions row, locking the
// rows that exist against a concurrent delete, and returns the versions it
// stored. versions holds the content of blobs that may be new; prev maps a
// new blob to the one it replaces, whose segments and blocks it may share
// without writing them again. Any other block must be in staged, or it
// returns an unstagedError listing only blocks staged lacks.
func (s *Store) storeVersions(ctx context.Context, tx pgx.Tx, refs []gitobj.ID, versions map[gitobj.ID]*version, prev map[gitobj.ID]gitobj.ID, staged map[[32]byte]bool) ([]*version, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	keys := make([][]byte, len(refs))
	for i, id := range refs {
		keys[i] = id[:]
	}
	rows, err := tx.Query(ctx, `SELECT blob_sha FROM file_versions WHERE org = $1 AND blob_sha = ANY($2) ORDER BY blob_sha FOR KEY SHARE`, s.db.Org, keys)
	if err != nil {
		return nil, err
	}
	present := map[gitobj.ID]bool{}
	for rows.Next() {
		var sha []byte
		if err := rows.Scan(&sha); err != nil {
			rows.Close()
			return nil, err
		}
		present[toID(sha)] = true
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	var fresh []*version
	seen := map[gitobj.ID]bool{}
	for _, id := range refs {
		if present[id] || seen[id] {
			continue
		}
		seen[id] = true
		v := versions[id]
		if v == nil {
			return nil, fmt.Errorf("content %s: %w", id, fs.ErrNotExist)
		}
		fresh = append(fresh, v)
	}
	if len(fresh) == 0 {
		return nil, nil
	}
	// Rows are locked in key order, so writers storing the same new content
	// in two notebooks at once cannot deadlock.
	slices.SortFunc(fresh, func(a, b *version) int { return bytes.Compare(a.blob[:], b.blob[:]) })
	sharedSegments, sharedBlocks := map[[32]byte]bool{}, map[[32]byte]bool{}
	for _, v := range fresh {
		old, ok := prev[v.blob]
		if !ok || v.inline() {
			continue
		}
		// The replaced file is at the locked head, so its blocks stay.
		m, err := s.manifest(ctx, tx, old)
		if err != nil {
			continue // Nothing shared; every block is staged.
		}
		for _, seg := range m.segments {
			sharedSegments[seg.sha] = true
		}
		for _, r := range m.blocks {
			sharedBlocks[r.sha] = true
		}
	}
	if err := recheckStaged(ctx, tx, s.db.Org, fresh, staged); err != nil {
		return nil, err
	}
	var missing, unstaged []pendingBlock
	listed := map[[32]byte]bool{}
	var newSegments []segmentRef
	bodies := map[[32]byte][]byte{}
	for _, v := range fresh {
		for i, r := range v.refs {
			if staged[r.sha] || listed[r.sha] {
				continue
			}
			listed[r.sha] = true
			b := pendingBlock{r, v.blocks[i]}
			unstaged = append(unstaged, b)
			if !sharedBlocks[r.sha] {
				missing = append(missing, b)
			}
		}
		for i, seg := range v.segments {
			if !sharedSegments[seg.sha] {
				sharedSegments[seg.sha] = true
				newSegments = append(newSegments, seg)
				bodies[seg.sha] = v.bodies[i]
			}
		}
	}
	if len(missing) > 0 {
		return nil, &unstagedError{blocks: missing, all: unstaged}
	}
	slices.SortFunc(newSegments, func(a, b segmentRef) int { return bytes.Compare(a.sha[:], b.sha[:]) })
	var segmentKeys, segmentBodies [][]byte
	for _, seg := range newSegments {
		segmentKeys = append(segmentKeys, bytes.Clone(seg.sha[:]))
		segmentBodies = append(segmentBodies, bodies[seg.sha])
	}
	if len(segmentKeys) > 0 {
		if _, err := tx.Exec(ctx, `INSERT INTO content_segments (org, segment_sha, blocks) SELECT $1, * FROM unnest($2::bytea[], $3::bytea[])
			ON CONFLICT (org, segment_sha) DO UPDATE SET touched = now()`, s.db.Org, segmentKeys, segmentBodies); err != nil {
			return nil, err
		}
	}
	shas, sizes := make([][]byte, len(fresh)), make([]int64, len(fresh))
	inline, segments := make([][]byte, len(fresh)), make([][]byte, len(fresh))
	for i, v := range fresh {
		shas[i], sizes[i] = v.blob[:], int64(len(v.data))
		if v.inline() {
			inline[i] = v.data
			if inline[i] == nil {
				inline[i] = []byte{}
			}
		} else {
			segments[i] = encodeSegments(v.segments)
		}
	}
	// An existing row is locked, not rewritten.
	_, err = tx.Exec(ctx, `INSERT INTO file_versions (org, blob_sha, size, inline, segments) SELECT $1, * FROM unnest($2::bytea[], $3::bigint[], $4::bytea[], $5::bytea[])
		ON CONFLICT (org, blob_sha) DO UPDATE SET size = EXCLUDED.size WHERE false`, s.db.Org, shas, sizes, inline, segments)
	if err != nil {
		return nil, err
	}
	return fresh, nil
}

// recheckStaged locks the rows of the blocks in staged that fresh versions
// use, and forgets those whose rows are gone: a purge removes a block no
// stored version uses at once, even one a write put just before, so the
// write puts it again instead of storing a version whose block is missing.
func recheckStaged(ctx context.Context, tx pgx.Tx, org string, fresh []*version, staged map[[32]byte]bool) error {
	var keys [][]byte
	listed := map[[32]byte]bool{}
	for _, v := range fresh {
		for _, r := range v.refs {
			if staged[r.sha] && !listed[r.sha] {
				listed[r.sha] = true
				keys = append(keys, bytes.Clone(r.sha[:]))
			}
		}
	}
	if len(keys) == 0 {
		return nil
	}
	slices.SortFunc(keys, bytes.Compare)
	rows, err := tx.Query(ctx, `SELECT block_sha FROM content_blocks WHERE org = $1 AND block_sha = ANY($2) ORDER BY block_sha FOR KEY SHARE`, org, keys)
	if err != nil {
		return err
	}
	present, err := pgx.CollectRows(rows, pgx.RowTo[[]byte])
	if err != nil {
		return err
	}
	for _, sha := range present {
		delete(listed, [32]byte(sha))
	}
	for sha := range listed {
		delete(staged, sha)
	}
	return nil
}

func (s *Store) putBlocks(ctx context.Context, refs []blockRef, blocks [][]byte) error {
	workers := min(4, len(blocks))
	errs := make([]error, len(blocks))
	var wg sync.WaitGroup
	next := make(chan int)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				errs[i] = blob.Write(ctx, s.blobs, blockKey(refs[i].sha), blocks[i])
			}
		}()
	}
	for i := range blocks {
		next <- i
	}
	close(next)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			return err
		}
		s.blocks.put(refs[i].sha, blocks[i], int64(len(blocks[i])))
	}
	return nil
}

// cacheVersion keeps the manifest of a version this process just stored, so
// the first read of it needs no query. Only a stored version's: the same
// content under another name may already be stored as other blocks.
func (s *Store) cacheVersion(v *version) {
	m := &manifest{size: int64(len(v.data))}
	if v.inline() {
		m.inline = bytes.Clone(v.data)
		if m.inline == nil {
			m.inline = []byte{}
		}
	} else {
		m.segments = v.segments
		m.blocks = v.refs
		var offset int64
		for _, r := range v.refs {
			m.offsets = append(m.offsets, offset)
			offset += int64(r.size)
		}
	}
	s.manifests.put(v.blob, m, m.bytes())
}

// deleteUnusedVersions removes the file versions among blobs that no
// revision change references any more. It locks the candidates before
// checking, so a write that references one either finishes first and keeps
// it, or waits and stores it again. The segments and blocks of removed versions
// count as touched now, so the sweep leaves them for a write that reuses them.
func deleteUnusedVersions(ctx context.Context, tx pgx.Tx, org string, blobs [][]byte) error {
	if len(blobs) == 0 {
		return nil
	}
	rows, err := tx.Query(ctx, `SELECT blob_sha, segments FROM file_versions WHERE org = $1 AND blob_sha = ANY($2) ORDER BY blob_sha FOR UPDATE`, org, blobs)
	if err != nil {
		return err
	}
	candidates := map[gitobj.ID][]byte{}
	for rows.Next() {
		var sha, segments []byte
		if err := rows.Scan(&sha, &segments); err != nil {
			rows.Close()
			return err
		}
		candidates[toID(sha)] = segments
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	rows, err = tx.Query(ctx, `SELECT DISTINCT blob_sha FROM revision_changes WHERE org = $1 AND blob_sha = ANY($2)`, org, blobs)
	if err != nil {
		return err
	}
	used, err := pgx.CollectRows(rows, pgx.RowTo[[]byte])
	if err != nil {
		return err
	}
	for _, sha := range used {
		delete(candidates, toID(sha))
	}
	if len(candidates) == 0 {
		return nil
	}
	var doomed, segments [][]byte
	for id, raw := range candidates {
		doomed = append(doomed, bytes.Clone(id[:]))
		refs, err := decodeSegments(raw)
		if err != nil {
			return err
		}
		for _, seg := range refs {
			segments = append(segments, bytes.Clone(seg.sha[:]))
		}
	}
	if _, err = tx.Exec(ctx, `DELETE FROM file_versions WHERE org = $1 AND blob_sha = ANY($2)`, org, doomed); err != nil {
		return err
	}
	if len(segments) == 0 {
		return nil
	}
	rows, err = tx.Query(ctx, `UPDATE content_segments SET touched = now() WHERE org = $1 AND segment_sha = ANY($2) RETURNING blocks`, org, segments)
	if err != nil {
		return err
	}
	bodies, err := pgx.CollectRows(rows, pgx.RowTo[[]byte])
	if err != nil {
		return err
	}
	var blocks [][]byte
	for _, body := range bodies {
		refs, err := decodeBlocks(body)
		if err != nil {
			return err
		}
		for _, r := range refs {
			blocks = append(blocks, bytes.Clone(r.sha[:]))
		}
	}
	_, err = tx.Exec(ctx, `UPDATE content_blocks SET touched = now() WHERE org = $1 AND block_sha = ANY($2)`, org, blocks)
	return err
}
