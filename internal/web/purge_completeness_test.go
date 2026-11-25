package web

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/knowledge"
	"forgetmenote/internal/pg"
	"forgetmenote/internal/workspace"
)

// purgeNeedles are the forms a stored copy of secret could take: its bytes,
// as JSON escapes it with and without HTML escaping, hexadecimal, and base64
// at each of the three alignments.
func purgeNeedles(secret string) [][]byte {
	plain, _ := json.Marshal(secret)
	var html bytes.Buffer
	enc := json.NewEncoder(&html)
	enc.SetEscapeHTML(false)
	enc.Encode(secret)
	needles := [][]byte{[]byte(secret), plain[1 : len(plain)-1], bytes.TrimSuffix(html.Bytes(), []byte("\n"))[1 : html.Len()-2], []byte(hex.EncodeToString([]byte(secret)))}
	for pad := range 3 {
		encoded := base64.StdEncoding.EncodeToString(append(make([]byte, pad), secret...))
		needles = append(needles, []byte(encoded[(pad*4+2)/3:len(encoded)-4]))
	}
	return needles
}

func findPurged(t *testing.T, where string, data []byte, secret string) {
	t.Helper()
	for _, needle := range purgeNeedles(secret) {
		if i := bytes.Index(data, needle); i >= 0 {
			start := max(0, i-60)
			t.Errorf("%s holds the secret as %q at byte %d: …%q…", where, needle[:min(len(needle), 12)], i, data[start:min(len(data), i+len(needle)+20)])
		}
	}
}

// everyRow is every row of every table of db's database, as binary COPY
// gives them.
func everyRow(t *testing.T, db *pg.DB) map[string][]byte {
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
	out := map[string][]byte{}
	for _, table := range tables {
		var b bytes.Buffer
		if _, err := conn.Conn().PgConn().CopyTo(t.Context(), &b, `COPY "`+table[0]+`"."`+table[1]+`" TO STDOUT WITH (FORMAT binary)`); err != nil {
			t.Fatal(table, err)
		}
		out[table[1]] = b.Bytes()
	}
	return out
}

func unitVector(i int) []float32 {
	v := make([]float32, 4)
	v[i%4] = 1
	return v
}

// The purge completeness test for what lies outside the notebook: a secret
// in notes, quoted by a private chat transcript, a Knowledge proposal, a
// consumer checkpoint and Jev records, indexed as current and historical
// passages with semantic vectors and in the note map, is purged by
// redaction through the browser route; then no row of any
// table, no file of the data directory and no object of the Git export
// holds it in any form, the vectors derived from it are gone, and each
// redacted record still reads.
func TestPurgeLeavesNoDerivedCopyOfARedactedSecret(t *testing.T) {
	app, store := fixture(t)
	secret := webPurgeSecret() + "<&>"
	ctx := context.Background()
	first := savePurgeNote(t, store, "notes/keys.md", "# Keys "+secret+"\n\nThe staging key is "+secret+".\n", "")
	savePurgeNote(t, store, "notes/keys.md", "# Keys "+secret+"\n\nThe staging key is "+secret+", rotated soon.\n\nMore text.\n", first.Revision)
	savePurgeNote(t, store, "notes/plain.md", "# Plain\n\nNothing secret, but it cites ?fmn-rev="+first.Revision+".\n", "")

	// The derived index: current and historical passages with vectors, and
	// the note map.
	engine, release, err := app.acquireEngine(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.ConfigureAgent(ctx, "synthetic-embedding", 4, false); err != nil {
		t.Fatal(err)
	}
	vectors := 0
	for _, scope := range []string{"current", "history"} {
		pending, err := engine.PendingScope(ctx, 64, scope)
		if err != nil {
			t.Fatal(scope, err)
		}
		submission := knowledge.Submission{Scope: scope, Revision: pending.Status.Revision, ConfigID: pending.Status.ConfigID}
		if scope == "history" {
			submission.Revision = pending.Status.History.TargetRevision
		}
		for i, p := range pending.Passages {
			submission.Vectors = append(submission.Vectors, knowledge.VectorSubmission{ID: p.ID, Hash: p.Hash, NoteHash: p.NoteHash, Vector: unitVector(i)})
		}
		if _, err := engine.Submit(ctx, submission); err != nil {
			t.Fatal(scope, err)
		}
		vectors += len(submission.Vectors)
	}
	if _, err := engine.Map(ctx, knowledge.MapQuery{}); err != nil {
		t.Fatal(err)
	}
	// A proposal and a consumer checkpoint that quote it.
	if _, err := engine.Propose(ctx, "admin", workspace.EditGroup{ID: "quote", Summary: "Add the key", Edits: []workspace.NoteEdit{{Path: "notes/proposed.md", Content: "# Proposed\n\nUse " + secret + ".\n"}}}); err != nil {
		t.Fatal(err)
	}
	status, _ := store.Status(ctx)
	if _, err := engine.Complete(ctx, "admin", "reviewer", "", status.Revision, "Reviewed the key "+secret, false); err != nil {
		t.Fatal(err)
	}
	release()
	// A private chat transcript and Jev records that quote it.
	conversation, err := app.chat.Store.Create("admin", "default", chat.Options{})
	if err != nil {
		t.Fatal(err)
	}
	err = app.chat.Store.Update("admin", "default", conversation.ID, func(c *chat.Conversation) error {
		c.Title = "About " + secret
		c.AddEvent("run-1", "tool", map[string]any{"result": `{"content":"# Keys\n\nThe staging key is ` + strings.ReplaceAll(secret, `"`, `\"`) + `."}`, "citation": "/notes/keys.md?fmn-rev=" + first.Revision})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	job := strings.Repeat("a", 64)
	if err := app.jev.Records().Write("jobs/"+job+".json", map[string]any{"version": 2, "project": "default", "message": "Checked " + secret}); err != nil {
		t.Fatal(err)
	}
	if err := app.jev.Records().Write("results/"+job+"-0001.json", map[string]any{"quote": secret}); err != nil {
		t.Fatal(err)
	}
	if err := app.jev.Records().Write("jobs/"+strings.Repeat("b", 64)+".json", map[string]any{"version": 2, "project": "elsewhere", "message": "Not this notebook"}); err != nil {
		t.Fatal(err)
	}
	// A private chat of another notebook that drew on this one.
	if err := app.catalog.CreateKind(catalog.Bootstrap, "other", "Other", "notes"); err != nil {
		t.Fatal(err)
	}
	corpus := &chat.Corpus{NoteProjects: []string{"other", "default"}, Version: 1, Archives: "exclude", Drafts: "exclude"}
	drew, err := app.chat.Store.Create("admin", "other", chat.Options{Corpus: corpus})
	if err != nil {
		t.Fatal(err)
	}
	err = app.chat.Store.Update("admin", "other", drew.ID, func(c *chat.Conversation) error {
		c.AddEvent("run-1", "tool", map[string]any{"result": "default/notes/keys.md: The staging key is " + secret})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// A bundle export in progress, which holds the notebook as it was.
	export, err := app.backupChunk(ctx, store, "default", 0, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	dir := app.data
	if _, err := os.Stat(filepath.Join(dir, "projects", "default", "cache", "history.json")); err != nil {
		t.Fatal("the history index was not written:", err)
	}
	var before int
	app.db.Pool.QueryRow(ctx, `SELECT count(*) FROM knowledge_vectors WHERE org = $1 AND notebook = 'default'`, app.db.Org).Scan(&before)
	if before == 0 || vectors == 0 {
		t.Fatal("no vectors were stored", before, vectors)
	}

	plan := preparePurgeHTTP(t, app, workspace.PurgeScope{Kind: workspace.PurgeText, Text: secret})
	if plan.Derived.Chats != 2 || plan.Derived.JevRecords != 2 || plan.Derived.Proposals != 2 {
		t.Fatalf("the plan counts what quoted the text: %+v", plan.Derived)
	}
	code, receipt, body := executePurgeHTTP(app, plan, "Leaked staging key")
	if code != 200 || len(receipt.Warnings) > 0 {
		t.Fatalf("execute: HTTP %d %s", code, body)
	}
	if receipt.Derived.Chats != 2 || receipt.Derived.JevRecords != 2 || receipt.Derived.Proposals != 1 || receipt.Derived.Checkpoints != 1 || receipt.Derived.Vectors != before {
		t.Fatalf("derived: %+v (vectors before %d)", receipt.Derived, before)
	}

	for table, rows := range everyRow(t, app.db) {
		findPurged(t, "table "+table, rows, secret)
	}
	var after int
	app.db.Pool.QueryRow(ctx, `SELECT count(*) FROM knowledge_vectors WHERE org = $1 AND notebook = 'default'`, app.db.Org).Scan(&after)
	if after != 0 {
		t.Errorf("%d vectors derived before the purge remain", after)
	}
	// The bundle export in progress is gone: its client starts again from
	// the purged notebook.
	if _, err := app.backupChunk(ctx, store, "default", 1, 1, export.SHA256); err == nil {
		t.Error("an export begun before the purge still serves its bundle")
	}
	var bundles int
	app.db.Pool.QueryRow(ctx, `SELECT count(*) FROM exports WHERE org = $1`, app.db.Org).Scan(&bundles)
	if keys, _ := app.blobs.List(ctx, "exports/"); bundles != 0 || slices.ContainsFunc(keys, func(key string) bool { return !strings.Contains(strings.TrimPrefix(key, "exports/"), "/") }) {
		t.Errorf("a bundle export begun before the purge remains: %d rows, blobs %v", bundles, keys)
	}
	// The Git export repository is checked object by object below.
	exports := filepath.Join(dir, "exports")
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasPrefix(p, filepath.Join(exports, "default.git")+string(filepath.Separator)) {
			return err
		}
		raw, err := os.ReadFile(p)
		findPurged(t, "file "+strings.TrimPrefix(p, dir), raw, secret)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if repo := filepath.Join(exports, "default.git"); receipt.Export {
		cmd := exec.Command("git", "--git-dir="+repo, "cat-file", "--batch-all-objects", "--unordered", "--batch")
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
		objects, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		findPurged(t, "the Git export", objects, secret)
	}

	// Each redacted record still reads, and old revisions it cited resolve.
	c, err := app.chat.Store.Read("admin", "default", conversation.ID)
	if err != nil || !strings.Contains(c.Title, workspace.PurgedText) {
		t.Fatalf("the transcript after the purge: %+v %v", c.Title, err)
	}
	elsewhere, err := app.chat.Store.Read("admin", "other", drew.ID)
	if err != nil || !strings.Contains(string(elsewhere.Events[0].Data), workspace.PurgedText) {
		t.Fatalf("the other notebook's transcript after the purge: %+v %v", elsewhere.Events, err)
	}
	cited := string(c.Events[0].Data)
	if strings.Contains(cited, first.Revision) || !strings.Contains(cited, "fmn-rev=") {
		t.Fatalf("the transcript's citation was not remapped: %s", cited)
	}
	engine, release, err = app.acquireEngine(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	proposals, err := engine.Proposals(ctx)
	if err != nil || len(proposals) != 1 || !strings.Contains(proposals[0].Group.Edits[0].Content, workspace.PurgedText) {
		t.Fatalf("the proposal after the purge: %+v %v", proposals, err)
	}
	checkpoints := engine.Checkpoints()
	if len(checkpoints) != 1 || checkpoints[0].Revision == status.Revision || !strings.Contains(checkpoints[0].Summary, workspace.PurgedText) {
		t.Fatalf("the checkpoint after the purge: %+v", checkpoints)
	}
	var other map[string]any
	if err := app.jev.Records().Read("jobs/"+strings.Repeat("b", 64)+".json", &other); err != nil || other["message"] != "Not this notebook" {
		t.Fatalf("another notebook's Jev record changed: %v %v", other, err)
	}
	// The index is derived again from the purged notebook.
	pending, err := engine.Pending(ctx, 64)
	if err != nil || len(pending.Passages) == 0 {
		t.Fatalf("the index after the purge: %+v %v", pending, err)
	}
	for _, p := range pending.Passages {
		if strings.Contains(p.Text, secret) {
			t.Fatal("a passage holds the secret")
		}
	}
}
