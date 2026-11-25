package automation

import (
	"context"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"forgetmenote/internal/pg"
	"forgetmenote/internal/pg/pgtest"

	"github.com/jackc/pgx/v5"
)

// An idle gateway must not keep the database busy: with no runs and no jobs,
// its automation runtime costs well under one transaction a second. Its
// periodic work, claimer fallbacks every 5 s, the scheduler lease's renewal
// every 7.5 s and the gateway presence's every 10 s, each costs a pool ping
// as well, so the window spans several of each and where its edges fall moves
// the rate little. FMN_IDLE_SECONDS changes the window.
func TestIdleRuntimeDatabaseLoad(t *testing.T) {
	window := 20 * time.Second
	if seconds, err := strconv.Atoi(os.Getenv("FMN_IDLE_SECONDS")); err == nil && seconds > 0 {
		window = time.Duration(seconds) * time.Second
	}
	address := pgtest.URL(t, true)
	db, err := pg.Open(t.Context(), address, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	r, err := OpenRuntime(db)
	if err != nil {
		t.Fatal(err)
	}
	r.Start(t.Context(), Hooks{Personal: func() []Spec { return nil }, Policy: func(Spec) error { return nil }, Run: func(context.Context, *Run, func() error) error { return nil }})
	t.Cleanup(r.Close)
	// Past the scheduler's first pass, one second in, and past the report of
	// the listener's LISTEN: an idle backend that is not closed reports its
	// counts up to ten seconds late.
	time.Sleep(11 * time.Second)
	commits, rollbacks := transactions(t, db, address)
	time.Sleep(window)
	afterCommits, afterRollbacks := transactions(t, db, address)
	commits, rollbacks = afterCommits-commits, afterRollbacks-rollbacks
	rate := float64(commits+rollbacks) / window.Seconds()
	t.Logf("idle runtime over %s: %d commits, %d rollbacks, %.2f transactions/s", window, commits, rollbacks, rate)
	if rate >= 1 {
		t.Errorf("an idle runtime costs %.2f transactions a second", rate)
	}
}

// transactions returns the database's committed and rolled back transactions
// once db's pooled connections have reported theirs. A backend reports its
// counts at most once a second, and while idle up to ten seconds later, but
// always when it exits, so the pool's connections are closed first.
func transactions(t *testing.T, db *pg.DB, address string) (commits, rollbacks int64) {
	t.Helper()
	u, err := url.Parse(address)
	if err != nil {
		t.Fatal(err)
	}
	name := strings.TrimPrefix(u.Path, "/")
	// Measure from another database, so the measurement counts no transaction.
	query := u.Query()
	query.Del("pool_max_conns")
	u.Path, u.RawQuery = "/postgres", query.Encode()
	conn, err := pgx.Connect(t.Context(), u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	db.Pool.Reset()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		var open int
		if err = conn.QueryRow(t.Context(), `SELECT count(*) FROM pg_stat_activity WHERE datname = $1 AND backend_type = 'client backend' AND application_name <> $2`, name, db.ListenerName()).Scan(&open); err != nil {
			t.Fatal(err)
		}
		if open == 0 {
			break
		}
	}
	// An exiting backend leaves pg_stat_activity just before it reports.
	time.Sleep(100 * time.Millisecond)
	if err = conn.QueryRow(t.Context(), `SELECT xact_commit, xact_rollback FROM pg_stat_database WHERE datname = $1`, name).Scan(&commits, &rollbacks); err != nil {
		t.Fatal(err)
	}
	return commits, rollbacks
}
