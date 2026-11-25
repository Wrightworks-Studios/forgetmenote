package pg_test

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"forgetmenote/internal/pg"
	"forgetmenote/internal/pg/pgtest"
)

var automationRows = pg.NewVersionedRows("automation_records", "body", "org", "name")

func TestVersionedRowsWriteOnlyOverTheVersionRead(t *testing.T) {
	db := pgtest.Org(t)
	ctx := t.Context()
	if version, body, err := automationRows.Read(ctx, db.Pool, db.Org, "run"); err != nil || version != 0 || body != nil {
		t.Fatalf("a missing row read as version %d, %q, %v", version, body, err)
	}
	first, err := automationRows.Write(ctx, db.Pool, 0, []byte("one"), db.Org, "run")
	if err != nil || first != 1 {
		t.Fatalf("insert = %d, %v", first, err)
	}
	// A second insert of the same row conflicts instead of replacing it.
	if _, err = automationRows.Write(ctx, db.Pool, 0, []byte("other"), db.Org, "run"); !errors.Is(err, pg.ErrConflict) {
		t.Fatalf("a second insert = %v, want a conflict", err)
	}
	second, err := automationRows.Write(ctx, db.Pool, first, []byte("two"), db.Org, "run")
	if err != nil || second != 2 {
		t.Fatalf("update = %d, %v", second, err)
	}
	if _, err = automationRows.Write(ctx, db.Pool, first, []byte("stale"), db.Org, "run"); !errors.Is(err, pg.ErrConflict) {
		t.Fatalf("a write over a replaced version = %v, want a conflict", err)
	}
	if version, body, err := automationRows.Read(ctx, db.Pool, db.Org, "run"); err != nil || version != 2 || string(body) != "two" {
		t.Fatalf("read = %d, %q, %v", version, body, err)
	}
	if version, err := automationRows.Version(ctx, db.Pool, db.Org, "run"); err != nil || version != 2 {
		t.Fatalf("version = %d, %v", version, err)
	}
	if err = automationRows.Delete(ctx, db.Pool, first, db.Org, "run"); !errors.Is(err, pg.ErrConflict) {
		t.Fatalf("a delete of a replaced version = %v, want a conflict", err)
	}
	if err = automationRows.Delete(ctx, db.Pool, second, db.Org, "run"); err != nil {
		t.Fatal(err)
	}
	if version, err := automationRows.Version(ctx, db.Pool, db.Org, "run"); err != nil || version != 0 {
		t.Fatalf("a deleted row has version %d, %v", version, err)
	}
	if err = automationRows.Delete(ctx, db.Pool, second, db.Org, "run"); !errors.Is(err, pg.ErrConflict) {
		t.Fatalf("a delete of a missing row = %v, want a conflict", err)
	}
}

func TestVersionedRowsLetOneOfConcurrentWritersWin(t *testing.T) {
	db := pgtest.Org(t)
	ctx := t.Context()
	version, err := automationRows.Write(ctx, db.Pool, 0, []byte("queued"), db.Org, "run")
	if err != nil {
		t.Fatal(err)
	}
	var won, lost atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			switch _, err := automationRows.Write(ctx, db.Pool, version, []byte("claimed"), db.Org, "run"); {
			case err == nil:
				won.Add(1)
			case errors.Is(err, pg.ErrConflict):
				lost.Add(1)
			default:
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if won.Load() != 1 || lost.Load() != 7 {
		t.Fatalf("%d writers won and %d conflicted over one version", won.Load(), lost.Load())
	}
}
