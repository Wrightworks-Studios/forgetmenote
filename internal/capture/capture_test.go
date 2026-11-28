package capture_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"forgetmenote/internal/blob"
	"forgetmenote/internal/capture"
	"forgetmenote/internal/gitexport"
	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
	"forgetmenote/internal/pg"
	"forgetmenote/internal/pg/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

var ada = gitobj.Signature{Name: "Ada Author", Email: "ada@example.com", When: 1700000000, TZ: "+0100"}

// instance is a running instance in the default layout: a database of its
// own, the blob store at the data directory and the Git exports under it.
type instance struct {
	t       *testing.T
	ctx     context.Context
	url     string
	db      *pg.DB
	data    string
	blobs   *blob.Dir
	store   *notestore.Store
	exports *gitexport.Exporter
}

func newInstance(t *testing.T) *instance {
	t.Helper()
	in := &instance{t: t, ctx: context.Background(), url: pgtest.URL(t, true), data: t.TempDir()}
	var err error
	if in.db, err = pg.Open(in.ctx, in.url, ""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(in.db.Close)
	if in.blobs, err = blob.OpenDir(in.data); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { in.blobs.Close() })
	in.store, in.exports = in.open(in.db, in.blobs, in.data)
	for name, content := range map[string]string{"admin.token": "recovery-" + randomHex(t), "proxy.secret": "proxy-" + randomHex(t), "custom.css": "body { color: teal; }\n"} {
		if err := os.WriteFile(filepath.Join(in.data, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return in
}

// open opens a notes store and its exporter, as a gateway does.
func (in *instance) open(db *pg.DB, blobs blob.Store, data string) (*notestore.Store, *gitexport.Exporter) {
	in.t.Helper()
	store, err := notestore.Open(in.ctx, db, blobs, notestore.Options{})
	if err != nil {
		in.t.Fatal(err)
	}
	in.t.Cleanup(store.Close)
	exports, err := gitexport.New(store, filepath.Join(data, "exports"), gitexport.Options{Logf: in.t.Logf})
	if err != nil {
		in.t.Fatal(err)
	}
	in.t.Cleanup(exports.Close)
	return store, exports
}

func randomHex(t *testing.T) string {
	t.Helper()
	raw := make([]byte, 12)
	rand.Read(raw)
	return hex.EncodeToString(raw)
}

// manuscript is a Markdown note of about 60 KiB, so it is stored as
// blocks; secret, when set, is in one paragraph.
func manuscript(title, secret string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", title)
	for i := range 120 {
		fmt.Fprintf(&b, "Paragraph %d of %s. %s\n\n", i, title, strings.Repeat("Call me Ishmael, some years ago. ", 10+i%7))
		if i == 60 && secret != "" {
			fmt.Fprintf(&b, "The vault code is %s.\n\n", secret)
		}
	}
	return b.String()
}

func (in *instance) write(notebook string, files map[string]string) notestore.Head {
	in.t.Helper()
	n := in.store.Notebook(notebook)
	if _, err := n.Head(in.ctx); errors.Is(err, fs.ErrNotExist) {
		if _, err := in.store.Create(in.ctx, notebook, ada); err != nil {
			in.t.Fatal(err)
		}
	}
	var changes []notestore.FileChange
	for _, p := range slices.Sorted(mapsKeys(files)) {
		changes = append(changes, notestore.FileChange{Path: p, Content: []byte(files[p])})
	}
	r, err := n.Apply(in.ctx, notestore.ChangeSet{Subject: "Write notes", Author: ada, Files: changes})
	if err != nil {
		in.t.Fatal(err)
	}
	if _, err := in.exports.Sync(in.ctx, notebook); err != nil {
		in.t.Fatal(err)
	}
	return r.Head
}

func mapsKeys(m map[string]string) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// purge erases secret from notebook's history from revision from on, and
// returns once its rewrite committed; done reports its block removal.
func (in *instance) purge(notebook, id, secret string, from int64) (head notestore.Head, done chan error) {
	in.t.Helper()
	n := in.store.Notebook(notebook)
	before, err := n.Head(in.ctx)
	if err != nil {
		in.t.Fatal(err)
	}
	done = make(chan error, 1)
	go func() {
		_, err := n.Rewrite(in.ctx, from, id, func(f notestore.RewriteFile, content func() ([]byte, error)) ([]byte, bool, error) {
			data, err := content()
			if err != nil || !bytes.Contains(data, []byte(secret)) {
				return nil, true, err
			}
			return bytes.ReplaceAll(data, []byte(secret), []byte("[purged]")), true, nil
		}, notestore.RewriteOptions{ExpectHead: &before.Commit, Actor: "owner", Reason: "a leaked key", Scope: map[string]any{"kind": "text"}})
		done <- err
	}()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		select {
		case err := <-done:
			done <- err
		default:
		}
		if head, err = n.Head(in.ctx); err == nil && head.Commit != before.Commit {
			return head, done
		}
		if time.Now().After(deadline) {
			in.t.Fatal("the purge's rewrite did not commit")
		}
	}
}

// running is a capture waiting for its answer.
type running struct {
	t        *testing.T
	out      string
	snapshot string
	purge    string
	stdin    *io.PipeWriter
	stdout   *bufio.Reader
	result   chan error
}

// startCapture starts a capture and waits for its snapshot line.
func (in *instance) startCapture(o capture.Options) *running {
	in.t.Helper()
	r := in.beginCapture(o)
	r.started(r.stdout.ReadString('\n'))
	return r
}

// beginCapture starts a capture without waiting for it.
func (in *instance) beginCapture(o capture.Options) *running {
	in.t.Helper()
	r := &running{t: in.t, out: filepath.Join(in.t.TempDir(), "capture"), result: make(chan error, 1)}
	stdin, answer := io.Pipe()
	output, stdout := io.Pipe()
	r.stdin, r.stdout = answer, bufio.NewReader(output)
	o.DB, o.Blobs, o.Data, o.Out, o.Version = in.db, in.blobs, in.data, r.out, "forgetmenote test"
	go func() {
		err := capture.Run(in.ctx, o, stdin, stdout)
		stdout.Close()
		r.result <- err
	}()
	in.t.Cleanup(func() { answer.Close() })
	return r
}

// started checks the capture's first line, read up to its newline.
func (r *running) started(line string, err error) {
	r.t.Helper()
	if err != nil {
		r.t.Fatalf("the capture printed %q: %v", line, <-r.result)
	}
	fields := strings.Fields(line)
	if len(fields) != 4 || fields[0] != "snapshot" || fields[2] != "purge" || strings.Count(line, "\n") != 1 {
		r.t.Fatalf("the capture printed %q", line)
	}
	r.snapshot, r.purge = fields[1], fields[3]
}

// finish answers done and returns the capture's manifest.
func (r *running) finish() capture.Manifest {
	r.t.Helper()
	if _, err := io.WriteString(r.stdin, "done\n"); err != nil {
		r.t.Fatal(err)
	}
	rest, _ := io.ReadAll(r.stdout)
	if err := <-r.result; err != nil || string(rest) != "ok\n" {
		r.t.Fatalf("the capture after done: %q, %v", rest, err)
	}
	return r.manifest()
}

func (r *running) manifest() capture.Manifest {
	r.t.Helper()
	raw, err := os.ReadFile(filepath.Join(r.out, "capture.json"))
	if err != nil {
		r.t.Fatal(err)
	}
	var m capture.Manifest
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		r.t.Fatal(err)
	}
	return m
}

// captureBackend is the database backend the running capture holds its
// lock and snapshot on.
func (in *instance) captureBackend() int32 {
	in.t.Helper()
	var pid int32
	if err := in.db.Pool.QueryRow(in.ctx, `SELECT pid FROM pg_stat_activity WHERE application_name = 'forgetmenote capture' AND datname = current_database()`).Scan(&pid); err != nil {
		in.t.Fatal("no capture backend:", err)
	}
	return pid
}

// sweepable reports whether a block sweep could run now.
func (in *instance) sweepable() bool {
	in.t.Helper()
	release, ok, err := in.db.TryExclusive(in.ctx, "notestore-block-sweep")
	if err != nil {
		in.t.Fatal(err)
	}
	if ok {
		release()
	}
	return ok
}

// files lists every file under dir by its slash-separated path.
func files(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		raw, err := os.ReadFile(p)
		out[filepath.ToSlash(rel)] = raw
		return err
	})
	if errors.Is(err, fs.ErrNotExist) {
		return out
	}
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func blockNames(t *testing.T, dir string) []string {
	t.Helper()
	var names []string
	for name := range files(t, filepath.Join(dir, "blocks")) {
		if !strings.HasPrefix(filepath.Base(name), ".") {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

func referenced(t *testing.T, q pg.Querier, org string) []string {
	t.Helper()
	blocks, err := notestore.ReferencedBlocks(context.Background(), q, org)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, b := range blocks {
		names = append(names, strings.TrimPrefix(b.Key, "blocks/"))
	}
	return names
}

// gitObjects is every object, reachable or not, of the repository at dir.
func gitObjects(t *testing.T, dir string) []byte {
	t.Helper()
	cmd := exec.Command("git", "cat-file", "--batch-all-objects", "--batch")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_DIR=" + dir, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git cat-file in %s: %v", dir, err)
	}
	return out
}

func gitBranch(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "refs/heads/main")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_DIR=" + dir, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git rev-parse in %s: %v", dir, err)
	}
	return strings.TrimSpace(string(out))
}

// A capture holds the blocks its snapshot uses while a purge commits, so it
// copies them and the purge's removal of them waits for done; a capture
// after the purge copies no block, export object or file that holds the
// purged content. The printed snapshot is the database as the capture
// copied it, for a second connection too, and no block sweep runs during
// the capture.
func TestACaptureCopiesExactlyWhatItsSnapshotUses(t *testing.T) {
	in := newInstance(t)
	secret := "sk-live-" + randomHex(t)
	in.write("other", map[string]string{"book.md": manuscript("Other", "")})
	before := in.write("notes", map[string]string{"big.md": manuscript("Notes", secret), "keep.md": "Unrelated.\n"})
	blocks := blockNames(t, in.data)

	first := in.startCapture(capture.Options{})
	if in.sweepable() {
		t.Fatal("a block sweep could run during a capture")
	}
	// The purge's rewrite commits during the capture; its removal of the
	// blocks it freed waits for the capture to end.
	purged, removal := in.purge("notes", "purge-one", secret, 2)
	pgtest.WaitingFor(t, in.db, in.captureBackend())(1)
	// The purge stored its redacted blocks; none it freed went.
	if got := blockNames(t, in.data); slices.ContainsFunc(blocks, func(b string) bool { return !slices.Contains(got, b) }) {
		t.Fatal("blocks went from the blob store during a capture")
	}
	// A second connection that imports the printed snapshot sees the
	// database before the purge, as the capture copied it.
	tx, err := in.db.Pool.BeginTx(in.ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(in.ctx, `SET TRANSACTION SNAPSHOT '`+first.snapshot+`'`); err != nil {
		t.Fatal("importing the printed snapshot:", err)
	}
	var head []byte
	if err := tx.QueryRow(in.ctx, `SELECT commit_sha FROM notebook_heads WHERE org = $1 AND notebook = 'notes'`, in.db.Org).Scan(&head); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(head, before.Commit[:]) {
		t.Fatal("the imported snapshot sees the purge, which committed after it")
	}
	snapshotBlocks := referenced(t, tx, in.db.Org)
	tx.Rollback(in.ctx)
	m := first.finish()
	if err := <-removal; err != nil {
		t.Fatal("the purge after the capture:", err)
	}
	if first.purge != "-" || m.Purge != "-" || m.Snapshot != first.snapshot {
		t.Fatalf("a capture before any purge printed %q and recorded %q, %q", first.purge, m.Purge, m.Snapshot)
	}
	copied := blockNames(t, filepath.Join(first.out, "blobs"))
	if !slices.Equal(copied, snapshotBlocks) || !slices.Equal(copied, blocks) || m.Blocks != len(copied) {
		t.Fatalf("the capture copied %d blocks and recorded %d; its snapshot uses %d", len(copied), m.Blocks, len(snapshotBlocks))
	}
	if !bytes.Contains(bytes.Join(mapsValues(files(t, filepath.Join(first.out, "blobs", "blocks"))), nil), []byte(secret)) {
		t.Fatal("the capture before the purge lacks the blocks the purge removed after it")
	}
	if bytes.Contains(bytes.Join(mapsValues(files(t, filepath.Join(in.data, "blocks"))), nil), []byte(secret)) {
		t.Fatal("the purge's removal did not run after the capture")
	}

	// Once the export is rebuilt for the purge, a capture holds the purge
	// and nothing it erased.
	if err := in.exports.Rebuild(in.ctx, "notes", "purge-one"); err != nil {
		t.Fatal(err)
	}
	second := in.startCapture(capture.Options{})
	if got := referenced(t, in.db.Pool, in.db.Org); !slices.Equal(blockNames(t, filepath.Join(second.out, "blobs")), got) {
		t.Fatal("the capture after the purge did not copy exactly the blocks its snapshot uses")
	}
	m = second.finish()
	if second.purge != "purge-one" || m.Purge != "purge-one" {
		t.Fatalf("the capture after the purge printed %q, recorded %q", second.purge, m.Purge)
	}
	for name, raw := range files(t, second.out) {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatalf("%s of the capture after the purge holds the purged content", name)
		}
	}
	export := filepath.Join(second.out, "data", "exports", "notes.git")
	if bytes.Contains(gitObjects(t, export), []byte(secret)) {
		t.Fatal("the copied export holds the purged content")
	}
	if gitBranch(t, export) != purged.Commit.String() {
		t.Fatal("the copied export is not at the purged notebook's head")
	}
	flag, ok, err := gitexport.ReadPurge(export)
	if err != nil || !ok || flag.Purge != "purge-one" {
		t.Fatalf("the copied export's purge flag: %+v %v %v", flag, ok, err)
	}
	checkLayout(t, in, second.out, m)
}

func mapsValues(m map[string][]byte) [][]byte {
	var out [][]byte
	for _, k := range slices.Sorted(func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}) {
		out = append(out, m[k])
	}
	return out
}

// checkLayout checks a finished capture's manifest, secrets, exports and
// permissions.
func checkLayout(t *testing.T, in *instance, out string, m capture.Manifest) {
	t.Helper()
	if m.Format != 1 || m.Snapshot == "" || !strings.Contains(m.Version, "forgetmenote test (schema ") {
		t.Fatalf("manifest: %+v", m)
	}
	if created, err := time.Parse(time.RFC3339, m.Created); err != nil || time.Since(created) > time.Minute {
		t.Fatalf("created %q: %v", m.Created, err)
	}
	var size int64
	for _, raw := range files(t, filepath.Join(out, "blobs", "blocks")) {
		size += int64(len(raw))
	}
	if m.Bytes != size || m.Blocks == 0 {
		t.Fatalf("the manifest counts %d blocks of %d bytes; the capture holds %d bytes", m.Blocks, m.Bytes, size)
	}
	for _, want := range []string{"blobs/blocks", "data/admin.token", "data/custom.css", "data/exports/notes.git", "data/exports/other.git", "data/proxy.secret"} {
		if !slices.Contains(m.Included, want) {
			t.Fatalf("the manifest does not include %s: %v", want, m.Included)
		}
	}
	paths := map[string]bool{}
	for _, e := range m.Excluded {
		if e.Path == "" || e.Reason == "" {
			t.Fatalf("an exclusion without its path or reason: %+v", e)
		}
		paths[e.Path] = true
	}
	for _, want := range []string{"blobs/source-jobs", "blobs/exports", "data/projects", "data/cache"} {
		if !paths[want] {
			t.Fatalf("the manifest does not exclude %s: %+v", want, m.Excluded)
		}
	}
	for _, name := range []string{"admin.token", "proxy.secret", "custom.css"} {
		want, _ := os.ReadFile(filepath.Join(in.data, name))
		if got, err := os.ReadFile(filepath.Join(out, "data", name)); err != nil || !bytes.Equal(got, want) {
			t.Fatalf("the capture's %s: %v", name, err)
		}
	}
	for _, id := range []string{"notes", "other"} {
		export := filepath.Join(out, "data", "exports", id+".git")
		head, err := in.store.Notebook(id).Head(in.ctx)
		if err != nil {
			t.Fatal(err)
		}
		if gitBranch(t, export) != head.Commit.String() {
			t.Fatalf("the copied export of %s is not at its head", id)
		}
		if _, err := gitexport.Verify(in.ctx, in.store.Notebook(id), export); err != nil {
			t.Fatalf("the copied export of %s: %v", id, err)
		}
	}
	err := filepath.WalkDir(out, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		want := os.FileMode(0o600)
		if d.IsDir() {
			want = os.ModeDir | 0o700
		}
		if info.Mode() != want {
			return fmt.Errorf("%s has mode %v", p, info.Mode())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A capture whose input ends, sends another line or never answers in time
// fails, ends its snapshot and releases the blocks at once, and leaves its
// directory for the caller.
func TestAnAbandonedCaptureReleasesItsLock(t *testing.T) {
	in := newInstance(t)
	in.write("notes", map[string]string{"big.md": manuscript("Notes", "")})
	for _, answer := range []func(r *running){
		func(r *running) { r.stdin.Close() },
		func(r *running) { io.WriteString(r.stdin, "abort\n") },
		func(r *running) {}, // the wait runs out
	} {
		r := in.startCapture(capture.Options{Wait: 500 * time.Millisecond})
		if in.sweepable() {
			t.Fatal("a block sweep could run during a capture")
		}
		answer(r)
		rest, _ := io.ReadAll(r.stdout)
		if err := <-r.result; !errors.Is(err, capture.ErrAbandoned) || len(rest) != 0 {
			t.Fatalf("an abandoned capture: %q, %v", rest, err)
		}
		if !in.sweepable() {
			t.Fatal("an abandoned capture kept the blocks locked")
		}
		tx, err := in.db.Pool.BeginTx(in.ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(in.ctx, `SET TRANSACTION SNAPSHOT '`+r.snapshot+`'`); err == nil {
			t.Fatal("an abandoned capture's snapshot can still be imported")
		}
		tx.Rollback(in.ctx)
		if _, err := os.Stat(filepath.Join(r.out, "capture.json")); err != nil {
			t.Fatal("an abandoned capture removed its directory:", err)
		}
	}
}

// A capture waits for a running block sweep to end before its snapshot,
// within its bound.
func TestACaptureWaitsForARunningSweep(t *testing.T) {
	in := newInstance(t)
	in.write("notes", map[string]string{"big.md": manuscript("Notes", "")})
	release, ok, err := in.db.TryExclusive(in.ctx, "notestore-block-sweep")
	if err != nil || !ok {
		t.Fatal("the sweep's lock:", ok, err)
	}
	t.Cleanup(release)
	err = capture.Run(in.ctx, capture.Options{DB: in.db, Blobs: in.blobs, Data: in.data, Out: filepath.Join(t.TempDir(), "capture"), LockWait: 300 * time.Millisecond}, strings.NewReader(""), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "kept them locked") {
		t.Fatal("a capture while a sweep ran:", err)
	}
	r := in.beginCapture(capture.Options{})
	type printed struct {
		line string
		err  error
	}
	first := make(chan printed, 1)
	go func() {
		line, err := r.stdout.ReadString('\n')
		first <- printed{line, err}
	}()
	select {
	case p := <-first:
		t.Fatalf("the capture printed %q while a sweep ran: %v", p.line, p.err)
	case <-time.After(300 * time.Millisecond):
	}
	release()
	p := <-first
	r.started(p.line, p.err)
	r.finish()
}

// The export of a notebook a purge rewrote before the export caught up
// names a commit the snapshot lacks and may hold what the purge erased, so
// the capture leaves it out and says so.
func TestACaptureLeavesOutAnExportAPurgeOutran(t *testing.T) {
	in := newInstance(t)
	secret := "sk-live-" + randomHex(t)
	in.write("notes", map[string]string{"big.md": manuscript("Notes", secret)})
	in.write("other", map[string]string{"book.md": manuscript("Other", "")})
	_, removal := in.purge("notes", "purge-late", secret, 2)
	if err := <-removal; err != nil {
		t.Fatal(err)
	}
	r := in.startCapture(capture.Options{})
	m := r.finish()
	if _, err := os.Stat(filepath.Join(r.out, "data", "exports", "notes.git")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("the capture kept an export the purge outran:", err)
	}
	if slices.Contains(m.Included, "data/exports/notes.git") || !slices.Contains(m.Included, "data/exports/other.git") {
		t.Fatalf("included: %v", m.Included)
	}
	if !slices.ContainsFunc(m.Excluded, func(e capture.Exclusion) bool {
		return e.Path == "data/exports/notes.git" && strings.Contains(e.Reason, "purge")
	}) {
		t.Fatalf("the manifest does not say the export was left out: %+v", m.Excluded)
	}
	for name, raw := range files(t, r.out) {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatalf("%s holds the purged content", name)
		}
	}
}

// A capture's blocks and data with a dump of its snapshot restore the
// instance as the capture found it, though a purge removed blocks from the
// live blob store right after it.
func TestACaptureRestoresTheInstance(t *testing.T) {
	dump, err := pgTool("pg_dump")
	if err != nil {
		t.Fatal(err)
	}
	psql, err := pgTool("psql")
	if err != nil {
		t.Fatal(err)
	}
	in := newInstance(t)
	secret := "sk-live-" + randomHex(t)
	book := manuscript("Notes", secret)
	head := in.write("notes", map[string]string{"big.md": book, "keep.md": "Unrelated.\n"})
	r := in.startCapture(capture.Options{})
	sql := filepath.Join(t.TempDir(), "instance.sql")
	if out, err := exec.Command(dump, "--snapshot="+r.snapshot, "--no-owner", "--file="+sql, clientURL(t, in.url)).CombinedOutput(); err != nil {
		t.Fatalf("pg_dump: %v\n%s", err, out)
	}
	r.finish()
	// After the capture the live instance moves on: a purge removes the
	// blocks the capture copied.
	_, removal := in.purge("notes", "purge-after", secret, 2)
	if err := <-removal; err != nil {
		t.Fatal(err)
	}

	restored := pgtest.URL(t, false)
	if out, err := exec.Command(psql, "-X", "-q", "-v", "ON_ERROR_STOP=1", "--file="+sql, clientURL(t, restored)).CombinedOutput(); err != nil {
		t.Fatalf("psql: %v\n%s", err, out)
	}
	db, err := pg.Open(in.ctx, restored, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	if err := db.Check(in.ctx); err != nil {
		t.Fatal(err)
	}
	data := t.TempDir()
	for _, part := range []string{"blobs", "data"} {
		if err := os.CopyFS(data, os.DirFS(filepath.Join(r.out, part))); err != nil {
			t.Fatal(err)
		}
	}
	blobs, err := blob.OpenDir(data)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { blobs.Close() })
	store, exports := in.open(db, blobs, data)
	got, _, err := store.Notebook("notes").Read(in.ctx, head.Commit, "big.md")
	if err != nil || string(got) != book {
		t.Fatalf("the restored notebook reads %d bytes: %v", len(got), err)
	}
	synced, err := exports.Sync(in.ctx, "notes")
	if err != nil || synced.Commit != head.Commit {
		t.Fatalf("the restored export: %v, %v", synced, err)
	}
	if _, err := gitexport.Verify(in.ctx, store.Notebook("notes"), exports.Path("notes")); err != nil {
		t.Fatal(err)
	}
}

// clientURL is address without the options only pgx reads, which the
// Postgres clients refuse.
func clientURL(t *testing.T, address string) string {
	t.Helper()
	u, err := url.Parse(address)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	for key := range q {
		if strings.HasPrefix(key, "pool_") {
			q.Del(key)
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// pgTool finds a Postgres client beside the server the tests run, as
// Ubuntu's package keeps them off PATH together.
func pgTool(name string) (string, error) {
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	server, err := exec.LookPath("postgres")
	if err != nil {
		return "", fmt.Errorf("%s is required for the restore test: %w", name, err)
	}
	p := filepath.Join(filepath.Dir(server), name)
	if _, err := os.Stat(p); err != nil {
		return "", fmt.Errorf("%s is required for the restore test: %w", name, err)
	}
	return p, nil
}

// A capture copies the index generation each repository source's state
// names and its retained evidence, and none of the gateway's caches beside
// them; a named generation a refresh retired meanwhile is left out and
// reported.
func TestACaptureCopiesRepositorySourceFiles(t *testing.T) {
	in := newInstance(t)
	in.write("notes", map[string]string{"big.md": manuscript("Notes", "")})
	put := func(key, content string) {
		t.Helper()
		if err := blob.Write(in.ctx, in.blobs, key, []byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	sum := func(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
	state := `{"version":1,"active":"main","sources":{"main":{"id":"main","index":{"generation":"gen-current-1"}},"docs":{"id":"docs","index":{"generation":"gen-retired-2"}}}}`
	if _, err := in.db.WriteDocument(in.ctx, in.db.Pool, "repository-sources", "code", 0, []byte(state)); err != nil {
		t.Fatal(err)
	}
	base := "repository-sources/code/main/"
	evidence := "fn main() {}\n"
	want := map[string]string{
		base + "index/generations/gen-current-1/manifest.json": `{"mapVersion":"v1"}`,
		base + "index/generations/gen-current-1/chunks.jsonl":  "{}\n",
		base + "index/generations/gen-current-1/map.json":      "{}",
		base + "index/generations/gen-current-1/vectors.json":  "{}",
		base + "index/generations/gen-current-1/vectors.f32":   "vectors",
		base + "evidence/blobs/" + sum(evidence):               evidence,
		base + "evidence/refs/" + sum("ref") + ".json":         `{"path":"main.go"}`,
	}
	for key, content := range want {
		put(key, content)
	}
	put(base+"index/generations/gen-old-0/manifest.json", "{}")
	put(base+"source.git/objects/ab/cdef", "a mirror object")
	put(base+"index/current.json", `{"generation":"gen-current-1"}`)
	put("repository-sources/code/docs/index/generations/gen-retired-2/chunks.jsonl", "{}\n")
	put("source-jobs/job1/original", "a source job")

	r := in.startCapture(capture.Options{})
	m := r.finish()
	got := map[string]string{}
	for name, raw := range files(t, filepath.Join(r.out, "blobs")) {
		if !strings.HasPrefix(name, "blocks/") {
			got[name] = string(raw)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("the capture holds %v", slices.Sorted(func(yield func(string) bool) {
			for k := range got {
				if !yield(k) {
					return
				}
			}
		}))
	}
	for key, content := range want {
		if got[key] != content {
			t.Fatalf("the capture's %s is %q", key, got[key])
		}
	}
	if !slices.Contains(m.Included, "blobs/repository-sources") {
		t.Fatalf("included: %v", m.Included)
	}
	if !slices.ContainsFunc(m.Excluded, func(e capture.Exclusion) bool {
		return e.Path == "blobs/repository-sources/code/docs/index/generations/gen-retired-2" && strings.Contains(e.Reason, "charge")
	}) {
		t.Fatalf("the manifest does not report the retired generation: %+v", m.Excluded)
	}
}
