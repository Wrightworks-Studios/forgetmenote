// Package capture takes a consistent copy of a running instance for the
// external backup worker, which needs no downtime for it: the Git exports and
// the data directory's secret files first, then, under one exported database
// snapshot, the repository source files and the content blocks that snapshot
// uses. The worker dumps the database with that snapshot
// (pg_dump --snapshot) while the capture keeps it open, and the three parts
// together restore the instance as it was at the snapshot.
//
// The protocol, version 1, is the one forgetmenote admin capture speaks:
// the capture prints one line, "snapshot <ID> purge <latest purge ID or ->",
// once DIR is complete, and then waits for one line on its input. "done"
// ends the snapshot and the capture succeeds; anything else, the end of the
// input, the wait running out or a signal abandons it, and the caller
// deletes DIR.
package capture

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"forgetmenote/internal/blob"
	"forgetmenote/internal/notestore"
	"forgetmenote/internal/pg"
)

// Format is the version of the protocol and of capture.json.
const Format = 1

// Defaults of Options' waits.
const (
	DefaultLockWait = 15 * time.Minute
	DefaultWait     = 30 * time.Minute
)

// Options name what one capture copies and where.
type Options struct {
	DB    *pg.DB
	Blobs blob.Store
	// Data is the instance's data directory, which holds the Git exports
	// and the secret files.
	Data string
	// Out is the new directory the capture creates and fills.
	Out string
	// Version names the binary; capture.json adds the database's schema.
	Version string
	// LockWait bounds the wait for a running block sweep or a purge's block
	// removal to end, DefaultLockWait when zero.
	LockWait time.Duration
	// Wait bounds the wait for "done" once the snapshot is printed,
	// DefaultWait when zero.
	Wait time.Duration
}

// A Manifest is capture.json.
type Manifest struct {
	Format int `json:"format"`
	// Created is when the database snapshot was taken.
	Created  string `json:"created"`
	Snapshot string `json:"snapshot"`
	// Purge is the latest purge the snapshot holds, or "-".
	Purge string `json:"purge"`
	// Blocks and Bytes count the content blocks copied and their bytes.
	Blocks   int         `json:"blocks"`
	Bytes    int64       `json:"bytes"`
	Version  string      `json:"version"`
	Included []string    `json:"included"`
	Excluded []Exclusion `json:"excluded"`
}

// An Exclusion is state the capture leaves out, and why.
type Exclusion struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// ErrAbandoned is a capture whose caller did not answer done.
var ErrAbandoned = errors.New("capture abandoned")

// excluded lists what no capture copies.
var excluded = []Exclusion{
	{"blobs/source-jobs", "Document conversion jobs: private, temporary state that expires within 24 hours. A job's objects are stored after the database row that names them and removed after it, so no copy matches the snapshot without pausing conversions. A restored instance cannot return these jobs' results; prepare those sources again."},
	{"blobs/exports", "Chunked notebook bundle downloads, which last fifteen minutes; a client starts the download again."},
	{"data/projects", "Each notebook's keyword, history and note map caches, which every gateway rebuilds from the notebook's revisions without provider charges (semantic vectors are in the database), and the previous release's notebook files from before the storage v2 cutover, which that cutover's backup holds."},
	{"data/repository-sources", "Each gateway's Git mirrors, lexical indexes and copies of index generations, rebuilt by fetching the upstream repository. The durable index generations and retained evidence are in blobs/repository-sources."},
	{"data/cache", "Each gateway's working space for conversions, bundles and imports."},
	{"data/automations/python", "Prepared Python environments, which a gateway prepares again on first use; automation definitions and runs are in the database."},
	{"data (other files)", "Lock files, and the state files of releases before the instance database, which that database now holds. Configuration and mounted secrets outside the data directory are deployment assets to back up separately."},
}

type capture struct {
	o        Options
	out      string
	included []string
	excluded []Exclusion
	exports  []string // notebook IDs of the copied exports
	blocks   int
	bytes    int64
}

// Run captures the instance o names into o.Out, prints the snapshot line to
// stdout and waits for done on stdin, as the package comment describes. Out
// must not exist and must not be inside the data directory or the blob
// directory. Run returns once the snapshot ended, with nil only after done;
// whatever it returns, DIR stays for the caller.
func Run(ctx context.Context, o Options, stdin io.Reader, stdout io.Writer) error {
	if o.LockWait <= 0 {
		o.LockWait = DefaultLockWait
	}
	if o.Wait <= 0 {
		o.Wait = DefaultWait
	}
	out, err := newDirectory(o)
	if err != nil {
		return err
	}
	c := &capture{o: o, out: out}
	// Before the snapshot, so no export is newer than the database copy.
	if err := c.copyData(ctx); err != nil {
		return err
	}
	pooled, err := o.DB.Pool.Acquire(ctx)
	if err != nil {
		return err
	}
	// The connection holds a session lock, so it never returns to the pool.
	conn := pooled.Hijack()
	defer func() {
		end, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		conn.Close(end)
	}()
	// The capture's snapshot stays idle in its transaction while the worker
	// dumps the database.
	for _, setting := range []string{`SET application_name = 'forgetmenote capture'`, `SET idle_in_transaction_session_timeout = 0`} {
		if _, err := conn.Exec(ctx, setting); err != nil {
			return err
		}
	}
	locking, cancel := context.WithTimeout(ctx, o.LockWait)
	unlock, err := notestore.LockBlocks(locking, conn, o.DB.Org)
	cancel()
	if err != nil {
		if ctx.Err() == nil && locking.Err() != nil {
			return fmt.Errorf("a block sweep or a purge's removal of content blocks kept them locked for %v; capture again later", o.LockWait)
		}
		return fmt.Errorf("locking the content blocks: %w", err)
	}
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	m := Manifest{Format: Format}
	var created time.Time
	var schema int
	if err := tx.QueryRow(ctx, `SELECT pg_export_snapshot(), now(), (SELECT coalesce(max(version), 0) FROM schema_migrations)`).Scan(&m.Snapshot, &created, &schema); err != nil {
		return err
	}
	m.Created = created.UTC().Format(time.RFC3339)
	m.Version = fmt.Sprintf("%s (schema %d, reader %d)", o.Version, schema, pg.ReaderVersion)
	err = tx.QueryRow(ctx, `SELECT purge_id FROM purges WHERE org = $1 ORDER BY created DESC, notebook DESC, purge_id DESC LIMIT 1`, o.DB.Org).Scan(&m.Purge)
	if errors.Is(err, pgx.ErrNoRows) {
		m.Purge, err = "-", nil
	}
	if err != nil {
		return err
	}
	if err := c.checkExports(ctx, tx); err != nil {
		return err
	}
	// Repository sources first: a refresh may retire a generation the
	// snapshot names, while nothing removes the blocks it uses.
	if err := c.copySources(ctx, tx); err != nil {
		return err
	}
	if err := c.copyBlocks(ctx, tx); err != nil {
		return err
	}
	m.Blocks, m.Bytes = c.blocks, c.bytes
	slices.Sort(c.included)
	m.Included = c.included
	m.Excluded = append(slices.Clone(excluded), c.excluded...)
	if err := writeManifest(filepath.Join(out, "capture.json"), m); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "snapshot %s purge %s\n", m.Snapshot, m.Purge); err != nil {
		return err
	}
	if err := waitForDone(ctx, stdin, o.Wait); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("ending the snapshot: %w", err)
	}
	if err := unlock(ctx); err != nil {
		return fmt.Errorf("releasing the content blocks: %w", err)
	}
	_, err = fmt.Fprintln(stdout, "ok")
	return err
}

// newDirectory creates o.Out, private, refusing a path inside the data
// directory or a blob directory, which the capture copies from.
func newDirectory(o Options) (string, error) {
	out, err := filepath.Abs(o.Out)
	if err != nil {
		return "", err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(out))
	if err != nil {
		return "", err
	}
	out = filepath.Join(parent, filepath.Base(out))
	sources := []string{o.Data}
	if dir, ok := o.Blobs.(*blob.Dir); ok {
		sources = append(sources, dir.Path())
	}
	for _, source := range sources {
		root, err := filepath.EvalSymlinks(source)
		if err != nil {
			return "", err
		}
		if root, err = filepath.Abs(root); err != nil {
			return "", err
		}
		if out == root || strings.HasPrefix(out, root+string(os.PathSeparator)) {
			return "", fmt.Errorf("the capture directory must be outside %s", source)
		}
	}
	if err := os.Mkdir(out, 0o700); err != nil {
		return "", err
	}
	for _, dir := range []string{"data", "blobs"} {
		if err := os.Mkdir(filepath.Join(out, dir), 0o700); err != nil {
			return "", err
		}
	}
	return out, nil
}

func writeManifest(path string, m Manifest) error {
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(append(raw, '\n'))
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return err
}

// waitForDone waits up to wait for one line on stdin and returns nil only
// when it is done.
func waitForDone(ctx context.Context, stdin io.Reader, wait time.Duration) error {
	type answer struct {
		line string
		err  error
	}
	answered := make(chan answer, 1)
	go func() {
		line, err := bufio.NewReaderSize(stdin, 64).ReadSlice('\n')
		answered <- answer{string(line), err}
	}()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case a := <-answered:
		line, ended := strings.CutSuffix(a.line, "\n")
		line = strings.TrimSuffix(line, "\r")
		switch {
		case line == "done" && (ended || errors.Is(a.err, io.EOF)):
			return nil
		case a.line == "" && errors.Is(a.err, io.EOF):
			return fmt.Errorf("%w: the input ended before done", ErrAbandoned)
		case a.err != nil && !errors.Is(a.err, io.EOF):
			return fmt.Errorf("%w: reading the input: %w", ErrAbandoned, a.err)
		default:
			return fmt.Errorf("%w: the input sent another line than done", ErrAbandoned)
		}
	case <-timer.C:
		return fmt.Errorf("%w: no done within %v", ErrAbandoned, wait)
	case <-ctx.Done():
		return fmt.Errorf("%w: %w", ErrAbandoned, context.Cause(ctx))
	}
}
