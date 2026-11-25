// Package pgtest gives each test its own Postgres database. A test binary
// starts one disposable cluster with the host's initdb and postgres, listening
// only on a Unix socket, unless FMN_TEST_DATABASE_URL names a server to use.
package pgtest

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"forgetmenote/internal/pg"

	"github.com/jackc/pgx/v5"
)

var (
	shared     *pg.DB
	sharedOnce sync.Once
	sharedErr  error
	once       sync.Once
	admin      *url.URL
	template   string
	cluster    *exec.Cmd
	watchdog   *exec.Cmd
	alive      *os.File
	dir        string
	startErr   error
	count      atomic.Int64
	dirs       sync.Map
)

// Main runs the package's tests, then stops the cluster this binary started.
// Packages that call Database or Empty use it from TestMain.
func Main(m *testing.M) {
	code := m.Run()
	stop()
	os.Exit(code)
}

// Database returns a fresh database with every migration applied.
func Database(t testing.TB) *pg.DB {
	t.Helper()
	return open(t, true)
}

// Empty returns a fresh database without the ForgetMeNote schema.
func Empty(t testing.TB) *pg.DB {
	t.Helper()
	return open(t, false)
}

// URL returns the connection string of a fresh database, for callers that open
// it themselves, such as a command reading a URL file.
func URL(t testing.TB, migrated bool) string {
	t.Helper()
	once.Do(start)
	if startErr != nil {
		t.Fatalf("start Postgres for tests: %v", startErr)
	}
	name := fmt.Sprintf("t%d_%d", os.Getpid(), count.Add(1))
	source := "template0"
	if migrated {
		source = template
	}
	if err := exec1(context.Background(), admin.String(), "CREATE DATABASE "+name+" TEMPLATE "+source); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := exec1(context.Background(), admin.String(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
			t.Errorf("drop test database: %v", err)
		}
	})
	return databaseURL(name)
}

// Org returns a handle on the binary's shared migrated database with an org of
// its own. Every table is keyed by org, so tests stay isolated without paying
// for a database each. The pool is shared: callers must not close the handle.
func Org(t testing.TB) *pg.DB {
	t.Helper()
	once.Do(start)
	if startErr != nil {
		t.Fatalf("start Postgres for tests: %v", startErr)
	}
	sharedOnce.Do(func() {
		name := fmt.Sprintf("shared_%d", os.Getpid())
		if sharedErr = exec1(context.Background(), admin.String(), "CREATE DATABASE "+name+" TEMPLATE "+template); sharedErr != nil {
			return
		}
		address, _ := url.Parse(databaseURL(name))
		q := address.Query()
		q.Set("pool_max_conns", "16")
		address.RawQuery = q.Encode()
		shared, sharedErr = pg.Open(context.Background(), address.String(), "")
	})
	if sharedErr != nil {
		t.Fatalf("open the shared test database: %v", sharedErr)
	}
	db := &pg.DB{Pool: shared.Pool, Org: fmt.Sprintf("t%d", count.Add(1)), Holder: pg.NewHolder()}
	leave, err := db.KeepPresence()
	if err != nil {
		t.Fatalf("record the test gateway's presence: %v", err)
	}
	t.Cleanup(leave)
	return db
}

// Gateway returns another gateway's handle on db's org: the same pool under a
// holder of its own, present until leave runs, as a gateway that stops does.
func Gateway(t testing.TB, db *pg.DB) (other *pg.DB, leave func()) {
	t.Helper()
	other = &pg.DB{Pool: db.Pool, Org: db.Org, Holder: pg.NewHolder()}
	leave, err := other.KeepPresence()
	if err != nil {
		t.Fatalf("record another gateway's presence: %v", err)
	}
	t.Cleanup(leave)
	return other, leave
}

// Listening waits until db's gateway listens on channel.
func Listening(t testing.TB, db *pg.DB, channel string) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		var listening bool
		err := db.Pool.QueryRow(context.Background(), `SELECT EXISTS (SELECT FROM pg_stat_activity WHERE application_name = $1 AND state = 'idle' AND query = $2)`, db.ListenerName(), `LISTEN "`+channel+`"`).Scan(&listening)
		if err != nil {
			t.Fatal(err)
		}
		if listening {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the gateway is not listening on", channel)
		}
	}
}

// Dir returns the org a test's data directory keeps when it is reopened, as one
// instance's database outlives a process restart.
func Dir(t testing.TB, dir string) *pg.DB {
	t.Helper()
	if db, ok := dirs.Load(dir); ok {
		return db.(*pg.DB)
	}
	db, _ := dirs.LoadOrStore(dir, Org(t))
	return db.(*pg.DB)
}

// FailWrites makes Postgres refuse every write of kind documents in db's org,
// as a full disk or a lost primary would, until the returned function runs or
// the test ends. Reads keep working.
func FailWrites(t testing.TB, db *pg.DB, kind string) func() {
	t.Helper()
	return failWrites(t, db, "documents", kind, "NEW.kind = '"+kind+"'")
}

// FailTable makes Postgres refuse every insert and update of table's rows in
// db's org, as FailWrites does for one kind of document.
func FailTable(t testing.TB, db *pg.DB, table string) func() {
	t.Helper()
	return failWrites(t, db, table, table, "true")
}

func identifier(prefix string, db *pg.DB, label string) string {
	return prefix + strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			return r
		}
		return '_'
	}, strings.ToLower(db.Org+"_"+label))
}

// HoldWrites makes every insert and update of table's rows in db's org wait,
// as a stalled disk would, until release runs or the test ends; reads and
// deletes go on. waiting returns once n statements of the database wait for a
// lock, held writes among them.
func HoldWrites(t testing.TB, db *pg.DB, table string) (waiting func(n int), release func()) {
	t.Helper()
	name := identifier(fmt.Sprintf("pgtest_hold_%d_", count.Add(1)), db, table)
	ctx := context.Background()
	hold, err := db.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release = func() { once.Do(func() { hold.Rollback(ctx) }) }
	t.Cleanup(func() {
		release()
		if _, err := db.Pool.Exec(ctx, "DROP TRIGGER IF EXISTS "+name+" ON "+table+"; DROP FUNCTION IF EXISTS "+name+"()"); err != nil {
			t.Errorf("stop holding writes: %v", err)
		}
	})
	if _, err = hold.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", name); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Pool.Exec(ctx, "CREATE FUNCTION "+name+"() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_advisory_xact_lock_shared(hashtext('"+name+"')); RETURN NEW; END $$"); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Pool.Exec(ctx, "CREATE TRIGGER "+name+" BEFORE INSERT OR UPDATE ON "+table+" FOR EACH ROW WHEN (NEW.org = '"+db.Org+"') EXECUTE FUNCTION "+name+"()"); err != nil {
		t.Fatal(err)
	}
	waiting = func(n int) {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
			waiters := LockWaiters(t, db)
			if waiters >= n {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%d statements wait for a lock, not %d", waiters, n)
			}
		}
	}
	return waiting, release
}

// LockWaiters counts the statements of db's database that wait for a lock.
func LockWaiters(t testing.TB, db *pg.DB) int {
	t.Helper()
	var n int
	if err := db.Pool.QueryRow(context.Background(), `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func failWrites(t testing.TB, db *pg.DB, table, label, condition string) func() {
	t.Helper()
	name := identifier("pgtest_fail_", db, label)
	ctx := context.Background()
	if _, err := db.Pool.Exec(ctx, "CREATE FUNCTION "+name+"() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected write failure'; END $$"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, "CREATE TRIGGER "+name+" BEFORE INSERT OR UPDATE ON "+table+" FOR EACH ROW WHEN (NEW.org = '"+db.Org+"' AND "+condition+") EXECUTE FUNCTION "+name+"()"); err != nil {
		t.Fatal(err)
	}
	var restore sync.Once
	stop := func() {
		restore.Do(func() {
			if _, err := db.Pool.Exec(ctx, "DROP TRIGGER IF EXISTS "+name+" ON "+table+"; DROP FUNCTION IF EXISTS "+name+"()"); err != nil {
				t.Errorf("restore writes: %v", err)
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

func open(t testing.TB, migrated bool) *pg.DB {
	t.Helper()
	db, err := pg.Open(context.Background(), URL(t, migrated), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	return db
}

func databaseURL(name string) string {
	u := *admin
	u.Path = "/" + name
	q := u.Query()
	// Parallel tests each hold a pool; keep the cluster's connection budget.
	q.Set("pool_max_conns", "4")
	u.RawQuery = q.Encode()
	return u.String()
}

func exec1(ctx context.Context, address, statement string) error {
	conn, err := pgx.Connect(ctx, address)
	if err != nil {
		return err
	}
	defer conn.Close(context.WithoutCancel(ctx))
	_, err = conn.Exec(ctx, statement)
	return err
}

func start() {
	if raw := os.Getenv("FMN_TEST_DATABASE_URL"); raw != "" {
		admin, startErr = url.Parse(raw)
	} else {
		startErr = startCluster()
	}
	if startErr != nil {
		return
	}
	// Concurrent test binaries may share one external server.
	template = fmt.Sprintf("fmn_template_%d", os.Getpid())
	if startErr = exec1(context.Background(), admin.String(), "CREATE DATABASE "+template+" TEMPLATE template0"); startErr != nil {
		return
	}
	db, err := pg.Open(context.Background(), databaseURL(template), "")
	if err != nil {
		startErr = err
		return
	}
	_, startErr = db.Migrate(context.Background())
	db.Close()
}

func startCluster() error {
	initdb, err := exec.LookPath("initdb")
	if err != nil {
		return fmt.Errorf("initdb is required for Postgres tests: %w", err)
	}
	postgres, err := exec.LookPath("postgres")
	if err != nil {
		return fmt.Errorf("postgres is required for Postgres tests: %w", err)
	}
	base := os.TempDir()
	// A Unix socket path must stay under about 100 bytes.
	if len(base) > 60 {
		base = "/tmp"
	}
	if dir, err = os.MkdirTemp(base, "fmnpg"); err != nil {
		return err
	}
	data := filepath.Join(dir, "data")
	if out, err := exec.Command(initdb, "-D", data, "-U", "fmn", "-A", "trust", "-E", "UTF8", "--no-locale", "--no-sync").CombinedOutput(); err != nil {
		return fmt.Errorf("initdb: %v: %s", err, out)
	}
	logFile, err := os.Create(filepath.Join(dir, "postgres.log"))
	if err != nil {
		return err
	}
	cluster = exec.Command(postgres, "-D", data, "-k", dir, "-c", "listen_addresses=", "-c", "max_connections=300",
		"-c", "fsync=off", "-c", "synchronous_commit=off", "-c", "full_page_writes=off")
	cluster.Stdout, cluster.Stderr = logFile, logFile
	cluster.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err = cluster.Start(); err != nil {
		logFile.Close()
		return err
	}
	logFile.Close()
	// If this binary is killed, its end of the pipe closes and the watchdog
	// stops the cluster and removes its files anyway. Until then it waits in a
	// shell builtin, so stop can end it at once: it inherits this binary's
	// descriptors, such as a release lock, and must not outlive a clean exit.
	watched, write, err := os.Pipe()
	if err != nil {
		return err
	}
	defer watched.Close()
	watchdog = exec.Command("/bin/sh", "-c", `read -r line; kill -INT "$1" 2>/dev/null; sleep 5; rm -rf "$2"`,
		"pgtest", strconv.Itoa(cluster.Process.Pid), dir)
	watchdog.Stdin = watched
	watchdog.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err = watchdog.Start(); err != nil {
		write.Close()
		return err
	}
	// A collected *os.File closes its descriptor, which would set the watchdog off.
	alive = write
	admin = &url.URL{Scheme: "postgres", User: url.User("fmn"), Path: "/postgres", RawQuery: url.Values{"host": {dir}, "sslmode": {"disable"}}.Encode()}
	deadline := time.Now().Add(30 * time.Second)
	for {
		err = exec1(context.Background(), admin.String(), "SELECT 1")
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			raw, _ := os.ReadFile(filepath.Join(dir, "postgres.log"))
			return fmt.Errorf("Postgres did not accept connections: %v: %s", err, raw)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func stop() {
	if shared != nil {
		shared.Close()
		if os.Getenv("FMN_TEST_DATABASE_URL") != "" {
			exec1(context.Background(), admin.String(), fmt.Sprintf("DROP DATABASE IF EXISTS shared_%d WITH (FORCE)", os.Getpid()))
		}
	}
	if admin != nil && template != "" && os.Getenv("FMN_TEST_DATABASE_URL") != "" {
		exec1(context.Background(), admin.String(), "DROP DATABASE IF EXISTS "+template+" WITH (FORCE)")
	}
	if cluster == nil {
		return
	}
	cluster.Process.Signal(syscall.SIGINT)
	done := make(chan struct{})
	go func() { cluster.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		syscall.Kill(-cluster.Process.Pid, syscall.SIGKILL)
		<-done
	}
	os.RemoveAll(dir)
	// Nothing is left for the watchdog to clean up.
	watchdog.Process.Kill()
	watchdog.Wait()
	alive.Close()
}
