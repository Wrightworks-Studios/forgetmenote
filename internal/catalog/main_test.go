package catalog

import (
	"testing"

	"forgetmenote/internal/pg"
	"forgetmenote/internal/pg/pgtest"
)

func TestMain(m *testing.M) {
	UseTestPasswordCost()
	pgtest.Main(m)
}

// testDB returns the database of a test's data directory, so a reopened
// catalog sees what an earlier one stored, as a restarted process would.
func testDB(t testing.TB, dir string) *pg.DB { t.Helper(); return pgtest.Dir(t, dir) }

// storedCatalog returns the catalog document as the database holds it.
func storedCatalog(t testing.TB, c *Catalog) string {
	t.Helper()
	_, body, err := c.db.ReadDocument(t.Context(), c.db.Pool, catalogKind, catalogID, -1)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// storeCatalog replaces the stored catalog document, as a restore would.
func storeCatalog(t testing.TB, db *pg.DB, raw string) {
	t.Helper()
	version, _, err := db.ReadDocument(t.Context(), db.Pool, catalogKind, catalogID, -1)
	if err == nil {
		_, err = db.WriteDocument(t.Context(), db.Pool, catalogKind, catalogID, version, []byte(raw))
	}
	if err != nil {
		t.Fatal(err)
	}
}
