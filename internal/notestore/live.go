package notestore

import (
	"context"
	"errors"
	"fmt"

	"forgetmenote/internal/gitobj"

	"github.com/jackc/pgx/v5"
)

// LiveState is a live document as stored: the engine's snapshot through
// SnapshotSeq, based on revision Base, and the updates after it. The bytes are
// the engine's (Yjs or Loro); the store never reads them. A document that
// never had an update has epoch 0 and nothing else.
type LiveState struct {
	Epoch       int64
	Base        gitobj.ID
	Snapshot    []byte
	SnapshotSeq int64
	Updates     []LiveUpdate
}

// A LiveUpdate is one stored engine update.
type LiveUpdate struct {
	Seq    int64
	Actor  string
	Update []byte
}

// LiveState returns the stored live document of entryID.
func (n *Notebook) LiveState(ctx context.Context, entryID string) (LiveState, error) {
	var st LiveState
	org := n.s.db.Org
	err := pgx.BeginTxFunc(ctx, n.s.db.Pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var base []byte
		err := tx.QueryRow(ctx, `SELECT epoch, snapshot, snapshot_seq, base_version FROM live_docs WHERE org = $1 AND notebook = $2 AND entry_id = $3`,
			org, n.id, entryID).Scan(&st.Epoch, &st.Snapshot, &st.SnapshotSeq, &base)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		st.Base = toID(base)
		rows, err := tx.Query(ctx, `SELECT seq, actor, body FROM live_updates WHERE org = $1 AND notebook = $2 AND entry_id = $3 AND seq > $4 ORDER BY seq`,
			org, n.id, entryID, st.SnapshotSeq)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var u LiveUpdate
			if err := rows.Scan(&u.Seq, &u.Actor, &u.Update); err != nil {
				return err
			}
			st.Updates = append(st.Updates, u)
		}
		return rows.Err()
	})
	return st, err
}

func liveConflict(entryID string) error {
	return &ConflictError{Path: entryID, Reason: "the live document's epoch changed"}
}

// LiveAppend stores update by actor and returns its sequence number. It fails
// with a ConflictError when the document's epoch is no longer epoch.
func (n *Notebook) LiveAppend(ctx context.Context, entryID string, epoch int64, update []byte, actor string) (int64, error) {
	return n.liveAppend(ctx, entryID, epoch, update, actor, nil)
}

// LiveAppendFenced is LiveAppend fenced on the notebook's authority record,
// as ChangeSet.ExpectAuthority fences a revision: unless the record is still
// at version authority, 0 for none, it stores nothing and fails with
// ErrAuthorityChanged.
func (n *Notebook) LiveAppendFenced(ctx context.Context, entryID string, epoch int64, update []byte, actor string, authority int64) (int64, error) {
	return n.liveAppend(ctx, entryID, epoch, update, actor, &authority)
}

func (n *Notebook) liveAppend(ctx context.Context, entryID string, epoch int64, update []byte, actor string, authority *int64) (int64, error) {
	if !validEntryID(entryID) {
		return 0, invalid("entry ID %q", entryID)
	}
	org := n.s.db.Org
	var seq int64
	err := pgx.BeginFunc(ctx, n.s.db.Pool, func(tx pgx.Tx) error {
		// An update locks the head row, in a mode PutAuthority's shared lock
		// keeps out, so an authority record is stored either before the
		// update, which then reads it below, or after the update is stored,
		// and sees it (AuthorityWrite.ExpectLive). The statements go in one
		// round trip, but each starts once the one before ended, so the
		// record is read after the lock is held.
		batch := &pgx.Batch{}
		batch.Queue(`SELECT seq, commit_sha, incarnation FROM notebook_heads WHERE org = $1 AND notebook = $2 FOR NO KEY UPDATE`, org, n.id)
		if epoch == 0 {
			batch.Queue(`INSERT INTO live_docs (org, notebook, entry_id, epoch) VALUES ($1, $2, $3, 0) ON CONFLICT DO NOTHING`, org, n.id, entryID)
		}
		batch.Queue(`WITH doc AS (
				UPDATE live_docs SET last_seq = last_seq + 1, updated = now() WHERE org = $1 AND notebook = $2 AND entry_id = $3 AND epoch = $4
					AND ($7::bigint IS NULL OR $7 = coalesce((SELECT version FROM notebook_authority WHERE org = $1 AND notebook = $2), 0))
				RETURNING last_seq)
			INSERT INTO live_updates (org, notebook, entry_id, seq, epoch, actor, body) SELECT $1, $2, $3, last_seq, $4, $5, $6 FROM doc RETURNING seq`,
			org, n.id, entryID, epoch, actor, update, authority)
		results := tx.SendBatch(ctx, batch)
		_, _, err := scanHead(results.QueryRow(), n.id)
		if err == nil && epoch == 0 {
			_, err = results.Exec()
		}
		if err == nil {
			err = results.QueryRow().Scan(&seq)
		}
		if closeErr := results.Close(); err == nil {
			err = closeErr
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		// Nothing was stored: the record or the epoch changed.
		if authority != nil {
			version, err := authorityVersion(ctx, tx, org, n.id)
			if err != nil {
				return err
			}
			if version != *authority {
				return ErrAuthorityChanged
			}
		}
		return liveConflict(entryID)
	})
	if err == nil {
		n.s.liveChanged(n.id, LiveChange{EntryID: entryID, Epoch: epoch, Seq: seq})
	}
	return seq, err
}

// LiveCompact replaces the updates through upto with snapshot, based on
// revision base.
func (n *Notebook) LiveCompact(ctx context.Context, entryID string, epoch int64, snapshot []byte, upto int64, base gitobj.ID) error {
	org := n.s.db.Org
	err := pgx.BeginFunc(ctx, n.s.db.Pool, func(tx pgx.Tx) error {
		var current, last, snapshotSeq int64
		err := tx.QueryRow(ctx, `SELECT epoch, last_seq, snapshot_seq FROM live_docs WHERE org = $1 AND notebook = $2 AND entry_id = $3 FOR UPDATE`,
			org, n.id, entryID).Scan(&current, &last, &snapshotSeq)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && current != epoch {
			return liveConflict(entryID)
		}
		if err != nil {
			return err
		}
		if upto > last || upto < snapshotSeq {
			return invalid("compaction through %d of updates %d..%d", upto, snapshotSeq, last)
		}
		if _, err = tx.Exec(ctx, `UPDATE live_docs SET snapshot = $4, snapshot_seq = $5, base_version = $6, updated = now() WHERE org = $1 AND notebook = $2 AND entry_id = $3`,
			org, n.id, entryID, snapshot, upto, idOrNil(base)); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `DELETE FROM live_updates WHERE org = $1 AND notebook = $2 AND entry_id = $3 AND seq <= $4`, org, n.id, entryID, upto)
		return err
	})
	if err == nil {
		n.s.liveChanged(n.id, LiveChange{EntryID: entryID, Epoch: epoch, Seq: upto, Base: base})
	}
	return err
}

// LiveReset starts a new epoch for entryID's live document, dropping every
// update and snapshot, as a purge or delete needs, and returns the epoch.
func (n *Notebook) LiveReset(ctx context.Context, entryID string) (int64, error) {
	if !validEntryID(entryID) {
		return 0, invalid("entry ID %q", entryID)
	}
	org := n.s.db.Org
	var epoch, seq int64
	err := pgx.BeginFunc(ctx, n.s.db.Pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `INSERT INTO live_docs (org, notebook, entry_id, epoch) VALUES ($1, $2, $3, 1)
			ON CONFLICT (org, notebook, entry_id) DO UPDATE SET epoch = live_docs.epoch + 1, snapshot = NULL, snapshot_seq = live_docs.last_seq, base_version = NULL, updated = now()
			RETURNING epoch, last_seq`, org, n.id, entryID).Scan(&epoch, &seq)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `DELETE FROM live_updates WHERE org = $1 AND notebook = $2 AND entry_id = $3`, org, n.id, entryID)
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("reset live document %s: %w", entryID, err)
	}
	n.s.liveChanged(n.id, LiveChange{EntryID: entryID, Epoch: epoch, Seq: seq})
	return epoch, nil
}
