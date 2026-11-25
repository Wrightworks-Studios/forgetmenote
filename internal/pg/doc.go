package pg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrTooLarge reports a document past its store's limit, refused before writing.
var ErrTooLarge = errors.New("state exceeds its storage limit")

// Doc caches a store whose state is one versioned document, as most stores kept
// one JSON file. Load rereads the document only when another process changed
// it, and Save writes it only over the version last loaded or saved. The store
// serializes calls under its own lock.
type Doc[T any] struct {
	db       *DB
	kind, id string
	limit    int
	version  int64 // -1 until loaded, and after a failed save
}

func NewDoc[T any](db *DB, kind, id string, limit int) *Doc[T] {
	return &Doc[T]{db: db, kind: kind, id: id, limit: limit, version: -1}
}

// Load returns the stored value and true when it changed since the last Load
// or Save; a missing document is the zero value. A store that refuses the value
// calls Forget, so the next Load reads it again.
func (d *Doc[T]) Load(ctx context.Context) (T, bool, error) {
	var value T
	version, body, err := d.db.ReadDocument(ctx, d.db.Pool, d.kind, d.id, d.version)
	if err != nil || version == d.version {
		return value, false, err
	}
	if version != 0 {
		if err = json.Unmarshal(body, &value); err != nil {
			return value, false, fmt.Errorf("%s state: %w", d.kind, err)
		}
	}
	d.version = version
	return value, true, nil
}

// Save stores value, or fails with ErrConflict when another process saved
// first. After any failed write the next Load reads the document again.
func (d *Doc[T]) Save(ctx context.Context, value T) error {
	if d.version < 0 {
		return fmt.Errorf("%s state is not loaded", d.kind)
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(raw) > d.limit {
		return fmt.Errorf("%w: %s state, %d KiB", ErrTooLarge, d.kind, d.limit>>10)
	}
	next, err := d.db.WriteDocument(ctx, d.db.Pool, d.kind, d.id, d.version, raw)
	if err != nil {
		d.version = -1
		return err
	}
	d.version = next
	return nil
}

// Forget makes the next Load read the document again.
func (d *Doc[T]) Forget() { d.version = -1 }
