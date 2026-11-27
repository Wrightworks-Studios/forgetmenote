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

// RecordGateway stores body as this gateway's record of kind, such as the
// startup settings each gateway of an instance should agree on, and removes
// the records of kind that gateways whose presence is gone left behind.
func (db *DB) RecordGateway(ctx context.Context, kind string, body []byte) error {
	if db.Holder == "" {
		return errors.New("database handle has no holder identity")
	}
	if _, err := db.Pool.Exec(ctx, `DELETE FROM documents d WHERE d.org = $1 AND d.kind = $2 AND d.id <> $3 AND NOT EXISTS (
		SELECT 1 FROM leases l WHERE l.org = d.org AND l.name = 'gateway/' || d.id AND l.holder = d.id AND l.expires > now())`, db.Org, kind, db.Holder); err != nil {
		return err
	}
	_, err := db.Pool.Exec(ctx, `INSERT INTO documents (org, kind, id, version, body) VALUES ($1, $2, $3, 1, $4::jsonb)
		ON CONFLICT (org, kind, id) DO UPDATE SET version = documents.version + 1, body = EXCLUDED.body, updated_at = now()`, db.Org, kind, db.Holder, string(body))
	return err
}

// GatewayRecords returns, by holder, the records of kind that gateways whose
// presence has not expired stored with RecordGateway.
func (db *DB) GatewayRecords(ctx context.Context, kind string) (map[string][]byte, error) {
	rows, err := db.Pool.Query(ctx, `SELECT d.id, d.body::text FROM documents d
		JOIN leases l ON l.org = d.org AND l.name = 'gateway/' || d.id AND l.holder = d.id
		WHERE d.org = $1 AND d.kind = $2 AND l.expires > now()`, db.Org, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records := map[string][]byte{}
	for rows.Next() {
		var holder, body string
		if err = rows.Scan(&holder, &body); err != nil {
			return nil, err
		}
		records[holder] = []byte(body)
	}
	return records, rows.Err()
}

// LiveGateways counts the gateways whose presence has not expired.
func (db *DB) LiveGateways(ctx context.Context, q Querier) (int, error) {
	var n int
	err := q.QueryRow(ctx, `SELECT count(*) FROM leases WHERE org = $1 AND name LIKE 'gateway/%' AND expires > now()`, db.Org).Scan(&n)
	return n, err
}
