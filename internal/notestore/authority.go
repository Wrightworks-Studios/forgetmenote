package notestore

import (
	"context"
	"errors"
	"fmt"

	"forgetmenote/internal/gitobj"

	"github.com/jackc/pgx/v5"
)

// An Authority is a notebook's authority handoff record, which the workspace
// layer keeps here and gives its meaning: State is its JSON receipt, and a
// stage kept with it the snapshot a prepared handoff installs. Version is 0
// when the notebook has no record.
type Authority struct {
	Version int64
	State   []byte
	Staged  bool
}

// Authority returns the notebook's authority record, without its stage.
func (n *Notebook) Authority(ctx context.Context) (Authority, error) {
	var a Authority
	var state string
	err := n.s.db.Pool.QueryRow(ctx, `SELECT version, state::text, stage IS NOT NULL FROM notebook_authority WHERE org = $1 AND notebook = $2`,
		n.s.db.Org, n.id).Scan(&a.Version, &state, &a.Staged)
	if errors.Is(err, pgx.ErrNoRows) {
		return Authority{}, nil
	}
	if err != nil {
		return Authority{}, err
	}
	a.State = []byte(state)
	return a, nil
}

// AuthorityStage returns the stage kept with the record at version, nil when
// it has none. It fails with ErrConflict once the record has moved on.
func (n *Notebook) AuthorityStage(ctx context.Context, version int64) ([]byte, error) {
	var stage []byte
	err := n.s.db.Pool.QueryRow(ctx, `SELECT stage FROM notebook_authority WHERE org = $1 AND notebook = $2 AND version = $3`,
		n.s.db.Org, n.id, version).Scan(&stage)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrAuthorityChanged
	}
	return stage, err
}

// An AuthorityWrite replaces the record at Version, 0 when there is none yet,
// with State and Stage; a nil Stage drops the stage.
type AuthorityWrite struct {
	Version int64
	State   []byte
	Stage   []byte
	// ExpectHead, when set, is the head the notebook must still have. No
	// revision is written between that check and the new record.
	ExpectHead *gitobj.ID
	// RefusePending, when set, is a receipt kind: the record is not stored
	// while the notebook has a pending receipt of that kind, and PutAuthority
	// fails with ErrPending. A receipt that SettleReceipt fences on the
	// record cannot become pending meanwhile.
	RefusePending string
	// ExpectLive, when set, is the sum of the notebook's live documents'
	// sequence numbers (LiveDoc.LastSeq) as the writer saw them: a live
	// update stored since fails PutAuthority with ErrLiveChanged. Every
	// update raises the sum, and one stored after the record reads it
	// (LiveAppendFenced).
	ExpectLive *int64
}

// ErrAuthorityChanged reports a write fenced on a notebook's authority
// record that another write has replaced; it matches ErrConflict.
var ErrAuthorityChanged = fmt.Errorf("%w: the notebook's authority record changed", ErrConflict)

// ErrLiveChanged reports an authority record refused because a live update
// was stored after its writer looked (AuthorityWrite.ExpectLive); it
// matches ErrConflict.
var ErrLiveChanged = fmt.Errorf("%w: a live update was stored meanwhile", ErrConflict)

// ErrPending reports operations still pending that refuse a write.
var ErrPending = errors.New("notestore: operations are still pending")

// authorityVersion reads the version of notebook's authority record, 0 when
// it has none.
func authorityVersion(ctx context.Context, q querier, org, notebook string) (int64, error) {
	var version int64
	err := q.QueryRow(ctx, `SELECT version FROM notebook_authority WHERE org = $1 AND notebook = $2`, org, notebook).Scan(&version)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return version, err
}

// PutAuthority stores w and returns the record's new version. A record that
// changed since w.Version, or a head other than w.ExpectHead, fails it with
// an error matching ErrConflict, and a notebook that does not exist with one
// matching fs.ErrNotExist.
func (n *Notebook) PutAuthority(ctx context.Context, w AuthorityWrite) (int64, error) {
	org := n.s.db.Org
	var version int64
	err := pgx.BeginFunc(ctx, n.s.db.Pool, func(tx pgx.Tx) error {
		// Writers lock the head row before anything else, so holding it
		// shared keeps every revision out until the record is stored.
		head, _, err := scanHead(tx.QueryRow(ctx, `SELECT seq, commit_sha, incarnation FROM notebook_heads WHERE org = $1 AND notebook = $2 FOR SHARE`, org, n.id), n.id)
		if err != nil {
			return err
		}
		if w.ExpectHead != nil && head.Commit != *w.ExpectHead {
			return &ConflictError{Expected: *w.ExpectHead, Actual: head.Commit}
		}
		if w.RefusePending != "" {
			var pending bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM operation_receipts WHERE org = $1 AND notebook = $2 AND kind = $3 AND done IS NULL)`,
				org, n.id, w.RefusePending).Scan(&pending); err != nil {
				return err
			}
			if pending {
				return fmt.Errorf("%w: %s", ErrPending, w.RefusePending)
			}
		}
		if w.ExpectLive != nil {
			// Live updates lock the head row too: this sees each one stored
			// before the record.
			var live int64
			if err := tx.QueryRow(ctx, `SELECT coalesce(sum(last_seq), 0)::bigint FROM live_docs WHERE org = $1 AND notebook = $2`, org, n.id).Scan(&live); err != nil {
				return err
			}
			if live != *w.ExpectLive {
				return ErrLiveChanged
			}
		}
		if w.Version == 0 {
			err = tx.QueryRow(ctx, `INSERT INTO notebook_authority (org, notebook, version, state, stage) VALUES ($1, $2, 1, $3::jsonb, $4)
				ON CONFLICT DO NOTHING RETURNING version`, org, n.id, string(w.State), w.Stage).Scan(&version)
		} else {
			err = tx.QueryRow(ctx, `UPDATE notebook_authority SET version = version + 1, state = $4::jsonb, stage = $5, updated = now()
				WHERE org = $1 AND notebook = $2 AND version = $3 RETURNING version`, org, n.id, w.Version, string(w.State), w.Stage).Scan(&version)
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrAuthorityChanged
		}
		return err
	})
	return version, err
}
