package pg

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// VersionedRows reads and writes the rows of a table that keeps one body per
// key under a version, which every write raises by one. A writer replaces only
// the version it read, so of two writers that read the same version, the second
// gets ErrConflict however their critical sections were serialized. A missing
// row reads as version 0, and writing version 0 inserts the row only while it
// is absent. The table's other columns need defaults.
type VersionedRows struct {
	read, version, insert, update, remove string
}

// NewVersionedRows prepares the statements for table, whose rows are keyed by
// the columns keys and hold a bigint version column and the column body.
func NewVersionedRows(table, body string, keys ...string) VersionedRows {
	name := func(s string) string { return pgx.Identifier{s}.Sanitize() }
	columns, values, match := make([]string, len(keys)), make([]string, len(keys)), make([]string, len(keys))
	for i, key := range keys {
		columns[i], values[i] = name(key), fmt.Sprintf("$%d", i+1)
		match[i] = columns[i] + " = " + values[i]
	}
	t, b, where, n := name(table), name(body), strings.Join(match, " AND "), len(keys)
	return VersionedRows{
		read:    fmt.Sprintf("SELECT version, %s FROM %s WHERE %s", b, t, where),
		version: fmt.Sprintf("SELECT version FROM %s WHERE %s", t, where),
		insert: fmt.Sprintf("INSERT INTO %s (%s, version, %s) VALUES (%s, 1, $%d) ON CONFLICT DO NOTHING RETURNING version",
			t, strings.Join(columns, ", "), b, strings.Join(values, ", "), n+1),
		update: fmt.Sprintf("UPDATE %s SET version = version + 1, %s = $%d WHERE %s AND version = $%d RETURNING version", t, b, n+1, where, n+2),
		remove: fmt.Sprintf("DELETE FROM %s WHERE %s AND version = $%d", t, where, n+1),
	}
}

// Read returns the version and body of the row keyed by key, or version 0 and
// no body when there is none.
func (v VersionedRows) Read(ctx context.Context, q Querier, key ...any) (int64, []byte, error) {
	var version int64
	var body []byte
	err := q.QueryRow(ctx, v.read, key...).Scan(&version, &body)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil, nil
	}
	return version, body, err
}

// Version returns only the version of the row keyed by key, 0 when there is
// none, so a caller watching a row pays one indexed lookup and no body.
func (v VersionedRows) Version(ctx context.Context, q Querier, key ...any) (int64, error) {
	var version int64
	err := q.QueryRow(ctx, v.version, key...).Scan(&version)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return version, err
}

// Write replaces the body of the row keyed by key that was read at version, or
// inserts it when version is 0, and returns the new version. It reports
// ErrConflict, writing nothing, when another writer changed, created or
// deleted the row first.
func (v VersionedRows) Write(ctx context.Context, q Querier, version int64, body []byte, key ...any) (int64, error) {
	var next int64
	var err error
	args := append(slices.Clip(key), body)
	if version == 0 {
		err = q.QueryRow(ctx, v.insert, args...).Scan(&next)
	} else {
		err = q.QueryRow(ctx, v.update, append(args, version)...).Scan(&next)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrConflict
	}
	if failure := (*pgconn.PgError)(nil); errors.As(err, &failure) && failure.Code == "22P05" {
		return 0, ErrNUL
	}
	return next, err
}

// Delete removes the row keyed by key that was read at version, and reports
// ErrConflict when another writer changed or deleted it first.
func (v VersionedRows) Delete(ctx context.Context, q Querier, version int64, key ...any) error {
	tag, err := q.Exec(ctx, v.remove, append(slices.Clip(key), version)...)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrConflict
	}
	return err
}
