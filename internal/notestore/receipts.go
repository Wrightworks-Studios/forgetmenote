package notestore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"forgetmenote/internal/gitobj"

	"github.com/jackc/pgx/v5"
)

// A Receipt records an operation a client may retry: capture, group
// operations, live patches, or a change set's OperationID (kind "commit").
// RequestHash binds the operation ID to the request it answered.
type Receipt struct {
	Kind, Operation string
	RequestHash     string
	State           string
	Result          []byte
	Created, Done   time.Time
}

// Receipt states. A receipt is swept ReceiptTTL after it is done.
const (
	ReceiptPending = "pending"
	ReceiptDone    = "done"
	ReceiptTTL     = 30 * 24 * time.Hour
)

// Receipt returns the receipt of operation op of kind, if there is one.
func (n *Notebook) Receipt(ctx context.Context, kind, op string) (Receipt, bool, error) {
	return readReceipt(ctx, n.s.db.Pool, n.s.db.Org, n.id, kind, op)
}

func readReceipt(ctx context.Context, q querier, org, notebook, kind, op string) (Receipt, bool, error) {
	r := Receipt{Kind: kind, Operation: op}
	var done *time.Time
	err := q.QueryRow(ctx, `SELECT request_hash, state, result, created, done FROM operation_receipts WHERE org = $1 AND notebook = $2 AND kind = $3 AND operation_id = $4`,
		org, notebook, kind, op).Scan(&r.RequestHash, &r.State, &r.Result, &r.Created, &done)
	if errors.Is(err, pgx.ErrNoRows) {
		return Receipt{}, false, nil
	}
	if done != nil {
		r.Done = *done
	}
	return r, err == nil, err
}

// PutReceipt records r, or updates its state and result. It fails with
// ErrOperationMismatch when the operation already has a different request,
// or its receipt is done and r is not: a done receipt never becomes pending
// again.
func (n *Notebook) PutReceipt(ctx context.Context, r Receipt) error {
	if r.Kind == "" || r.Operation == "" || r.RequestHash == "" {
		return invalid("a receipt needs a kind, an operation and a request hash")
	}
	if r.State == "" {
		r.State = ReceiptPending
	}
	sql, args := receiptStatement(n.s.db.Org, n.id, r)
	tag, err := n.s.db.Pool.Exec(ctx, sql, args...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: %s %s", ErrOperationMismatch, r.Kind, r.Operation)
	}
	return nil
}

func queueReceipt(b *pgx.Batch, org, notebook string, r Receipt) {
	sql, args := receiptStatement(org, notebook, r)
	b.Queue(sql, args...)
}

// receiptStatement stores r unless its operation has another request, or
// its receipt is done and r is not.
func receiptStatement(org, notebook string, r Receipt) (string, []any) {
	return `INSERT INTO operation_receipts (org, notebook, kind, operation_id, request_hash, state, result, done)
		VALUES ($1, $2, $3, $4, $5, $6, $7, CASE WHEN $6 = 'done' THEN now() END)
		ON CONFLICT (org, notebook, kind, operation_id) DO UPDATE SET state = EXCLUDED.state, result = EXCLUDED.result,
			done = CASE WHEN EXCLUDED.state = 'done' THEN coalesce(operation_receipts.done, now()) END
		WHERE operation_receipts.request_hash = EXCLUDED.request_hash AND (operation_receipts.state <> 'done' OR EXCLUDED.state = 'done')`,
		[]any{org, notebook, r.Kind, r.Operation, r.RequestHash, r.State, r.Result}
}

// A Settlement is what SettleReceipt learns while it holds the notebook's
// head row.
type Settlement struct {
	// Committed is the newest revision whose Gitnotes-Operation trailer
	// names the operation, nil when there is none.
	Committed *CommitInfo
	// Authority is the version of the notebook's authority record, 0 when
	// it has none.
	Authority int64
}

// SettleReceipt stores the receipt settle makes, in one transaction that
// locks the notebook's head row as a writer does, so neither a revision nor
// an authority record (PutAuthority) lands between what settle learns and
// the receipt: a change set fenced on the receipt (ChangeSet.Pending)
// committed before, and settle sees its revision, or it finds the receipt
// settled and fails. An error from settle ends the transaction, storing
// nothing, and is returned; the receipt itself fails as PutReceipt's does.
// With operation empty, Committed stays nil.
func (n *Notebook) SettleReceipt(ctx context.Context, operation string, settle func(Settlement) (Receipt, error)) error {
	org := n.s.db.Org
	return pgx.BeginFunc(ctx, n.s.db.Pool, func(tx pgx.Tx) error {
		if _, _, err := lockHead(ctx, tx, org, n.id); err != nil {
			return err
		}
		var s Settlement
		if operation != "" {
			// A statement after the lock, so it sees a revision committed
			// while the lock was awaited.
			var info CommitInfo
			var sha, body []byte
			err := tx.QueryRow(ctx, `SELECT seq, commit_sha, commit_body FROM revisions WHERE org = $1 AND notebook = $2 AND operation_id = $3 ORDER BY seq DESC LIMIT 1`,
				org, n.id, operation).Scan(&info.Seq, &sha, &body)
			switch {
			case errors.Is(err, pgx.ErrNoRows):
			case err != nil:
				return err
			default:
				info.ID = toID(sha)
				if info.Commit, err = gitobj.ParseCommit(body); err != nil {
					return fmt.Errorf("revision %s: %w", info.ID, err)
				}
				info.Trailers = gitobj.Trailers(info.Commit.Message)
				s.Committed = &info
			}
		}
		var err error
		if s.Authority, err = authorityVersion(ctx, tx, org, n.id); err != nil {
			return err
		}
		r, err := settle(s)
		if err != nil {
			return err
		}
		if r.Kind == "" || r.Operation == "" || r.RequestHash == "" {
			return invalid("a receipt needs a kind, an operation and a request hash")
		}
		if r.State == "" {
			r.State = ReceiptPending
		}
		sql, args := receiptStatement(org, n.id, r)
		tag, err := tx.Exec(ctx, sql, args...)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("%w: %s %s", ErrOperationMismatch, r.Kind, r.Operation)
		}
		return nil
	})
}

// SweepReceipts deletes receipts done more than ReceiptTTL before now, and
// returns how many.
func (s *Store) SweepReceipts(ctx context.Context, now time.Time) (int64, error) {
	tag, err := s.db.Pool.Exec(ctx, `DELETE FROM operation_receipts WHERE org = $1 AND done < $2`, s.db.Org, now.Add(-ReceiptTTL))
	return tag.RowsAffected(), err
}
