package pgtest

import (
	"context"
	"testing"
	"time"
)

// Tests of one binary share a database, so a statement another test leaves
// waiting for a lock must not count as a write the hold keeps waiting, while
// a statement that waits behind a held write does.
func TestHoldWritesCountsOnlyTheStatementsItHolds(t *testing.T) {
	ctx := context.Background()
	db := Org(t)
	table := identifier("hold_probe_", db, "rows")
	if _, err := db.Pool.Exec(ctx, "CREATE TABLE "+table+" (org text NOT NULL, v int)"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Pool.Exec(ctx, "DROP TABLE IF EXISTS "+table) })
	unrelatedKey, rowKey := table+" unrelated", table+" row"

	// Another test's statement waits for a lock of its own.
	other, err := db.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Rollback(ctx)
	if _, err := other.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", unrelatedKey); err != nil {
		t.Fatal(err)
	}
	unrelated := make(chan error, 1)
	go func() {
		_, err := db.Pool.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", unrelatedKey)
		unrelated <- err
	}()
	for deadline := time.Now().Add(10 * time.Second); LockWaiters(t, db) == 0; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the unrelated statement never waited")
		}
	}

	waiting, release := HoldWrites(t, db, table)
	// A write, held, in a transaction that first takes a lock of the row's.
	const delay = 300 * time.Millisecond
	wrote := make(chan error, 1)
	go func() {
		time.Sleep(delay)
		tx, err := db.Pool.Begin(ctx)
		if err != nil {
			wrote <- err
			return
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", rowKey); err != nil {
			wrote <- err
			return
		}
		if _, err := tx.Exec(ctx, "INSERT INTO "+table+" (org, v) VALUES ($1, 1)", db.Org); err != nil {
			wrote <- err
			return
		}
		wrote <- tx.Commit(ctx)
	}()
	start := time.Now()
	waiting(1)
	if waited := time.Since(start); waited < delay {
		t.Errorf("waiting(1) returned after %v, before any write was held", waited)
	}
	// A statement that waits behind the held write's transaction.
	behind := make(chan error, 1)
	go func() {
		_, err := db.Pool.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", rowKey)
		behind <- err
	}()
	waiting(2)
	release()
	if err := <-wrote; err != nil {
		t.Fatal(err)
	}
	if err := <-behind; err != nil {
		t.Fatal(err)
	}
	other.Rollback(ctx)
	if err := <-unrelated; err != nil {
		t.Fatal(err)
	}
}
