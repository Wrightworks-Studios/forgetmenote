package web

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"forgetmenote/internal/blob/blobtest"
	"forgetmenote/internal/pg/pgtest"
	"forgetmenote/internal/purgehttp"
	"forgetmenote/internal/workspace"
)

func TestExportPinnedAcrossSaves(t *testing.T) {
	app, store := fixture(t)
	ctx := context.Background()
	if _, err := store.Save(ctx, "a.md", []byte("before"), "", ""); err != nil {
		t.Fatal(err)
	}
	first, err := app.backupChunk(ctx, store, "default", 0, 100, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Save(ctx, "later.md", []byte("after"), "", ""); err != nil {
		t.Fatal(err)
	}
	data, _ := base64.StdEncoding.DecodeString(first.Data)
	for offset := first.NextOffset; offset < first.Total; {
		part, err := app.backupChunk(ctx, store, "default", offset, 262144, first.SHA256)
		if err != nil {
			t.Fatalf("snapshot changed: %v", err)
		}
		raw, _ := base64.StdEncoding.DecodeString(part.Data)
		data = append(data, raw...)
		offset = part.NextOffset
	}
	if workspace.ContentHash(data) != first.SHA256 {
		t.Fatal("inconsistent bundle")
	}
}

func TestExportScopeExpiryAndCapacity(t *testing.T) {
	app, store := fixture(t)
	ctx := context.Background()
	store.Save(ctx, "a.md", []byte("example"), "", "")
	first, err := app.backupChunk(ctx, store, "default", 0, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	foreign := context.WithValue(ctx, identityKey{}, "another-user")
	if _, err = app.backupChunk(foreign, store, "default", 1, 1, first.SHA256); err == nil {
		t.Fatal("another user consumed snapshot")
	}
	if _, err = app.db.Pool.Exec(ctx, `UPDATE exports SET expires = now() - interval '1 second' WHERE org = $1`, app.db.Org); err != nil {
		t.Fatal(err)
	}
	if _, err = app.backupChunk(ctx, store, "default", 1, 1, first.SHA256); err == nil {
		t.Fatal("expired snapshot accepted")
	}
	app.expireExports()
	if n := storedExports(t, app); n != 0 {
		t.Fatal("expired export leaked", n)
	}
	files, _ := filepath.Glob(filepath.Join(app.data, "cache", "exports", "bundle-*"))
	if len(files) > 0 {
		t.Fatal("temporary bundle leaked")
	}
}

// storedExports counts the export rows and stored bundles of app's instance.
func storedExports(t *testing.T, app *Server) int {
	t.Helper()
	var n int
	if err := app.db.Pool.QueryRow(context.Background(), `SELECT count(*) FROM exports WHERE org = $1`, app.db.Org).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n + len(exportBundles(t, app))
}

// exportBundles lists the bundles of MCP exports in app's blob store, which
// lie directly under exports/. With blobs in the data directory, exports/
// also holds the notebooks' Git exports (exports/ID.git), which backups
// bundle from and which are not exports of this kind.
func exportBundles(t *testing.T, app *Server) []string {
	t.Helper()
	keys, err := app.blobs.List(context.Background(), "exports/")
	if err != nil {
		t.Fatal(err)
	}
	var bundles []string
	for _, key := range keys {
		if !strings.Contains(strings.TrimPrefix(key, "exports/"), "/") {
			bundles = append(bundles, key)
		}
	}
	return bundles
}

// An export begun on one gateway continues on another, chunk boundaries need
// not match stored pieces, and the instance holds at most two exports.
func TestExportContinuesOnAnotherGateway(t *testing.T) {
	app, store := fixture(t)
	exportAcrossGateways(t, app, gatewayOf(t, app), store)
}

// Gateways with disks of their own share exports through S3.
func TestExportContinuesThroughS3OnAnotherGateway(t *testing.T) {
	app, store := fixture(t)
	s3 := blobtest.Start(t)
	first := gatewayWith(t, app, s3.Store(t, "instance"))
	exportAcrossGateways(t, first, gatewayWith(t, app, s3.Store(t, "instance")), store)
	if leftovers := s3.Objects(); len(leftovers) != 2 {
		t.Fatalf("stored bundles: %d, want the two active exports", len(leftovers))
	}
	if bundles := exportBundles(t, app); len(bundles) != 0 {
		t.Fatal("an export was stored on a gateway's own disk", bundles)
	}
	// A bundle whose row is gone, as after a crash, is swept.
	app.db.Pool.Exec(context.Background(), `DELETE FROM exports WHERE org = $1`, app.db.Org)
	first.expireExports()
	if leftovers := s3.Objects(); len(leftovers) != 0 {
		t.Fatalf("orphaned bundles remain: %d", len(leftovers))
	}
}

func exportAcrossGateways(t *testing.T, app, other *Server, store *workspace.Store) {
	t.Helper()
	ctx := context.Background()
	noise := make([]byte, 600<<10)
	rand.Read(noise)
	if _, err := store.Save(ctx, "a.md", []byte(base64.StdEncoding.EncodeToString(noise)), "", ""); err != nil {
		t.Fatal(err)
	}
	first, err := app.backupChunk(ctx, store, "default", 0, 1000, "")
	if err != nil {
		t.Fatal(err)
	}
	if first.Total <= 2*exportPiece {
		t.Fatal("fixture bundle spans too few pieces", first.Total)
	}
	data, _ := base64.StdEncoding.DecodeString(first.Data)
	for offset := first.NextOffset; offset < first.Total; {
		part, err := other.backupChunk(ctx, store, "default", offset, exportPiece, first.SHA256)
		if err != nil {
			t.Fatalf("another gateway could not continue the export: %v", err)
		}
		raw, _ := base64.StdEncoding.DecodeString(part.Data)
		data = append(data, raw...)
		offset = part.NextOffset
	}
	if int64(len(data)) != first.Total || workspace.ContentHash(data) != first.SHA256 {
		t.Fatal("the export assembled across gateways differs from its bundle")
	}
	if n := storedExports(t, app); n != 0 {
		t.Fatal("a completed export remained stored", n)
	}
	for i, user := range []string{"alice", "bob"} {
		gateway := []*Server{app, other}[i]
		if _, err = gateway.backupChunk(context.WithValue(ctx, identityKey{}, user), store, "default", 0, 1, ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = app.backupChunk(context.WithValue(ctx, identityKey{}, "carol"), store, "default", 0, 1, ""); err == nil || !strings.Contains(err.Error(), "already active") {
		t.Fatal("a third export began while two were active on other gateways", err)
	}
}

// An export whose bundle was made before a purge, and which is recorded only
// after the purge ended the exports in progress, ends too: nothing of the
// notebook as it was is served or stays stored, and the next export holds
// the purged notebook.
func TestExportMadeBeforeAPurgeEnds(t *testing.T) {
	app, store := fixture(t)
	ctx := context.Background()
	secret := webPurgeSecret()
	savePurgeNote(t, store, "notes/keys.md", "# Keys\n\nThe staging key is "+secret+".\n", "")

	// The export makes its bundle, then waits to record it while the purge
	// begins, which waits for it and ends it.
	_, release := pgtest.HoldWrites(t, app.db, "exports")
	type result struct {
		chunk binaryChunk
		err   error
	}
	began := make(chan result, 1)
	go func() {
		chunk, err := app.backupChunk(ctx, store, "default", 0, exportPiece, "")
		began <- result{chunk, err}
	}()
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		var held int
		app.db.Pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND query LIKE 'INSERT INTO exports%'`).Scan(&held)
		if held == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the export never came to record its bundle")
		}
	}
	// Planned now: the plan covers the Git export the bundle came from.
	plan := preparePurgeHTTP(t, app, workspace.PurgeScope{Kind: workspace.PurgeText, Text: secret})
	purged := make(chan purgeOutcome, 1)
	go func() { purged <- executePurgeOutcome(app, plan) }()
	waitForExportLock(t, app, `SELECT pg_advisory_xact_lock(hashtextextended($1 || %`)
	release()
	// Recorded before the purge ended it, the export may serve its first
	// chunk before the purge rewrites anything, and nothing once it has.
	got := <-began
	if got.err != nil && (!errors.Is(got.err, errExportPurged) && !errors.Is(got.err, errExportGone) || got.chunk.Data != "") {
		t.Fatalf("an export made before the purge: %v, %d bytes served", got.err, len(got.chunk.Data))
	}
	if done := <-purged; done.code != 200 || len(done.receipt.Warnings) > 0 || done.receipt.Rewritten == 0 {
		t.Fatalf("execute: HTTP %d %s", done.code, done.body)
	}
	if got.err == nil {
		if next, err := app.backupChunk(ctx, store, "default", got.chunk.NextOffset, exportPiece, got.chunk.SHA256); !errors.Is(err, errExportGone) || next.Data != "" {
			t.Fatalf("an export made before the purge, after it: %v, %d bytes served", err, len(next.Data))
		}
	}
	if n := storedExports(t, app); n != 0 {
		t.Fatal("the export made before the purge remains stored:", n)
	}

	// Exporting again gives the notebook as purged.
	first, err := app.backupChunk(ctx, store, "default", 0, exportPiece, "")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := base64.StdEncoding.DecodeString(first.Data)
	for offset := first.NextOffset; offset < first.Total; {
		part, err := app.backupChunk(ctx, store, "default", offset, exportPiece, first.SHA256)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := base64.StdEncoding.DecodeString(part.Data)
		data = append(data, raw...)
		offset = part.NextOffset
	}
	bundle, repo := filepath.Join(t.TempDir(), "notes.bundle"), filepath.Join(t.TempDir(), "notes.git")
	if err := os.WriteFile(bundle, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "clone", "--quiet", "--bare", bundle, repo).CombinedOutput(); err != nil {
		t.Fatalf("clone the export: %v\n%s", err, out)
	}
	objects, err := exec.Command("git", "--git-dir="+repo, "cat-file", "--batch-all-objects", "--unordered", "--batch").Output()
	if err != nil || !bytes.Contains(objects, []byte("The staging key is")) {
		t.Fatal("the export holds no version of the note:", err)
	}
	findPurged(t, "the export after the purge", objects, secret)
}

type purgeOutcome struct {
	code    int
	receipt purgehttp.Receipt
	body    string
}

func executePurgeOutcome(app *Server, plan webPurgePlan) purgeOutcome {
	code, receipt, body := executePurgeHTTP(app, plan, "Leaked staging key")
	return purgeOutcome{code, receipt, body}
}

// waitForExportLock returns once a statement of app's database whose text
// is like query waits for a lock.
func waitForExportLock(t *testing.T, app *Server, query string) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		var waiting int
		app.db.Pool.QueryRow(context.Background(), `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock' AND query LIKE $1`, query).Scan(&waiting)
		if waiting > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("nothing waits for a lock running %q", query)
		}
	}
}

// Once a purge has rewritten a notebook's history, an export in progress
// serves no further chunk, even before the purge has cleaned up after
// itself: the purge ended it before the rewrite.
func TestExportServesNothingOnceAPurgeRewroteItsNotebook(t *testing.T) {
	app, store := fixture(t)
	ctx := context.Background()
	secret := webPurgeSecret()
	savePurgeNote(t, store, "notes/keys.md", "# Keys\n\nThe staging key is "+secret+".\n", "")
	first, err := app.backupChunk(ctx, store, "default", 0, 64, "")
	if err != nil || first.NextOffset >= first.Total {
		t.Fatal("the first chunk:", first.NextOffset, first.Total, err)
	}
	plan := preparePurgeHTTP(t, app, workspace.PurgeScope{Kind: workspace.PurgeText, Text: secret})

	// The purge is held once it has rewritten the history, while it rebuilds
	// the notebook's Git export, before its clean-up.
	began, release := holdGit(t, "prune")
	purged := make(chan purgeOutcome, 1)
	go func() { purged <- executePurgeOutcome(app, plan) }()
	for deadline := time.Now().Add(30 * time.Second); !began(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the purge never rebuilt the export")
		}
	}
	if status, err := store.Status(ctx); err != nil || status.Revision == plan.Revision {
		t.Fatal("the purge has not rewritten the notebook:", status.Revision, err)
	}
	next, err := app.backupChunk(ctx, store, "default", first.NextOffset, 64, first.SHA256)
	release()
	if !errors.Is(err, errExportGone) || next.Data != "" {
		t.Fatalf("an export in progress after the purge's rewrite: %v, %d bytes served", err, len(next.Data))
	}
	if done := <-purged; done.code != 200 || len(done.receipt.Warnings) > 0 || done.receipt.Rewritten == 0 {
		t.Fatalf("execute: HTTP %d %s", done.code, done.body)
	}
}

// An export that makes its bundle while a purge rewrites the notebook waits
// to record it until the purge is done, and then ends itself; one that would
// wait too long for the purge is asked to start again.
func TestExportWaitsForAPurgeThatRewritesItsNotebook(t *testing.T) {
	app, store := fixture(t)
	ctx := context.Background()
	defer func(wait time.Duration) { exportLockWait = wait }(exportLockWait)
	secret := webPurgeSecret()
	savePurgeNote(t, store, "notes/keys.md", "# Keys\n\nThe staging key is "+secret+".\n", "")

	exportLockWait = 200 * time.Millisecond
	fence, err := app.db.Exclusive(ctx, "exports")
	if err != nil {
		t.Fatal(err)
	}
	chunk, err := app.backupChunk(ctx, store, "default", 0, exportPiece, "")
	fence()
	if !errors.Is(err, errExportFenced) || chunk.Data != "" {
		t.Fatalf("an export that waited too long for a purge: %v, %d bytes", err, len(chunk.Data))
	}

	// The purge's rewrite is held, as a slow database would, while it holds
	// the export lock.
	exportLockWait = time.Minute
	plan := preparePurgeHTTP(t, app, workspace.PurgeScope{Kind: workspace.PurgeText, Text: secret})
	_, hold := pgtest.HoldWrites(t, pgtest.Dir(t, app.data), "notebook_heads")
	purged := make(chan purgeOutcome, 1)
	go func() { purged <- executePurgeOutcome(app, plan) }()
	waitForExportLock(t, app, `UPDATE notebook_heads%`)
	type result struct {
		chunk binaryChunk
		err   error
	}
	exported := make(chan result, 1)
	go func() {
		chunk, err := app.backupChunk(ctx, store, "default", 0, exportPiece, "")
		exported <- result{chunk, err}
	}()
	waitForExportLock(t, app, `SELECT pg_advisory_xact_lock(hashtextextended('exports ' || $1%`)
	hold()
	got := <-exported
	if !errors.Is(got.err, errExportPurged) || got.chunk.Data != "" {
		t.Fatalf("an export made while the purge rewrote the notebook: %v, %d bytes served", got.err, len(got.chunk.Data))
	}
	if done := <-purged; done.code != 200 || len(done.receipt.Warnings) > 0 || done.receipt.Rewritten == 0 {
		t.Fatalf("execute: HTTP %d %s", done.code, done.body)
	}
	if n := storedExports(t, app); n != 0 {
		t.Fatal("the export made during the purge remains stored:", n)
	}
}
