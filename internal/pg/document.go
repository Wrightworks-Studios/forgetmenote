package pg

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ErrConflict reports a document another process changed after it was read.
var ErrConflict = errors.New("document changed concurrently; reload and retry")

// ErrNUL reports text Postgres cannot store: a NUL character, which jsonb
// refuses. It is bad input, not an outage.
var ErrNUL = errors.New("text cannot contain NUL characters")

// Querier is a pool or a transaction.
type Querier interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

// ReadDocument returns a document's version and, only when it differs from
// known, its body. A missing document has version 0 and an empty body, so a
// caller checking an unchanged document pays one indexed lookup.
func (db *DB) ReadDocument(ctx context.Context, q Querier, kind, id string, known int64) (int64, []byte, error) {
	var version int64
	var body []byte
	err := q.QueryRow(ctx, `SELECT version, CASE WHEN version = $4 THEN NULL ELSE body::text END
		FROM documents WHERE org = $1 AND kind = $2 AND id = $3`, db.Org, kind, id, known).Scan(&version, &body)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, []byte("{}"), nil
	}
	return version, body, err
}

// LockDocument locks a document until tx ends and returns its version and, when
// it differs from known, its body. A missing document first gets its empty
// version 0 row, so concurrent first writers serialize on the same lock.
func (db *DB) LockDocument(ctx context.Context, tx pgx.Tx, kind, id string, known int64) (int64, []byte, error) {
	var version int64
	var body []byte
	for created := false; ; created = true {
		err := tx.QueryRow(ctx, `SELECT version, CASE WHEN version = $4 THEN NULL ELSE body::text END
			FROM documents WHERE org = $1 AND kind = $2 AND id = $3 FOR UPDATE`, db.Org, kind, id, known).Scan(&version, &body)
		if !errors.Is(err, pgx.ErrNoRows) || created {
			return version, body, err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO documents (org, kind, id, version, body) VALUES ($1, $2, $3, 0, '{}')
			ON CONFLICT DO NOTHING`, db.Org, kind, id); err != nil {
			return 0, nil, err
		}
	}
}

// WriteDocument replaces the document read at version and returns its new
// version, or ErrConflict when another writer committed first.
func (db *DB) WriteDocument(ctx context.Context, q Querier, kind, id string, version int64, body []byte) (int64, error) {
	var next int64
	var err error
	if version == 0 {
		err = q.QueryRow(ctx, `INSERT INTO documents (org, kind, id, version, body) VALUES ($1, $2, $3, 1, $4::jsonb)
			ON CONFLICT (org, kind, id) DO UPDATE SET version = 1, body = EXCLUDED.body, updated_at = now()
			WHERE documents.version = 0 RETURNING version`, db.Org, kind, id, string(body)).Scan(&next)
	} else {
		err = q.QueryRow(ctx, `UPDATE documents SET version = version + 1, body = $5::jsonb, updated_at = now()
			WHERE org = $1 AND kind = $2 AND id = $3 AND version = $4 RETURNING version`, db.Org, kind, id, version, string(body)).Scan(&next)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrConflict
	}
	if failure := (*pgconn.PgError)(nil); errors.As(err, &failure) && failure.Code == "22P05" {
		return 0, ErrNUL
	}
	return next, err
}

// DocumentIDs returns the ids of an org's written documents of one kind, in
// byte order.
func (db *DB) DocumentIDs(ctx context.Context, q Querier, kind string) ([]string, error) {
	rows, err := q.Query(ctx, `SELECT id FROM documents WHERE org = $1 AND kind = $2 AND version > 0 ORDER BY id COLLATE "C"`, db.Org, kind)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}
