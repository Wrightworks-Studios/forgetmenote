package notestore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"time"

	"forgetmenote/internal/gitobj"

	"github.com/jackc/pgx/v5"
)

// The rows a rewrite writes, in chunks so no statement carries an unbounded
// array.
const rewriteChunk = 1000

func sortedIDs(ids []gitobj.ID) [][]byte {
	keys := make([][]byte, len(ids))
	for i, id := range ids {
		keys[i] = bytesOf(id)
	}
	slices.SortFunc(keys, bytes.Compare)
	return keys
}

// storeVersions stores the rewrite's new content. The blocks the rewrite put
// are locked against removal first; one that a purge of other content
// removed meanwhile fails the rewrite, which can run again.
func (r *rewriter) storeVersions(ctx context.Context, tx pgx.Tx) error {
	if len(r.versions) == 0 {
		return nil
	}
	org := r.n.s.db.Org
	if len(r.staged) > 0 {
		keys := make([][]byte, 0, len(r.staged))
		for sha := range r.staged {
			keys = append(keys, bytes.Clone(sha[:]))
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
		if len(present) != len(keys) {
			return fmt.Errorf("%w: content blocks the purge stored were removed meanwhile; run it again", ErrConflict)
		}
	}
	segments := map[[32]byte][]byte{}
	var blobs, inline, lists [][]byte
	var sizes []int64
	for _, id := range slices.SortedFunc(maps.Keys(r.versions), func(a, b gitobj.ID) int { return bytes.Compare(a[:], b[:]) }) {
		v := r.versions[id]
		blobs, sizes = append(blobs, bytesOf(v.blob)), append(sizes, v.size)
		if v.inline != nil {
			inline, lists = append(inline, v.inline), append(lists, nil)
			continue
		}
		inline, lists = append(inline, nil), append(lists, encodeSegments(v.segments))
		for i, seg := range v.segments {
			segments[seg.sha] = v.bodies[i]
		}
	}
	var keys, bodies [][]byte
	for _, sha := range slices.SortedFunc(maps.Keys(segments), func(a, b [32]byte) int { return bytes.Compare(a[:], b[:]) }) {
		keys, bodies = append(keys, bytes.Clone(sha[:])), append(bodies, segments[sha])
	}
	for start := 0; start < len(keys); start += rewriteChunk {
		end := min(start+rewriteChunk, len(keys))
		if _, err := tx.Exec(ctx, `INSERT INTO content_segments (org, segment_sha, blocks) SELECT $1, * FROM unnest($2::bytea[], $3::bytea[])
			ON CONFLICT (org, segment_sha) DO UPDATE SET touched = now()`, org, keys[start:end], bodies[start:end]); err != nil {
			return err
		}
	}
	for start := 0; start < len(blobs); start += rewriteChunk {
		end := min(start+rewriteChunk, len(blobs))
		// An existing row is locked, not rewritten.
		if _, err := tx.Exec(ctx, `INSERT INTO file_versions (org, blob_sha, size, inline, segments) SELECT $1, * FROM unnest($2::bytea[], $3::bigint[], $4::bytea[], $5::bytea[])
			ON CONFLICT (org, blob_sha) DO UPDATE SET size = EXCLUDED.size WHERE false`, org, blobs[start:end], sizes[start:end], inline[start:end], lists[start:end]); err != nil {
			return err
		}
	}
	return nil
}

func insertTrees(ctx context.Context, tx pgx.Tx, org, notebook string, trees map[gitobj.ID][]byte) error {
	ids := slices.SortedFunc(maps.Keys(trees), func(a, b gitobj.ID) int { return bytes.Compare(a[:], b[:]) })
	for start := 0; start < len(ids); start += rewriteChunk {
		var shas, bodies [][]byte
		for _, id := range ids[start:min(start+rewriteChunk, len(ids))] {
			body := trees[id]
			if body == nil {
				body = []byte{}
			}
			shas, bodies = append(shas, bytesOf(id)), append(bodies, body)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO git_trees (org, notebook, tree_sha, body) SELECT $1, $2, * FROM unnest($3::bytea[], $4::bytea[]) ON CONFLICT DO NOTHING`,
			org, notebook, shas, bodies); err != nil {
			return err
		}
	}
	return nil
}

// insertRevisions stores revs and their changes, with the columns Apply
// derives from each commit.
func insertRevisions(ctx context.Context, tx pgx.Tx, org, notebook string, revs []newRevision) error {
	for start := 0; start < len(revs); start += rewriteChunk {
		var seqs []int64
		var parents []*int64
		var shas, trees, bodies [][]byte
		var times []time.Time
		var names, emails []string
		var users, sources, clients, operations, requests []*string
		for _, rev := range revs[start:min(start+rewriteChunk, len(revs))] {
			fields := map[string]*string{}
			for _, t := range gitobj.Trailers(rev.commit.Message) {
				value := t.Value
				fields[t.Key] = &value
			}
			var parent *int64
			if rev.parentSeq > 0 {
				p := rev.parentSeq
				parent = &p
			}
			seqs, parents = append(seqs, rev.seq), append(parents, parent)
			shas, trees, bodies = append(shas, bytesOf(rev.id)), append(trees, bytesOf(rev.commit.Tree)), append(bodies, rev.body)
			times = append(times, rev.commit.Committer.Time())
			names, emails = append(names, rev.commit.Author.Name), append(emails, rev.commit.Author.Email)
			users, sources, clients = append(users, fields["Gitnotes-User"]), append(sources, fields["Gitnotes-Source"]), append(clients, fields["Gitnotes-Client"])
			operations, requests = append(operations, fields[OperationTrailer]), append(requests, fields[RequestTrailer])
		}
		if _, err := tx.Exec(ctx, `INSERT INTO revisions (org, notebook, seq, commit_sha, parent_seq, tree_sha, commit_body, committed_at, author_name, author_email, actor, source, client, operation_id, request_id)
			SELECT $1, $2, * FROM unnest($3::bigint[], $4::bytea[], $5::bigint[], $6::bytea[], $7::bytea[], $8::timestamptz[], $9::text[], $10::text[], $11::text[], $12::text[], $13::text[], $14::text[], $15::text[])`,
			org, notebook, seqs, shas, parents, trees, bodies, times, names, emails, users, sources, clients, operations, requests); err != nil {
			return err
		}
	}
	var seqs, sizes []int64
	var entries, ops, paths, oldPaths []string
	var blobs [][]byte
	var modes []int32
	flush := func() error {
		if len(seqs) == 0 {
			return nil
		}
		_, err := tx.Exec(ctx, `INSERT INTO revision_changes (org, notebook, seq, entry_id, op, path, old_path, blob_sha, mode, size)
			SELECT $1, $2, s, e, o, p, NULLIF(op_, ''), bl, m, sz FROM unnest($3::bigint[], $4::text[], $5::text[], $6::text[], $7::text[], $8::bytea[], $9::integer[], $10::bigint[]) AS u(s, e, o, p, op_, bl, m, sz)`,
			org, notebook, seqs, entries, ops, paths, oldPaths, blobs, modes, sizes)
		seqs, sizes, entries, ops, paths, oldPaths, blobs, modes = nil, nil, nil, nil, nil, nil, nil, nil
		return err
	}
	for _, rev := range revs {
		for _, c := range rev.changes {
			var blob []byte
			if c.Op != OpDelete {
				blob = idOrNil(c.Blob)
			}
			seqs, sizes = append(seqs, rev.seq), append(sizes, c.Size)
			entries, ops, paths, oldPaths = append(entries, c.EntryID), append(ops, c.Op), append(paths, c.Path), append(oldPaths, c.OldPath)
			blobs, modes = append(blobs, blob), append(modes, int32(c.Mode))
			if len(seqs) == 2*rewriteChunk {
				if err := flush(); err != nil {
					return err
				}
			}
		}
	}
	return flush()
}

func insertRewrites(ctx context.Context, tx pgx.Tx, org, notebook, purge string, revs []newRevision) error {
	for start := 0; start < len(revs); start += rewriteChunk {
		chunk := revs[start:min(start+rewriteChunk, len(revs))]
		if _, err := tx.Exec(ctx, `INSERT INTO revision_rewrites (org, notebook, old_sha, new_sha, purge_id) SELECT $1, $2, o, n, $3 FROM unnest($4::bytea[], $5::bytea[]) AS m(o, n)`,
			org, notebook, purge, oldIDs(chunk), newIDs(chunk)); err != nil {
			return err
		}
	}
	return nil
}

// remapReceipts points the notebook's commit receipts at the commits that
// replaced theirs, and drops from them the files the rewrite dropped, so a
// retried change set answers with commits that exist and names no purged
// file.
func (r *rewriter) remapReceipts(ctx context.Context, tx pgx.Tx) error {
	org := r.n.s.db.Org
	rows, err := tx.Query(ctx, `SELECT operation_id, result FROM operation_receipts WHERE org = $1 AND notebook = $2 AND kind = $3 AND result IS NOT NULL`, org, r.n.id, commitReceipt)
	if err != nil {
		return err
	}
	var ops []string
	var results [][]byte
	for rows.Next() {
		var op string
		var raw []byte
		if err := rows.Scan(&op, &raw); err != nil {
			rows.Close()
			return err
		}
		var result CommitResult
		if json.Unmarshal(raw, &result) != nil {
			continue
		}
		changed := false
		if id, ok := r.mapping[result.Head.Commit]; ok {
			result.Head.Commit, changed = id, true
		}
		for p, v := range result.Versions {
			if r.dropped[droppedFile{v, p}] {
				delete(result.Versions, p)
				changed = true
			} else if id, ok := r.mapping[v]; ok {
				result.Versions[p], changed = id, true
			}
		}
		if changed {
			next, err := json.Marshal(result)
			if err != nil {
				rows.Close()
				return err
			}
			ops, results = append(ops, op), append(results, next)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for start := 0; start < len(ops); start += rewriteChunk {
		end := min(start+rewriteChunk, len(ops))
		if _, err := tx.Exec(ctx, `UPDATE operation_receipts o SET result = u.result FROM unnest($4::text[], $5::bytea[]) AS u(op, result)
			WHERE o.org = $1 AND o.notebook = $2 AND o.kind = $3 AND o.operation_id = u.op`, org, r.n.id, commitReceipt, ops[start:end], results[start:end]); err != nil {
			return err
		}
	}
	return nil
}

func oldIDs(revs []newRevision) [][]byte {
	out := make([][]byte, len(revs))
	for i, rev := range revs {
		out[i] = bytesOf(rev.old)
	}
	return out
}

func newIDs(revs []newRevision) [][]byte {
	out := make([][]byte, len(revs))
	for i, rev := range revs {
		out[i] = bytesOf(rev.id)
	}
	return out
}

// resetLive starts a new epoch for the live documents of entries, dropping
// their updates and snapshots as LiveReset does, and returns the entries that
// had one.
func resetLive(ctx context.Context, tx pgx.Tx, org, notebook string, entries []string) ([]string, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	rows, err := tx.Query(ctx, `UPDATE live_docs SET epoch = epoch + 1, snapshot = NULL, snapshot_seq = last_seq, base_version = NULL, updated = now()
		WHERE org = $1 AND notebook = $2 AND entry_id = ANY($3) RETURNING entry_id`, org, notebook, entries)
	if err != nil {
		return nil, err
	}
	reset, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM live_updates WHERE org = $1 AND notebook = $2 AND entry_id = ANY($3)`, org, notebook, entries); err != nil {
		return nil, err
	}
	slices.Sort(reset)
	return reset, nil
}

// collectTrees deletes the notebook's trees that no revision reaches any
// more: it marks every tree under the rewritten revisions and the revisions
// before them, and removes the rest.
func (r *rewriter) collectTrees(ctx context.Context, tx pgx.Tx, revs []newRevision) ([]gitobj.ID, error) {
	s, n := r.n.s, r.n
	rows, err := tx.Query(ctx, `SELECT tree_sha FROM revisions WHERE org = $1 AND notebook = $2 AND seq < $3`, s.db.Org, n.id, r.from)
	if err != nil {
		return nil, err
	}
	roots, err := pgx.CollectRows(rows, pgx.RowTo[[]byte])
	if err != nil {
		return nil, err
	}
	marked := map[gitobj.ID]bool{gitobj.EmptyTree: true}
	var level []gitobj.ID
	for _, rev := range revs {
		level = append(level, rev.commit.Tree)
	}
	for _, root := range roots {
		level = append(level, toID(root))
	}
	for len(level) > 0 {
		var next, fetch []gitobj.ID
		visit := func(entries []gitobj.TreeEntry) {
			for _, e := range entries {
				if e.IsTree() && !marked[e.ID] {
					next = append(next, e.ID)
				}
			}
		}
		for _, id := range level {
			if marked[id] {
				continue
			}
			marked[id] = true
			if entries, ok := r.known[id]; ok {
				visit(entries)
			} else {
				fetch = append(fetch, id)
			}
		}
		for start := 0; start < len(fetch); start += rewriteChunk {
			found, missing, err := s.findTrees(ctx, tx, n.id, fetch[start:min(start+rewriteChunk, len(fetch))])
			if err != nil {
				return nil, err
			}
			if len(missing) > 0 {
				return nil, fmt.Errorf("notebook %s: tree %s is missing", n.id, missing[0])
			}
			for _, entries := range found {
				visit(entries)
			}
		}
		level = next
	}
	rows, err = tx.Query(ctx, `SELECT tree_sha FROM git_trees WHERE org = $1 AND notebook = $2`, s.db.Org, n.id)
	if err != nil {
		return nil, err
	}
	all, err := pgx.CollectRows(rows, pgx.RowTo[[]byte])
	if err != nil {
		return nil, err
	}
	var doomed []gitobj.ID
	for _, sha := range all {
		if id := toID(sha); !marked[id] {
			doomed = append(doomed, id)
		}
	}
	keys := sortedIDs(doomed)
	for start := 0; start < len(keys); start += rewriteChunk {
		if _, err := tx.Exec(ctx, `DELETE FROM git_trees WHERE org = $1 AND notebook = $2 AND tree_sha = ANY($3)`, s.db.Org, n.id, keys[start:min(start+rewriteChunk, len(keys))]); err != nil {
			return nil, err
		}
	}
	return doomed, nil
}

// lockVersions locks the stored file versions among blobs against writers
// that would use them, and returns those that exist with their segments.
func lockVersions(ctx context.Context, tx pgx.Tx, org string, blobs []gitobj.ID) (map[gitobj.ID][]byte, error) {
	keys := sortedIDs(blobs)
	out := map[gitobj.ID][]byte{}
	for start := 0; start < len(keys); start += rewriteChunk {
		rows, err := tx.Query(ctx, `SELECT blob_sha, segments FROM file_versions WHERE org = $1 AND blob_sha = ANY($2) ORDER BY blob_sha FOR UPDATE`, org, keys[start:min(start+rewriteChunk, len(keys))])
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var sha, segments []byte
			if err := rows.Scan(&sha, &segments); err != nil {
				rows.Close()
				return nil, err
			}
			out[toID(sha)] = segments
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// deleteVersions deletes the locked file versions that no revision of any
// notebook uses any more, and returns their segments and how many went.
func deleteVersions(ctx context.Context, tx pgx.Tx, org string, locked map[gitobj.ID][]byte) ([][32]byte, int, error) {
	candidates := maps.Clone(locked)
	keys := sortedIDs(slices.Collect(maps.Keys(candidates)))
	for start := 0; start < len(keys); start += rewriteChunk {
		rows, err := tx.Query(ctx, `SELECT DISTINCT blob_sha FROM revision_changes WHERE org = $1 AND blob_sha = ANY($2)`, org, keys[start:min(start+rewriteChunk, len(keys))])
		if err != nil {
			return nil, 0, err
		}
		used, err := pgx.CollectRows(rows, pgx.RowTo[[]byte])
		if err != nil {
			return nil, 0, err
		}
		for _, sha := range used {
			delete(candidates, toID(sha))
		}
	}
	seen := map[[32]byte]bool{}
	var segments [][32]byte
	for _, raw := range candidates {
		refs, err := decodeSegments(raw)
		if err != nil {
			return nil, 0, err
		}
		for _, seg := range refs {
			if !seen[seg.sha] {
				seen[seg.sha] = true
				segments = append(segments, seg.sha)
			}
		}
	}
	doomed := sortedIDs(slices.Collect(maps.Keys(candidates)))
	for start := 0; start < len(doomed); start += rewriteChunk {
		if _, err := tx.Exec(ctx, `DELETE FROM file_versions WHERE org = $1 AND blob_sha = ANY($2)`, org, doomed[start:min(start+rewriteChunk, len(doomed))]); err != nil {
			return nil, 0, err
		}
	}
	return segments, len(doomed), nil
}

// purgeSegments deletes segments, and their blocks in the blob store, that
// no file version uses any more, at once rather than after the sweep's grace
// period. It locks blocks before segments, in the order every writer locks
// them; a writer that put a block it is about to use finds the block's row
// gone, and puts it again.
func (s *Store) purgeSegments(ctx context.Context, segments [][32]byte) (int, int, error) {
	if len(segments) == 0 {
		return 0, 0, nil
	}
	org := s.db.Org
	keys := make([][]byte, len(segments))
	for i, sha := range segments {
		keys[i] = bytes.Clone(sha[:])
	}
	slices.SortFunc(keys, bytes.Compare)
	var removedSegments int
	var gone [][32]byte
	err := pgx.BeginFunc(ctx, s.db.Pool, func(tx pgx.Tx) error {
		removedSegments, gone = 0, nil
		rows, err := tx.Query(ctx, `SELECT segment_sha, blocks FROM content_segments WHERE org = $1 AND segment_sha = ANY($2)`, org, keys)
		if err != nil {
			return err
		}
		bodies := map[[32]byte][]byte{}
		for rows.Next() {
			var sha, body []byte
			if err := rows.Scan(&sha, &body); err != nil {
				rows.Close()
				return err
			}
			bodies[[32]byte(sha)] = body
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		candidates := map[[32]byte]bool{}
		for _, body := range bodies {
			if err := keepBlocks(candidates, body); err != nil {
				return err
			}
		}
		blockKeys := make([][]byte, 0, len(candidates))
		for sha := range candidates {
			blockKeys = append(blockKeys, bytes.Clone(sha[:]))
		}
		slices.SortFunc(blockKeys, bytes.Compare)
		if _, err := tx.Exec(ctx, `SELECT FROM content_blocks WHERE org = $1 AND block_sha = ANY($2) ORDER BY block_sha FOR UPDATE`, org, blockKeys); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT FROM content_segments WHERE org = $1 AND segment_sha = ANY($2) ORDER BY segment_sha FOR UPDATE`, org, keys); err != nil {
			return err
		}
		// What every file version and segment uses now, read with the
		// candidates locked, so a write that uses one has committed.
		rows, err = tx.Query(ctx, `SELECT segments FROM file_versions WHERE org = $1 AND segments IS NOT NULL`, org)
		if err != nil {
			return err
		}
		used := map[[32]byte]bool{}
		for rows.Next() {
			var raw []byte
			if err := rows.Scan(&raw); err != nil {
				rows.Close()
				return err
			}
			refs, err := decodeSegments(raw)
			if err != nil {
				rows.Close()
				return err
			}
			for _, seg := range refs {
				used[seg.sha] = true
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		doomed := [][]byte{}
		inDoomed := map[[32]byte]bool{}
		for _, key := range keys {
			if sha := [32]byte(key); !used[sha] {
				doomed = append(doomed, key)
				if body, ok := bodies[sha]; ok {
					if err := keepBlocks(inDoomed, body); err != nil {
						return err
					}
				}
			}
		}
		if len(doomed) == 0 {
			return nil
		}
		rows, err = tx.Query(ctx, `SELECT blocks FROM content_segments WHERE org = $1 AND segment_sha <> ALL($2)`, org, doomed)
		if err != nil {
			return err
		}
		keep := map[[32]byte]bool{}
		for rows.Next() {
			var body []byte
			if err := rows.Scan(&body); err != nil {
				rows.Close()
				return err
			}
			if err := keepBlocks(keep, body); err != nil {
				rows.Close()
				return err
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		blocks := [][]byte{}
		for _, key := range blockKeys {
			if sha := [32]byte(key); inDoomed[sha] && !keep[sha] {
				blocks = append(blocks, key)
				gone = append(gone, sha)
			}
		}
		tag, err := tx.Exec(ctx, `DELETE FROM content_segments WHERE org = $1 AND segment_sha = ANY($2)`, org, doomed)
		if err != nil {
			return err
		}
		removedSegments = int(tag.RowsAffected())
		if _, err := tx.Exec(ctx, `DELETE FROM content_blocks WHERE org = $1 AND block_sha = ANY($2)`, org, blocks); err != nil {
			return err
		}
		// The objects go while their rows are locked; if the transaction then
		// fails, its rows name blocks no file version uses, which the sweep
		// removes.
		for _, sha := range gone {
			if err := s.blobs.Delete(ctx, blockKey(sha)); err != nil {
				return err
			}
		}
		return nil
	})
	for _, sha := range gone {
		s.blocks.remove(sha)
	}
	if err != nil {
		return 0, 0, err
	}
	return removedSegments, len(gone), nil
}
