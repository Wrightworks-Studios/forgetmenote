package pg_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"forgetmenote/internal/pg"
	"forgetmenote/internal/pg/pgtest"

	"github.com/jackc/pgx/v5"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

func TestConcurrentMigrationsApplyEachVersionOnce(t *testing.T) {
	db := pgtest.Empty(t)
	ctx := context.Background()
	if err := db.Check(ctx); err == nil || !strings.Contains(err.Error(), "forgetmenote migrate") {
		t.Fatalf("an empty database must ask for migrate, got %v", err)
	}
	counts := make([]int, 4)
	errs := make([]error, 4)
	var wg sync.WaitGroup
	for i := range counts {
		wg.Add(1)
		go func(i int) { defer wg.Done(); counts[i], errs[i] = db.Migrate(ctx) }(i)
	}
	wg.Wait()
	total := 0
	for i, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
		total += counts[i]
	}
	var applied int
	if err := db.Pool.QueryRow(ctx, "SELECT count(*) FROM schema_migrations").Scan(&applied); err != nil {
		t.Fatal(err)
	}
	if total != applied || applied == 0 {
		t.Fatalf("migrate runs applied %d migrations in total, schema records %d", total, applied)
	}
	if again, err := db.Migrate(ctx); err != nil || again != 0 {
		t.Fatalf("a current schema reapplied %d migrations: %v", again, err)
	}
	if err := db.Check(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestCheckRefusesADatabaseThatRequiresANewerReader(t *testing.T) {
	db := pgtest.Database(t)
	ctx := context.Background()
	if required, err := db.RequireReader(ctx); err != nil || required != pg.ReaderVersion {
		t.Fatalf("require reader = %d, %v", required, err)
	}
	if _, err := db.Pool.Exec(ctx, "UPDATE instance_reader SET min_version = $1", pg.ReaderVersion+1); err != nil {
		t.Fatal(err)
	}
	if err := db.Check(ctx); err == nil || !strings.Contains(err.Error(), "must not serve") {
		t.Fatalf("an older reader was allowed to serve: %v", err)
	}
	if required, err := db.RequireReader(ctx); err != nil || required != pg.ReaderVersion+1 {
		t.Fatalf("require reader lowered the minimum to %d: %v", required, err)
	}
}

func TestCheckRefusesARenamedMigration(t *testing.T) {
	db := pgtest.Database(t)
	ctx := context.Background()
	if _, err := db.Pool.Exec(ctx, "UPDATE schema_migrations SET name = 'other.sql' WHERE version = 1"); err != nil {
		t.Fatal(err)
	}
	if err := db.Check(ctx); err == nil {
		t.Fatal("a database recording a different migration 1 was accepted")
	}
	if _, err := db.Migrate(ctx); err == nil || !strings.Contains(err.Error(), "other.sql") {
		t.Fatalf("migrate accepted a different migration 1: %v", err)
	}
}

func TestOpenNeverEchoesTheDatabaseURL(t *testing.T) {
	_, err := pg.Open(context.Background(), "postgres://fmn:hunter2-secret@[::1", "")
	if err == nil || strings.Contains(err.Error(), "hunter2-secret") {
		t.Fatalf("open error leaked the URL: %v", err)
	}
	if _, err := pg.Open(context.Background(), "postgres://localhost/x", "Bad Org"); err == nil {
		t.Fatal("an invalid org was accepted")
	}
}

func TestDocumentsSerializeWritersAndReportConflicts(t *testing.T) {
	db := pgtest.Database(t)
	ctx := context.Background()
	if version, body, err := db.ReadDocument(ctx, db.Pool, "fixture", "one", -1); err != nil || version != 0 || string(body) != "{}" {
		t.Fatalf("missing document = %d %q %v", version, body, err)
	}
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = pgx.BeginFunc(ctx, db.Pool, func(tx pgx.Tx) error {
				version, body, err := db.LockDocument(ctx, tx, "fixture", "one", -1)
				if err != nil {
					return err
				}
				var count map[string]int
				if err := json.Unmarshal(body, &count); err != nil {
					return err
				}
				count["n"]++
				raw, _ := json.Marshal(count)
				_, err = db.WriteDocument(ctx, tx, "fixture", "one", version, raw)
				return err
			})
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	version, body, err := db.ReadDocument(ctx, db.Pool, "fixture", "one", -1)
	if err != nil || version != 8 || string(body) != `{"n": 8}` {
		t.Fatalf("locked writers = version %d body %s: %v", version, body, err)
	}
	if again, unchanged, err := db.ReadDocument(ctx, db.Pool, "fixture", "one", version); err != nil || again != version || unchanged != nil {
		t.Fatalf("an unchanged read returned its body: %d %q %v", again, unchanged, err)
	}
	if _, err := db.WriteDocument(ctx, db.Pool, "fixture", "one", 3, []byte(`{}`)); !errors.Is(err, pg.ErrConflict) {
		t.Fatalf("a stale write was accepted: %v", err)
	}
	other := &pg.DB{Pool: db.Pool, Org: "other"}
	if version, _, err := other.ReadDocument(ctx, db.Pool, "fixture", "one", -1); err != nil || version != 0 {
		t.Fatalf("another org read this org's document: %d %v", version, err)
	}
}

func TestDocumentRefusesNULTextAsInputNotOutage(t *testing.T) {
	db := pgtest.Org(t)
	ctx := context.Background()
	raw, _ := json.Marshal(map[string]string{"name": "a\x00b"})
	if _, err := db.WriteDocument(ctx, db.Pool, "fixture", "nul", 0, raw); !errors.Is(err, pg.ErrNUL) {
		t.Fatalf("NUL text = %v", err)
	}
	raw, _ = json.Marshal(map[string]string{"name": `a\u0000b`})
	if _, err := db.WriteDocument(ctx, db.Pool, "fixture", "nul", 0, raw); err != nil {
		t.Fatal("the literal text \\u0000 was refused:", err)
	}
}

func TestDocReloadsOnlyAnotherProcessChangeAndRefusesStaleSaves(t *testing.T) {
	db := pgtest.Org(t)
	ctx := context.Background()
	type state struct{ N int }
	a, b := pg.NewDoc[state](db, "fixture", "doc", 1<<10), pg.NewDoc[state](db, "fixture", "doc", 1<<10)
	if value, changed, err := a.Load(ctx); err != nil || !changed || value.N != 0 {
		t.Fatalf("a missing document = %+v %v %v", value, changed, err)
	}
	if err := a.Save(ctx, state{1}); err != nil {
		t.Fatal(err)
	}
	if _, changed, err := a.Load(ctx); err != nil || changed {
		t.Fatalf("its own save reloaded: %v %v", changed, err)
	}
	if value, changed, err := b.Load(ctx); err != nil || !changed || value.N != 1 {
		t.Fatalf("another process's save is unseen: %+v %v %v", value, changed, err)
	}
	if err := b.Save(ctx, state{2}); err != nil {
		t.Fatal(err)
	}
	if err := a.Save(ctx, state{3}); !errors.Is(err, pg.ErrConflict) {
		t.Fatalf("a stale save = %v", err)
	}
	if value, changed, err := a.Load(ctx); err != nil || !changed || value.N != 2 {
		t.Fatalf("after a conflict = %+v %v %v", value, changed, err)
	}
	if err := a.Save(ctx, state{N: 1 << 40}); err != nil {
		t.Fatal(err)
	}
	big := pg.NewDoc[map[string]string](db, "fixture", "big", 16)
	big.Load(ctx)
	if err := big.Save(ctx, map[string]string{"k": "a value past the limit"}); !errors.Is(err, pg.ErrTooLarge) {
		t.Fatal("an oversized document was stored")
	}
}
