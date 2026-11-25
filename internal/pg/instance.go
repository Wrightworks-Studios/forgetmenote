package pg

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ErrNotEmpty reports an import into a database that already holds some of the
// org's state, which an import never merges with.
var ErrNotEmpty = errors.New("the database already holds this instance's state")

// RequireEmpty fails with ErrNotEmpty when any table holds a row of db's org,
// including the presence a gateway leaves once it has served.
func (db *DB) RequireEmpty(ctx context.Context, q Querier) error {
	rows, err := q.Query(ctx, `SELECT table_name::text FROM information_schema.columns
		WHERE table_schema = current_schema() AND column_name = 'org' ORDER BY 1`)
	if err != nil {
		return err
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, table := range tables {
		var used bool
		if err = q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM `+pgx.Identifier{table}.Sanitize()+` WHERE org = $1)`, db.Org).Scan(&used); err != nil {
			return err
		}
		if used {
			return fmt.Errorf("%w: %s has rows for org %s", ErrNotEmpty, table, db.Org)
		}
	}
	return nil
}

// LiveGateways counts the gateways whose presence has not expired.
func (db *DB) LiveGateways(ctx context.Context, q Querier) (int, error) {
	var n int
	err := q.QueryRow(ctx, `SELECT count(*) FROM leases WHERE org = $1 AND name LIKE 'gateway/%' AND expires > now()`, db.Org).Scan(&n)
	return n, err
}
