package catalog

import (
	"testing"
	"time"

	"forgetmenote/internal/pg/pgtest"
)

// The storage import is unfinished from its start until it finishes, which
// records the commit it imported; an import that removed what it stored
// leaves neither record. Retire records when it last removed a notebook's
// previous release files. Each record is the notebook's own.
func TestStorageImportAndRetireRecords(t *testing.T) {
	db := pgtest.Org(t)
	ctx := t.Context()
	state := func(id string) (bool, string, time.Time) {
		t.Helper()
		unfinished, err := StorageImportUnfinished(ctx, db, id)
		if err != nil {
			t.Fatal(err)
		}
		imported, err := StorageImported(ctx, db, id)
		if err != nil {
			t.Fatal(err)
		}
		retired, err := StorageRetired(ctx, db, id)
		if err != nil {
			t.Fatal(err)
		}
		return unfinished, imported, retired
	}
	const first, second = "1111111111111111111111111111111111111111", "2222222222222222222222222222222222222222"
	if unfinished, imported, retired := state("alpha"); unfinished || imported != "" || !retired.IsZero() {
		t.Fatal("a notebook never imported:", unfinished, imported, retired)
	}

	if err := BeginStorageImport(ctx, db, "alpha", first); err != nil {
		t.Fatal(err)
	}
	if unfinished, imported, _ := state("alpha"); !unfinished || imported != "" {
		t.Fatal("an import begun:", unfinished, imported)
	}
	if err := FinishStorageImport(ctx, db, "alpha", first); err != nil {
		t.Fatal(err)
	}
	if unfinished, imported, _ := state("alpha"); unfinished || imported != first {
		t.Fatal("an import finished:", unfinished, imported)
	}
	// Imported again at another commit, after a restore of the database.
	if err := FinishStorageImport(ctx, db, "alpha", second); err != nil {
		t.Fatal(err)
	}
	if _, imported, _ := state("alpha"); imported != second {
		t.Fatal("an import finished again:", imported)
	}
	if unfinished, imported, retired := state("beta"); unfinished || imported != "" || !retired.IsZero() {
		t.Fatal("another notebook:", unfinished, imported, retired)
	}

	// An import that failed removed what it stored.
	if err := BeginStorageImport(ctx, db, "alpha", first); err != nil {
		t.Fatal(err)
	}
	if err := FinishStorageImport(ctx, db, "alpha", ""); err != nil {
		t.Fatal(err)
	}
	if unfinished, imported, _ := state("alpha"); unfinished || imported != "" {
		t.Fatal("an import that removed what it stored:", unfinished, imported)
	}

	before := time.Now().Add(-time.Minute)
	if err := RecordStorageRetired(ctx, db, "alpha", first); err != nil {
		t.Fatal(err)
	}
	_, _, retired := state("alpha")
	if retired.Before(before) || retired.After(time.Now().Add(time.Minute)) {
		t.Fatal("retired at", retired)
	}
	if err := RecordStorageRetired(ctx, db, "alpha", ""); err != nil {
		t.Fatal(err)
	}
	if _, _, again := state("alpha"); again.Before(retired) {
		t.Fatal("retired again at", again, "before", retired)
	}
	if _, _, other := state("beta"); !other.IsZero() {
		t.Fatal("another notebook retired at", other)
	}
}
