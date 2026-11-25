package notestore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"time"

	"forgetmenote/internal/gitobj"

	"github.com/jackc/pgx/v5"
)

// A RewriteRecord says where purges took a commit: the commit that now stands
// for it, after every later purge, and the purges that took it there.
type RewriteRecord struct {
	Commit gitobj.ID
	Purges []string
}

// Rewritten returns what commit old became when purges rewrote the
// notebook's history; ok is false when no purge rewrote it. References to old
// commits, such as citations and history pins, resolve through it.
func (n *Notebook) Rewritten(ctx context.Context, old gitobj.ID) (RewriteRecord, bool, error) {
	id := old
	var purges []string
	// A commit one purge made can be rewritten by a later one; the chain
	// cannot be longer than the purges the notebook had.
	for range 10000 {
		var next []byte
		var purge string
		err := n.s.db.Pool.QueryRow(ctx, `SELECT new_sha, purge_id FROM revision_rewrites WHERE org = $1 AND notebook = $2 AND old_sha = $3`,
			n.s.db.Org, n.id, id[:]).Scan(&next, &purge)
		if errors.Is(err, pgx.ErrNoRows) {
			break
		}
		if err != nil {
			return RewriteRecord{}, false, err
		}
		purges = append(purges, purge)
		if next == nil {
			id = gitobj.ID{}
			break
		}
		id = toID(next)
	}
	if len(purges) == 0 {
		return RewriteRecord{}, false, nil
	}
	return RewriteRecord{Commit: id, Purges: purges}, true, nil
}

// RewrittenAll maps every commit purges rewrote to the commit that stands
// for it now, after every later purge, as Rewritten finds it: the zero ID
// for one a purge dropped. A notebook never purged has none.
func (n *Notebook) RewrittenAll(ctx context.Context) (map[gitobj.ID]gitobj.ID, error) {
	rows, err := n.s.db.Pool.Query(ctx, `SELECT old_sha, new_sha FROM revision_rewrites WHERE org = $1 AND notebook = $2`, n.s.db.Org, n.id)
	if err != nil {
		return nil, err
	}
	next := map[gitobj.ID]gitobj.ID{}
	for rows.Next() {
		var old, replaced []byte
		if err := rows.Scan(&old, &replaced); err != nil {
			rows.Close()
			return nil, err
		}
		next[toID(old)] = gitobj.ID{}
		if replaced != nil {
			next[toID(old)] = toID(replaced)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// A commit one purge made can be rewritten by a later one; the chain
	// cannot be longer than the purges the notebook had.
	out := make(map[gitobj.ID]gitobj.ID, len(next))
	for old, id := range next {
		for range len(next) {
			later, ok := next[id]
			if !ok || id.IsZero() {
				break
			}
			id = later
		}
		out[old] = id
	}
	return out, nil
}

// Original returns the commit that purge replaced with commit id; ok is
// false when id is not one of purge's replacements.
func (n *Notebook) Original(ctx context.Context, id gitobj.ID, purge string) (gitobj.ID, bool, error) {
	var old []byte
	err := n.s.db.Pool.QueryRow(ctx, `SELECT old_sha FROM revision_rewrites WHERE org = $1 AND notebook = $2 AND new_sha = $3 AND purge_id = $4`,
		n.s.db.Org, n.id, id[:], purge).Scan(&old)
	if errors.Is(err, pgx.ErrNoRows) {
		return gitobj.ID{}, false, nil
	}
	return toID(old), err == nil, err
}

// A PurgeRecord is what the purges table keeps of a purge. FromSeq is the
// first revision it rewrote, or 0 for a purge that rewrote none.
type PurgeRecord struct {
	ID, Actor, Reason string
	Scope             json.RawMessage
	FromSeq           int64
	Created           time.Time
}

// PurgeLive runs a purge that rewrites no revision, as one of content that
// only live documents hold: one transaction, holding the head row's lock,
// records it and resets the live documents of o.ResetLive. o says who ran
// it, why and what it covered, as for Rewrite, and o.ExpectHead the head it
// must find; its Message is unused. The result's head is the current one.
func (n *Notebook) PurgeLive(ctx context.Context, purge string, o RewriteOptions) (RewriteResult, error) {
	if !purgeID.MatchString(purge) {
		return RewriteResult{}, invalid("purge ID %q", purge)
	}
	if strings.TrimSpace(o.Actor) == "" || strings.TrimSpace(o.Reason) == "" {
		return RewriteResult{}, invalid("a purge needs an actor and a reason")
	}
	if len(o.ResetLive) == 0 {
		return RewriteResult{}, invalid("a purge of live documents needs the entries to reset")
	}
	for _, entry := range o.ResetLive {
		if !validEntryID(entry) {
			return RewriteResult{}, invalid("entry ID %q", entry)
		}
	}
	scope, err := json.Marshal(o.Scope)
	if err != nil {
		return RewriteResult{}, err
	}
	org := n.s.db.Org
	var result RewriteResult
	err = pgx.BeginFunc(ctx, n.s.db.Pool, func(tx pgx.Tx) error {
		head, _, err := lockHead(ctx, tx, org, n.id)
		if err != nil {
			return err
		}
		if o.ExpectHead != nil && *o.ExpectHead != head.Commit {
			return &ConflictError{Expected: *o.ExpectHead, Actual: head.Commit}
		}
		if err := o.checkAuthority(ctx, tx, org, n.id); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `INSERT INTO purges (org, notebook, purge_id, actor, reason, scope, from_seq) VALUES ($1, $2, $3, $4, $5, $6, 0) ON CONFLICT DO NOTHING`,
			org, n.id, purge, o.Actor, o.Reason, scope)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return invalid("purge %s already ran", purge)
		}
		entries := slices.Clone(o.ResetLive)
		slices.Sort(entries)
		reset, err := resetLive(ctx, tx, org, n.id, slices.Compact(entries))
		result = RewriteResult{Head: head, LiveReset: reset}
		return err
	})
	if err != nil {
		return RewriteResult{}, err
	}
	return result, nil
}

// Purge returns the record of purge id, or an fs.ErrNotExist error.
func (n *Notebook) Purge(ctx context.Context, id string) (PurgeRecord, error) {
	p := PurgeRecord{ID: id}
	err := n.s.db.Pool.QueryRow(ctx, `SELECT actor, reason, scope, from_seq, created FROM purges WHERE org = $1 AND notebook = $2 AND purge_id = $3`,
		n.s.db.Org, n.id, id).Scan(&p.Actor, &p.Reason, &p.Scope, &p.FromSeq, &p.Created)
	if errors.Is(err, pgx.ErrNoRows) {
		return PurgeRecord{}, fmt.Errorf("purge %s: %w", id, fs.ErrNotExist)
	}
	return p, err
}

// SharedElsewhere counts, for each other notebook of the org, how many of
// blobs its history uses: content a purge of this notebook keeps stored for
// them, which its plan reports.
func (n *Notebook) SharedElsewhere(ctx context.Context, blobs []gitobj.ID) (map[string]int, error) {
	keys := make([][]byte, len(blobs))
	for i, b := range blobs {
		keys[i] = bytesOf(b)
	}
	slices.SortFunc(keys, bytes.Compare)
	keys = slices.CompactFunc(keys, bytes.Equal)
	out := map[string]int{}
	for start := 0; start < len(keys); start += rewriteChunk {
		rows, err := n.s.db.Pool.Query(ctx, `SELECT notebook, count(DISTINCT blob_sha) FROM revision_changes WHERE org = $1 AND notebook <> $2 AND blob_sha = ANY($3) GROUP BY notebook`,
			n.s.db.Org, n.id, keys[start:min(start+rewriteChunk, len(keys))])
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var notebook string
			var count int
			if err := rows.Scan(&notebook, &count); err != nil {
				rows.Close()
				return nil, err
			}
			out[notebook] += count
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// LiveEntries lists the entries that have a stored live document.
func (n *Notebook) LiveEntries(ctx context.Context) ([]string, error) {
	rows, err := n.s.db.Pool.Query(ctx, `SELECT entry_id FROM live_docs WHERE org = $1 AND notebook = $2 ORDER BY entry_id`, n.s.db.Org, n.id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}
