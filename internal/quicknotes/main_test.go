package quicknotes

import (
	"context"
	"strings"
	"sync"
	"testing"

	"forgetmenote/internal/pg"
	"forgetmenote/internal/pg/pgtest"

	"github.com/jackc/pgx/v5"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

// failNotebookCommits makes Postgres refuse every commit to notebook in db's
// org, as a full disk or a lost primary would for one write, until the
// returned function runs or the test ends. Other notebooks commit as usual.
func failNotebookCommits(t *testing.T, db *pg.DB, notebook string) func() {
	t.Helper()
	ctx := context.Background()
	name := pgx.Identifier{"quicknotes_fail_" + db.Org + "_" + strings.ReplaceAll(notebook, "-", "_")}.Sanitize()
	literal := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	if _, err := db.Pool.Exec(ctx, "CREATE FUNCTION "+name+"() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected commit failure'; END $$"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, "CREATE TRIGGER "+name+" BEFORE INSERT OR UPDATE ON revisions FOR EACH ROW WHEN (NEW.org = "+literal(db.Org)+" AND NEW.notebook = "+literal(notebook)+") EXECUTE FUNCTION "+name+"()"); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	restore := func() {
		once.Do(func() {
			if _, err := db.Pool.Exec(ctx, "DROP TRIGGER IF EXISTS "+name+" ON revisions; DROP FUNCTION IF EXISTS "+name+"()"); err != nil {
				t.Errorf("restore commits: %v", err)
			}
		})
	}
	t.Cleanup(restore)
	return restore
}
