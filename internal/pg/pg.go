// Package pg owns the instance's Postgres connection pool, its embedded schema
// migrations and the reader-version check. Owner packages keep their own tables
// and transactions; this package opens every database connection.
package pg

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ReaderVersion is the newest stored meaning this binary enforces. A database
// whose min_version is higher holds rules this binary would not apply.
// Version 2 keeps notebook content in the notes store: a release that keeps
// it in Git working trees (version 1) must not serve such an instance, as it
// would serve and change the old repositories, which storage v2 never reads.
const ReaderVersion = 2

// DefaultOrg is the org of a dedicated instance; every table is keyed by org.
const DefaultOrg = "default"

// MinimumServer is the oldest Postgres server this schema is tested against.
const MinimumServer = 140000

// migrationLock serializes concurrent migrate runs. A session advisory lock is
// enough here because it fences nothing beyond the schema change itself.
const migrationLock = 0x666d6e6d696772

//go:embed migrations/*.sql
var migrationFiles embed.FS

var orgID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
var migrationName = regexp.MustCompile(`^([0-9]{4})_[a-z0-9_]+\.sql$`)

type DB struct {
	Pool *pgxpool.Pool
	Org  string
	// Holder identifies this gateway process in leases and in the work it
	// claims. Open gives each handle a new one.
	Holder string
}

type migration struct {
	version  int
	name, up string
}

// Open connects without echoing the URL, which carries the database password.
func Open(ctx context.Context, url, org string) (*DB, error) {
	if org == "" {
		org = DefaultOrg
	}
	if !orgID.MatchString(org) {
		return nil, errors.New("database org must be lowercase letters, digits and hyphens")
	}
	config, err := pgxpool.ParseConfig(strings.TrimSpace(url))
	if err != nil {
		return nil, errors.New("database URL is not a valid Postgres connection string")
	}
	// Each lock held across slow work, such as an import or a citation check,
	// keeps a connection. pgx's default of max(4, CPUs) would let a few of them
	// stall every request, so a URL that sets no size gets 16.
	if !strings.Contains(url, "pool_max_conns") {
		config.MaxConns = max(config.MaxConns, 16)
	}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	var server int
	if err = pool.QueryRow(ctx, "SELECT current_setting('server_version_num')::integer").Scan(&server); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect to database: %w", err)
	}
	if server < MinimumServer {
		pool.Close()
		return nil, fmt.Errorf("Postgres %d.%d is older than the supported %d", server/10000, server%10000, MinimumServer/10000)
	}
	return &DB{Pool: pool, Org: org, Holder: NewHolder()}, nil
}

func (db *DB) Close() { db.Pool.Close() }

func migrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return nil, err
	}
	var list []migration
	for _, entry := range entries {
		match := migrationName.FindStringSubmatch(entry.Name())
		if match == nil {
			return nil, fmt.Errorf("migration %s is not named NNNN_name.sql", entry.Name())
		}
		raw, err := migrationFiles.ReadFile("migrations/" + entry.Name())
		if err != nil {
			return nil, err
		}
		version, _ := strconv.Atoi(match[1])
		list = append(list, migration{version: version, name: entry.Name(), up: string(raw)})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].version < list[j].version })
	for i, m := range list {
		if m.version != i+1 {
			return nil, fmt.Errorf("migration %s breaks the version sequence", m.name)
		}
	}
	return list, nil
}

// Migrate applies this binary's missing migrations, each in its own transaction,
// and returns how many it applied. Versions newer than this binary are kept: the
// expand-and-contract rule makes them readable by it.
func (db *DB) Migrate(ctx context.Context) (int, error) {
	list, err := migrations()
	if err != nil {
		return 0, err
	}
	conn, err := db.Pool.Acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Release()
	if _, err = conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrationLock); err != nil {
		return 0, err
	}
	defer conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", migrationLock)
	if _, err = conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version integer PRIMARY KEY, name text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return 0, err
	}
	applied, err := appliedVersions(ctx, conn)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, m := range list {
		if applied[m.version] != "" {
			if applied[m.version] != m.name {
				return count, fmt.Errorf("database migration %d is %s, not %s", m.version, applied[m.version], m.name)
			}
			continue
		}
		err = pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, m.up); err != nil {
				return fmt.Errorf("migration %s: %w", m.name, err)
			}
			_, err := tx.Exec(ctx, "INSERT INTO schema_migrations (version, name) VALUES ($1, $2)", m.version, m.name)
			return err
		})
		if err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

type querier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func appliedVersions(ctx context.Context, q querier) (map[int]string, error) {
	rows, err := q.Query(ctx, "SELECT version, name FROM schema_migrations")
	if err != nil {
		return nil, err
	}
	applied := map[int]string{}
	for rows.Next() {
		var version int
		var name string
		if err := rows.Scan(&version, &name); err != nil {
			rows.Close()
			return nil, err
		}
		applied[version] = name
	}
	return applied, rows.Err()
}

// Check refuses a database this binary must not serve: one missing any of its
// migrations, or one that requires a newer reader.
func (db *DB) Check(ctx context.Context) error {
	list, err := migrations()
	if err != nil {
		return err
	}
	var exists bool
	if err = db.Pool.QueryRow(ctx, "SELECT to_regclass('schema_migrations') IS NOT NULL").Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return errors.New("database has no ForgetMeNote schema; run forgetmenote migrate")
	}
	applied, err := appliedVersions(ctx, db.Pool)
	if err != nil {
		return err
	}
	for _, m := range list {
		if applied[m.version] != m.name {
			return fmt.Errorf("database schema lacks %s; run forgetmenote migrate", m.name)
		}
	}
	var required int
	if err = db.Pool.QueryRow(ctx, "SELECT min_version FROM instance_reader").Scan(&required); err != nil {
		return err
	}
	if required > ReaderVersion {
		return fmt.Errorf("database requires reader version %d; this binary enforces only %d, so it must not serve this instance", required, ReaderVersion)
	}
	return nil
}

// RequireReader raises the database's minimum reader to this binary's version,
// once every replica runs it, so a rollback cannot drop a newer rule. It never
// lowers the minimum.
func (db *DB) RequireReader(ctx context.Context) (int, error) {
	var required int
	err := db.Pool.QueryRow(ctx, "UPDATE instance_reader SET min_version = GREATEST(min_version, $1) RETURNING min_version", ReaderVersion).Scan(&required)
	return required, err
}
