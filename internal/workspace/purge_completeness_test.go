package workspace

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"forgetmenote/internal/benchfixture"
	"forgetmenote/internal/blob"
	"forgetmenote/internal/gitexport"
	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
	"forgetmenote/internal/pg"
	"forgetmenote/internal/pg/pgtest"

	"github.com/reearth/ygo/crdt"
)

// secretNeedles are the forms a stored copy of secret could take: its bytes,
// as JSON escapes it, hexadecimal, and base64 at each of the three
// alignments.
func secretNeedles(secret string) [][]byte {
	needles := append(jsonForms(secret), []byte(hex.EncodeToString([]byte(secret))), []byte(strings.ToUpper(hex.EncodeToString([]byte(secret)))))
	for pad := range 3 {
		enc := base64.StdEncoding.EncodeToString(append(make([]byte, pad), secret...))
		// Skip the characters the padding bytes share, and the last group.
		needles = append(needles, []byte(enc[(pad*4+2)/3:len(enc)-4]))
	}
	return needles
}

func findSecret(t *testing.T, where string, data []byte, secret string) {
	t.Helper()
	for _, needle := range secretNeedles(secret) {
		if i := bytes.Index(data, needle); i >= 0 {
			t.Errorf("%s holds the secret as %q at byte %d of %d", where, needle[:min(len(needle), 16)], i, len(data))
		}
	}
}

// databaseBytes is every row of every table in db's database, whatever its
// org, as a binary COPY gives them.
func databaseBytes(t *testing.T, db *pg.DB) []byte {
	t.Helper()
	conn, err := db.Pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	rows, err := conn.Query(t.Context(), `SELECT schemaname, tablename FROM pg_tables WHERE schemaname NOT IN ('pg_catalog', 'information_schema') ORDER BY 1, 2`)
	if err != nil {
		t.Fatal(err)
	}
	var tables [][2]string
	for rows.Next() {
		var schema, table string
		rows.Scan(&schema, &table)
		tables = append(tables, [2]string{schema, table})
	}
	rows.Close()
	if len(tables) < 20 {
		t.Fatal("tables:", tables)
	}
	var out bytes.Buffer
	for _, table := range tables {
		if _, err := conn.Conn().PgConn().CopyTo(t.Context(), &out, `COPY "`+table[0]+`"."`+table[1]+`" TO STDOUT WITH (FORMAT binary)`); err != nil {
			t.Fatal(table, err)
		}
	}
	return out.Bytes()
}

func treeBytes(t *testing.T, dir string) []byte {
	t.Helper()
	var out bytes.Buffer
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, err := os.ReadFile(p)
		out.Write(raw)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// exportObjects is every object of the export repository at path,
// decompressed, as git cat-file --batch-all-objects prints them.
func exportObjects(t *testing.T, path string) []byte {
	t.Helper()
	cmd := exec.Command("git", "--git-dir="+path, "cat-file", "--batch-all-objects", "--unordered", "--batch")
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// The purge completeness test: a secret planted in 300 revisions of an F2
// notebook, in notes, discussions (escaped, as JSON
// stores its quote, backslash, ampersand and angle brackets) and commit
// messages, and in a live note's typed, compacted and pending edits, is
// purged by redaction; then no stored byte holds it in any form: no
// Postgres row, block, workspace state, live document or object of the Git
// export. Old revisions resolve to their replacements and the export still
// round-trips. FMN_PURGE_F2=full runs the full 20,000-revision F2.
func TestPurgeLeavesNoStoredCopyOfARedactedSecret(t *testing.T) {
	if testing.Short() {
		t.Skip("replays a generated notebook")
	}
	o := benchfixture.NotebookOptions{Notes: 60, Assets: 6, Revisions: 1000, MinAssetBytes: 4 << 10, MaxAssetBytes: 16 << 10}
	if os.Getenv("FMN_PURGE_F2") == "full" {
		o = benchfixture.NotebookOptions{}
	}
	revs, err := benchfixture.Notebook(o)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	db := pgtest.Database(t)
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "blobs"), 0o700); err != nil {
		t.Fatal(err)
	}
	blobs, err := blob.OpenDir(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { blobs.Close() })
	notes, err := notestore.Open(ctx, db, blobs, notestore.Options{ImportEntryID: NoteEntryID})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(notes.Close)
	state := filepath.Join(dir, "projects", "f2")
	s, err := CreateDB(ctx, notes, "f2", state, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	ex, err := gitexport.New(notes, filepath.Join(dir, "exports"), gitexport.Options{Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ex.Close)
	n := notes.Notebook("f2")
	secret := jsonSecret(t)
	escaped := jsonForms(secret)[1]

	// Replay F2, planting the secret in 300 revisions: in every note and
	// discussion they write, and in some of their messages.
	started := time.Now()
	exists := map[string]bool{}
	var planted []string
	for rev := range revs {
		if rev.Kind == benchfixture.KindInit {
			continue
		}
		plant := rev.Seq > 40 && len(planted) < 300
		cs := notestore.ChangeSet{Author: gitobj.Signature{Name: rev.AuthorName, Email: rev.AuthorEmail, When: rev.Time.Unix(), TZ: rev.Time.Format("-0700")},
			Subject: rev.Subject, AllowEmpty: true}
		for _, tr := range rev.Trailers {
			cs.Trailers = append(cs.Trailers, gitobj.Trailer{Key: tr.Key, Value: tr.Value})
		}
		did := false
		for _, c := range rev.Changes {
			fc := notestore.FileChange{Path: c.Path, Content: c.Content, From: c.OldPath}
			if plant && c.Content != nil {
				if strings.HasSuffix(c.Path, ".md") {
					fc.Content, did = append(bytes.Clone(c.Content), "\nAPI key: "+secret+"\n"...), true
				} else if i := bytes.Index(c.Content, []byte(`"text": "`)); strings.HasPrefix(c.Path, "comments/") && i >= 0 {
					i += len(`"text": "`)
					fc.Content, did = slices.Concat(c.Content[:i], escaped, []byte(" "), c.Content[i:]), true
				}
			}
			if c.Content != nil && c.OldPath == "" && !exists[c.Path] {
				fc.EntryID = NoteEntryID(c.Path, fc.Content)
			}
			if c.OldPath != "" {
				delete(exists, c.OldPath)
			}
			exists[c.Path] = c.Content != nil
			cs.Files = append(cs.Files, fc)
		}
		if did && len(planted)%30 == 0 {
			cs.Subject += " (" + secret + ")"
		}
		r, err := n.Apply(ctx, cs)
		if err != nil {
			t.Fatalf("revision %d (%s): %v", rev.Seq, rev.Kind, err)
		}
		if did {
			planted = append(planted, r.Head.Commit.String())
		}
	}
	head, err := n.Head(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("replayed %d revisions in %v, the secret in %d of them", head.Seq, time.Since(started).Round(time.Millisecond), len(planted))
	if len(planted) != 300 {
		t.Fatal("planted in", len(planted), "revisions")
	}

	// A live note the secret never reached in Git: typed key by key,
	// compacted into a snapshot, then pasted again and left pending.
	files, err := n.Files(ctx, gitobj.ID{})
	if err != nil {
		t.Fatal(err)
	}
	var live notestore.File
	var liveText []byte
	for _, f := range files {
		if content, _, err := n.Read(ctx, gitobj.ID{}, f.Path); err == nil && strings.HasSuffix(f.Path, ".md") && !IsTrash(f.Path) && NoteEntryID(f.Path, content) == f.EntryID && !bytes.Contains(content, []byte(secret)) {
			live, liveText = f, content
			break
		}
	}
	if live.Path == "" {
		t.Fatal("no note without the secret")
	}
	doc, err := browserDoc(nil)
	if err != nil {
		t.Fatal(err)
	}
	var lastSeq int64
	edit := func(fn func(*crdt.Transaction, *crdt.YText)) {
		t.Helper()
		before := doc.StateVector()
		doc.Transact(func(tx *crdt.Transaction) { fn(tx, tx.GetText("markdown")) })
		if lastSeq, err = n.LiveAppend(ctx, live.EntryID, 0, crdt.EncodeStateAsUpdateV1(doc, before), "ada"); err != nil {
			t.Fatal(err)
		}
	}
	edit(func(tx *crdt.Transaction, text *crdt.YText) { text.Insert(tx, 0, string(liveText), nil) })
	for i, key := range "Typed key: " + secret + "\n" {
		edit(func(tx *crdt.Transaction, text *crdt.YText) { text.Insert(tx, i, string(key), nil) })
	}
	if err := n.LiveCompact(ctx, live.EntryID, 0, crdt.EncodeStateAsUpdateV1(doc, nil), lastSeq, head.Commit); err != nil {
		t.Fatal(err)
	}
	edit(func(tx *crdt.Transaction, text *crdt.YText) { text.Insert(tx, 0, "Pasted: "+secret+"\n", nil) })
	if _, err := ex.Sync(ctx, "f2"); err != nil {
		t.Fatal(err)
	}
	objects, rows, stored := exportObjects(t, ex.Path("f2")), databaseBytes(t, db), treeBytes(t, blobs.Path())
	if !bytes.Contains(objects, []byte(secret)) || !bytes.Contains(rows, []byte(secret)) || !bytes.Contains(stored, []byte(secret)) {
		t.Fatal("the fixture does not store the secret everywhere the scan looks")
	}
	if !bytes.Contains(objects, escaped) || !bytes.Contains(rows, escaped) && !bytes.Contains(stored, escaped) {
		t.Fatal("the fixture does not store the secret escaped")
	}

	scope := PurgeScope{Kind: PurgeText, Text: secret}
	started = time.Now()
	plan, err := s.PreparePurge(ctx, scope, ex)
	if err != nil {
		t.Fatal(err)
	}
	prepared := time.Since(started)
	if plan.Blocked != "" || plan.Versions < 300 || plan.Messages != 10 || !plan.ExportForcePush || len(plan.Unredactable) != 0 || len(plan.Elsewhere) != 0 {
		t.Fatalf("plan: %d files, %d versions, %d messages, blocked %q, force push %v, unredactable %v, elsewhere %v",
			len(plan.Notes), plan.Versions, plan.Messages, plan.Blocked, plan.ExportForcePush, plan.Unredactable, plan.Elsewhere)
	}
	if i := slices.IndexFunc(plan.Live, func(l PurgeLive) bool { return l.Path == live.Path }); i < 0 || !plan.Live[i].Pending {
		t.Fatal("the live note:", plan.Live)
	}
	// The export rebuild ExecutePurge runs with an exporter, timed on its
	// own here; TestExecutePurgeRebuildsTheGitExport covers the two together.
	// The confirmation binds the force push, so this plan is made without
	// the exporter.
	if plan, err = s.PreparePurge(ctx, scope, nil); err != nil || plan.ExportForcePush {
		t.Fatal(plan.ExportForcePush, err)
	}
	started = time.Now()
	result, err := s.ExecutePurge(ctx, plan, plan.Confirmation, "owner", "an API key was pasted into the notebook", nil)
	if err != nil {
		t.Fatal(err)
	}
	executed := time.Since(started)
	started = time.Now()
	if err := ex.Rebuild(ctx, "f2", result.Purge); err != nil {
		t.Fatal(err)
	}
	rebuilt := time.Since(started)
	t.Logf("purge of %d revisions (%d file versions in %d files, %d messages, %d live notes): plan %v; execute %v (planning again and rewriting); export rebuild %v; removed %d versions and %d blocks",
		result.Rewritten, plan.Versions, len(plan.Notes), plan.Messages, result.LiveReset, prepared.Round(time.Millisecond), executed.Round(time.Millisecond), rebuilt.Round(time.Millisecond), result.RemovedVersions, result.RemovedBlocks)
	if result.Rewritten != plan.RevisionsRewritten {
		t.Fatalf("result %+v", result)
	}

	// Every stored byte.
	findSecret(t, "a Postgres row", databaseBytes(t, db), secret)
	findSecret(t, "the blob store", treeBytes(t, blobs.Path()), secret)
	findSecret(t, "the workspace state", treeBytes(t, state), secret)
	findSecret(t, "an export object", exportObjects(t, ex.Path("f2")), secret)
	findSecret(t, "an export file", treeBytes(t, ex.Path("f2")), secret)
	st, err := n.LiveState(ctx, live.EntryID)
	if err != nil || st.Epoch != 1 || st.Snapshot != nil || len(st.Updates) != 0 {
		t.Fatalf("the live note: %+v %v", st, err)
	}

	// References to old revisions resolve through the mapping.
	newHead, err := n.Head(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got, purged := resolved(t, s, "", head.Commit.String()); got != newHead.Commit.String() || purged {
		t.Fatal("the old head resolves to", got, purged)
	}
	for _, old := range planted {
		next, purged := resolved(t, s, "", old)
		id, _ := parseRevision(next)
		info, err := n.Commit(ctx, id)
		if err != nil || purged || !slices.ContainsFunc(info.Trailers, func(tr gitobj.Trailer) bool { return tr.Key == notestore.PurgeTrailer && tr.Value == result.Purge }) {
			t.Fatal("planted revision", old, "resolves to", next, purged, err)
		}
	}
	for _, c := range []string{planted[0], planted[len(planted)-1]} {
		next, _ := resolved(t, s, "", c)
		id, _ := parseRevision(next)
		info, err := n.Commit(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		for _, ch := range info.Changes {
			if strings.HasSuffix(ch.Path, ".md") && ch.Op != notestore.OpDelete {
				if content, _, err := n.Read(ctx, id, ch.Path); err != nil || !bytes.Contains(content, []byte("API key: [purged]")) {
					t.Fatalf("%s at %s: %v", ch.Path, next, err)
				}
			}
		}
	}

	// The export round trip still holds: the rebuilt export verifies
	// against the store, imports into a new notebook with the same head, and
	// carries the purge flag for the backup worker.
	repo := ex.Path("f2")
	if _, err := gitexport.Verify(ctx, n, repo); err != nil {
		t.Fatal("the rebuilt export:", err)
	}
	copied := notes.Notebook("f2-copy")
	if got, err := gitexport.ImportGitDir(ctx, copied, repo); err != nil || got != newHead {
		t.Fatal("importing the export:", got, newHead, err)
	}
	flag, ok, err := gitexport.ReadPurge(repo)
	if err != nil || !ok || flag.Purge != result.Purge || flag.Head != newHead.Commit || !slices.Contains(flag.Replaces, head.Commit) {
		t.Fatalf("purge flag %+v %v %v", flag, ok, err)
	}
	if got, err := ex.Sync(ctx, "f2"); err != nil || got != newHead {
		t.Fatal("the export after the purge:", got, err)
	}
}
