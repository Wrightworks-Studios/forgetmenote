package notestore

import (
	"context"

	"github.com/jackc/pgx/v5"

	"forgetmenote/internal/pg"
)

// A RecordedPurge is a purge a notebook recorded in the transaction that
// rewrote its history or reset its live documents.
type RecordedPurge struct {
	Notebook string
	PurgeRecord
}

// RecordedPurges lists the purges db's org recorded, oldest first: those of
// notebook, or of every notebook when it is "". It needs no open Store, as a
// gateway's list of purge receipts and its recovery of a purge that stopped
// before it finished read it.
func RecordedPurges(ctx context.Context, db *pg.DB, notebook string) ([]RecordedPurge, error) {
	rows, err := db.Pool.Query(ctx, `SELECT notebook, purge_id, actor, reason, scope, from_seq, created FROM purges
		WHERE org = $1 AND ($2 = '' OR notebook = $2) ORDER BY created, notebook, purge_id`, db.Org, notebook)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (RecordedPurge, error) {
		var p RecordedPurge
		err := row.Scan(&p.Notebook, &p.ID, &p.Actor, &p.Reason, &p.Scope, &p.FromSeq, &p.Created)
		return p, err
	})
}
