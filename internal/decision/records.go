package decision

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"forgetmenote/internal/pg"
	"os"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Records owns bounded private decision metadata by name, in the instance's
// database; notebook content stays in Git. Every write of a record holds the
// record's lock for its transaction, which Update holds across its read too,
// so a read-modify-write through Update never loses a change another gateway
// committed meanwhile. Write replaces a record whole, as the record files did.
type Records struct {
	db    *pg.DB
	Bytes int64
}

// recordBytes bounds a record unless Bytes sets another limit.
const recordBytes = 4 << 20

func OpenRecords(db *pg.DB) (*Records, error) {
	if db == nil {
		return nil, ErrStorage
	}
	return &Records{db: db}, nil
}

func recordName(name string) bool {
	return name != "" && len(name) <= 256 && !strings.ContainsRune(name, 0) && !strings.Contains(name, "..")
}

func recordContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 10*time.Second)
}

// Read decodes the named record, or returns os.ErrNotExist when there is none.
func (j *Records) Read(name string, value any) error {
	if !recordName(name) {
		return ErrStorage
	}
	ctx, cancel := recordContext()
	defer cancel()
	raw, err := j.read(ctx, j.db.Pool, name)
	if errors.Is(err, pgx.ErrNoRows) {
		return os.ErrNotExist
	}
	if err != nil || int64(len(raw)) > j.limit() || !DecodeRecord(raw, value) {
		return ErrStorage
	}
	return nil
}

func (j *Records) Write(name string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil || int64(len(raw)) > j.limit() || !recordName(name) {
		return ErrStorage
	}
	return j.locked(name, func(ctx context.Context, tx pgx.Tx) error { return j.put(ctx, tx, name, raw) })
}

// Update lets change modify the named record and writes the result, in one
// transaction holding the record's lock, so updates of one name apply one
// after another on every gateway. value points to the caller's default, which
// a stored record replaces before change runs; found reports whether one did.
// Nothing is written when change fails, whose error Update returns as it is,
// or when change leaves value as it was.
func (j *Records) Update(name string, value any, change func(found bool) error) error {
	target := reflect.ValueOf(value)
	if !recordName(name) || target.Kind() != reflect.Pointer || target.IsNil() {
		return ErrStorage
	}
	var refused error
	err := j.locked(name, func(ctx context.Context, tx pgx.Tx) error {
		raw, err := j.read(ctx, tx, name)
		found := err == nil
		if found {
			target.Elem().SetZero()
			if int64(len(raw)) > j.limit() || !DecodeRecord(raw, value) {
				return ErrStorage
			}
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		before, err := json.Marshal(value)
		if err != nil {
			return err
		}
		if refused = change(found); refused != nil {
			return refused
		}
		after, err := json.Marshal(value)
		if err != nil || int64(len(after)) > j.limit() {
			return ErrStorage
		}
		if bytes.Equal(before, after) {
			return nil
		}
		return j.put(ctx, tx, name, after)
	})
	if refused != nil {
		return refused
	}
	return err
}

// Remove deletes the named record and, in the same transaction, every record
// whose name starts with one of owned, so what a record owns, such as a job's
// results, never outlives it. Only the named record's lock is taken.
func (j *Records) Remove(name string, owned ...string) error {
	if !recordName(name) || slices.Contains(owned, "") {
		return ErrStorage
	}
	prefixes := append([]string{}, owned...)
	return j.locked(name, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM jev_records WHERE org = $1 AND (name = $2 OR name ^@ ANY($3::text[]))`, j.db.Org, name, prefixes)
		return err
	})
}

// List returns the names that start with prefix, in order, up to limit+1 so a
// caller can tell that more exist.
func (j *Records) List(prefix string, limit int) ([]string, error) {
	ctx, cancel := recordContext()
	defer cancel()
	rows, err := j.db.Pool.Query(ctx, `SELECT name FROM jev_records WHERE org = $1 AND starts_with(name, $2) ORDER BY name LIMIT $3`, j.db.Org, prefix, limit+1)
	if err != nil {
		return nil, ErrStorage
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, ErrStorage
	}
	return names, nil
}

// Body is a stored record as ListBodies returns it.
type Body struct {
	Name string
	raw  []byte
}

// Decode decodes the body as Read decodes a record.
func (b Body) Decode(value any) error {
	if b.raw == nil || !DecodeRecord(b.raw, value) {
		return ErrStorage
	}
	return nil
}

// ListBodies returns the records whose names start with prefix, with their
// bodies, in name order, up to limit+1 so a caller can tell that more exist,
// in one query.
func (j *Records) ListBodies(prefix string, limit int) ([]Body, error) {
	return j.ListMatching(prefix, nil, limit)
}

// ListMatching is ListBodies keeping only the records whose bodies contain
// fields, as JSON containment (@>) defines it, so a caller looking for a few
// records does not read every one.
func (j *Records) ListMatching(prefix string, fields map[string]any, limit int) ([]Body, error) {
	var match *string
	if fields != nil {
		raw, err := json.Marshal(fields)
		if err != nil {
			return nil, ErrStorage
		}
		text := string(raw)
		match = &text
	}
	ctx, cancel := recordContext()
	defer cancel()
	rows, err := j.db.Pool.Query(ctx, `SELECT name, body::text FROM jev_records WHERE org = $1 AND starts_with(name, $2) AND ($3::jsonb IS NULL OR body @> $3::jsonb) ORDER BY name LIMIT $4`, j.db.Org, prefix, match, limit+1)
	if err != nil {
		return nil, ErrStorage
	}
	bodies, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Body, error) {
		var body Body
		err := row.Scan(&body.Name, &body.raw)
		// A body past the limit is listed but, as Read does, refuses to decode.
		if int64(len(body.raw)) > j.limit() {
			body.raw = nil
		}
		return body, err
	})
	if err != nil {
		return nil, ErrStorage
	}
	return bodies, nil
}

// locked runs write in a transaction that first takes the record's lock. A
// record not yet written has no row to lock, so the lock is a
// transaction-scoped advisory lock on the name. It ends with the transaction
// that writes, so it fences the write: a write whose connection drops is
// never committed.
func (j *Records) locked(name string, write func(context.Context, pgx.Tx) error) error {
	ctx, cancel := recordContext()
	defer cancel()
	err := pgx.BeginFunc(ctx, j.db.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('jev-record ' || $2 || ' ' || $1, 0))`, j.db.Org, name); err != nil {
			return err
		}
		return write(ctx, tx)
	})
	if err != nil {
		return ErrStorage
	}
	return nil
}

func (j *Records) read(ctx context.Context, q pg.Querier, name string) ([]byte, error) {
	var raw []byte
	err := q.QueryRow(ctx, `SELECT body::text FROM jev_records WHERE org = $1 AND name = $2`, j.db.Org, name).Scan(&raw)
	return raw, err
}

func (j *Records) put(ctx context.Context, tx pgx.Tx, name string, raw []byte) error {
	_, err := tx.Exec(ctx, `INSERT INTO jev_records (org, name, body) VALUES ($1, $2, $3::jsonb)
		ON CONFLICT (org, name) DO UPDATE SET body = EXCLUDED.body`, j.db.Org, name, string(raw))
	return err
}

func (j *Records) limit() int64 {
	if j.Bytes > 0 {
		return j.Bytes
	}
	return recordBytes
}
