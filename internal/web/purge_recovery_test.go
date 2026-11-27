package web

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/gitexport"
	"forgetmenote/internal/notestore"
	"forgetmenote/internal/pg"
	"forgetmenote/internal/pg/pgtest"
	"forgetmenote/internal/purgehttp"
	"forgetmenote/internal/workspace"
)

// instanceFixture serves an instance whose notes store and gateway share one
// database, as every deployment's do (fixture keeps them apart).
func instanceFixture(t *testing.T) (*Server, *workspace.Store) {
	t.Helper()
	dir := t.TempDir()
	db := pgtest.Dir(t, dir)
	projects, err := catalog.Open(dir, nil, db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { projects.Close() })
	store, _ := projects.Access(catalog.Bootstrap, "default", "edit")
	app, err := New(projects, db, dir, "http://localhost", testToken)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	return app, store
}

// A purge rewrites the notebook's history in one transaction and finishes
// after it: the export, the indexes and the records outside the notebook,
// then its receipt. When its gateway stops in between, the purge is listed
// as unfinished, with what it did not do and how to finish it; a gateway that
// starts finishes what needs no erased content and keeps a receipt that says
// it was interrupted, unless the purge is still running on another gateway;
// and the next export of the notebook rebuilds the export for the purge, with
// the flag that lets the backup worker replace the remote's copy once.
func TestPurgeThatStoppedAfterItsRewriteIsListedAndFinished(t *testing.T) {
	app, store := instanceFixture(t)
	ctx := context.Background()
	secret := webPurgeSecret()
	first := savePurgeNote(t, store, "notes/keys.md", "# Keys\n\nThe key is "+secret+".\n", "")
	savePurgeNote(t, store, "notes/keys.md", "# Keys\n\nThe key is "+secret+", rotated soon.\n", first.Revision)
	export := store.PurgeExporter()
	before, err := export.Sync(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	if err := app.db.BeginFlow(ctx, purgehttp.FlowKind("default"), "plan", "admin", []byte(`{"scope":{"text":"`+secret+`"}}`), time.Hour, pg.FlowBound{Limit: 64}); err != nil {
		t.Fatal(err)
	}
	// The purge rewrites the history, and its gateway stops before anything
	// else: neither the export nor the clean-up nor the receipt happen.
	plan, err := store.PreparePurge(ctx, workspace.PurgeScope{Kind: workspace.PurgeText, Text: secret}, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := store.ExecutePurge(ctx, plan, plan.Confirmation, "@local-admin", "Leaked key", nil)
	if err != nil {
		t.Fatal(err)
	}
	listed := func() purgehttp.Receipt {
		t.Helper()
		w := call(app, "GET", "/api/purge", "", testToken)
		var out struct {
			Receipts []purgehttp.Receipt `json:"receipts"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil || len(out.Receipts) != 1 {
			t.Fatalf("receipts: HTTP %d %s", w.Code, w.Body)
		}
		return out.Receipts[0]
	}
	says := func(r purgehttp.Receipt, text string) bool {
		return slices.ContainsFunc(r.Warnings, func(w string) bool { return strings.Contains(w, text) })
	}
	unfinished := listed()
	if unfinished.Purge != result.Purge || !unfinished.Unfinished || unfinished.Interrupted || unfinished.Reason != "Leaked key" || unfinished.Kind != workspace.PurgeText ||
		!says(unfinished, "admin purge rebuild -notebook default -purge "+result.Purge) || !says(unfinished, "may not have been redacted") {
		t.Fatalf("an unfinished purge: %+v", unfinished)
	}

	// While a gateway holds the purge's lock, the purge is running there.
	running, err := app.db.Exclusive(ctx, purgehttp.RunKey("default"))
	if err != nil {
		t.Fatal(err)
	}
	app.purge.FinishStopped(ctx)
	running()
	if r := listed(); !r.Unfinished {
		t.Fatalf("a starting gateway finished a purge that runs elsewhere: %+v", r)
	}

	heard := make(chan string, 4)
	listening, stop := context.WithCancel(ctx)
	defer stop()
	go app.db.Listen(listening, "notebook_purges", func(notebook string) { heard <- notebook })
	if <-heard != "" {
		t.Fatal("not listening")
	}
	if err := app.Start(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case notebook := <-heard:
		if notebook != "default" {
			t.Fatalf("announced %q", notebook)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the other gateways were not told of the purge")
	}
	interrupted := listed()
	if interrupted.Unfinished || !interrupted.Interrupted || interrupted.Purge != result.Purge || !says(interrupted, "may not have been redacted") || !says(interrupted, "admin purge rebuild") {
		t.Fatalf("an interrupted purge's receipt: %+v", interrupted)
	}
	var plans int
	if err := app.db.Pool.QueryRow(ctx, `SELECT count(*) FROM pending_flows WHERE org = $1`, app.db.Org).Scan(&plans); err != nil || plans != 0 {
		t.Fatalf("%d prepared plans remain (%v)", plans, err)
	}

	// The next export of the notebook rebuilds it for the purge.
	head, err := export.Sync(ctx, "default")
	if err != nil || head.Commit.String() != result.Revision {
		t.Fatalf("export: %v %v", head, err)
	}
	flag, ok, err := gitexport.ReadPurge(export.Path("default"))
	if err != nil || !ok || flag.Purge != result.Purge || !slices.Contains(flag.Replaces, before.Commit) {
		t.Fatalf("purge flag: %+v %v %v", flag, ok, err)
	}

	// A purge that was still running when another gateway took it for
	// stopped keeps its own receipt.
	finished := interrupted
	finished.Interrupted, finished.Warnings = false, nil
	if err := app.purge.KeepReceipt(ctx, finished); err != nil {
		t.Fatal(err)
	}
	if r := listed(); r.Interrupted || r.Unfinished || len(r.Warnings) > 0 {
		t.Fatalf("the purge's own receipt: %+v", r)
	}
}

// A purge that finished but whose receipt the database refused is not
// taken for one that stopped: its gateway keeps the receipt in its data
// directory and in the database once it can, on its periodic sweep or when
// it starts, in place of an interrupted receipt another gateway kept
// meanwhile, and no gateway says it left records unredacted.
func TestPurgeWhoseReceiptTheDatabaseRefusedKeepsItLater(t *testing.T) {
	app, store := instanceFixture(t)
	ctx := context.Background()
	listed := func(purge string) purgehttp.Receipt {
		t.Helper()
		w := call(app, "GET", "/api/purge", "", testToken)
		var out struct {
			Receipts []purgehttp.Receipt `json:"receipts"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatalf("receipts: HTTP %d %s", w.Code, w.Body)
		}
		for _, r := range out.Receipts {
			if r.Purge == purge {
				return r
			}
		}
		t.Fatalf("purge %s is not listed: %s", purge, w.Body)
		return purgehttp.Receipt{}
	}
	finished := func(r purgehttp.Receipt) bool {
		return !r.Unfinished && !r.Interrupted && len(r.Warnings) == 0 && r.Derived.JevRecords == 1
	}
	for i, other := range []bool{false, true} {
		secret := webPurgeSecret()
		savePurgeNote(t, store, fmt.Sprintf("notes/keys-%d.md", i), "# Keys\n\nThe key is "+secret+".\n", "")
		if err := app.jev.Records().Write(fmt.Sprintf("jobs/%064d.json", i), map[string]any{"version": 2, "project": "default", "message": "Checked " + secret}); err != nil {
			t.Fatal(err)
		}
		restore := pgtest.FailWrites(t, app.db, purgehttp.ReceiptKind)
		plan := preparePurgeHTTP(t, app, workspace.PurgeScope{Kind: workspace.PurgeText, Text: secret})
		code, receipt, body := executePurgeHTTP(app, plan, "Leaked key")
		if code != 200 || len(receipt.Warnings) != 1 || !strings.Contains(receipt.Warnings[0], "once it can") {
			t.Fatalf("execute: HTTP %d %s", code, body)
		}
		if r := listed(receipt.Purge); !r.Unfinished {
			t.Fatalf("a purge whose receipt is not kept yet: %+v", r)
		}
		restore()
		if other {
			// Another gateway started meanwhile and took the purge for
			// stopped; this one keeps the receipt on its periodic sweep.
			for _, p := range mustStoppedPurges(t, app) {
				interrupted := purgehttp.RecordedReceipt(p)
				interrupted.Interrupted, interrupted.Warnings = true, purgehttp.StoppedWarnings(p)
				if err := app.purge.KeepReceipt(ctx, interrupted); err != nil {
					t.Fatal(err)
				}
			}
			app.sweepFlows()
		} else {
			app.purge.FinishStopped(ctx)
		}
		if r := listed(receipt.Purge); !finished(r) {
			t.Fatalf("the purge's receipt, kept later: %+v", r)
		}
		if entries, err := os.ReadDir(app.purge.ReceiptSpool()); err != nil || len(entries) != 0 {
			t.Fatalf("receipts left to keep: %v %v", entries, err)
		}
		app.purge.FinishStopped(ctx)
		if r := listed(receipt.Purge); !finished(r) {
			t.Fatalf("the purge's receipt after a gateway started: %+v", r)
		}
	}
}

func mustStoppedPurges(t *testing.T, app *Server) []notestore.RecordedPurge {
	t.Helper()
	stopped, err := app.purge.StoppedPurges(context.Background(), "default")
	if err != nil || len(stopped) != 1 {
		t.Fatalf("stopped purges: %+v %v", stopped, err)
	}
	return stopped
}

// A spooled receipt leaves the spool only once the database holds a receipt
// of its purge that is not an interrupted one. When keeping it in place of
// the interrupted receipt another gateway kept loses to a concurrent write,
// it waits for the next sweep, which keeps it.
func TestSpooledPurgeReceiptWaitsWhileTheStoredOneIsInterrupted(t *testing.T) {
	app, _ := fixture(t)
	ctx := context.Background()
	receipt := purgehttp.Receipt{Purge: "purge-spooled", Notebook: "default", Actor: "owner", Reason: "Leaked key", Created: time.Now().UTC().Format(time.RFC3339), Kind: "text", Exposure: purgehttp.Exposure}
	interrupted := receipt
	interrupted.Interrupted, interrupted.Warnings = true, []string{"This purge stopped after it rewrote the notebook, before it finished."}
	if err := app.purge.KeepReceipt(ctx, interrupted); err != nil {
		t.Fatal(err)
	}
	if err := app.purge.SpoolReceipt(receipt); err != nil {
		t.Fatal(err)
	}
	spooled := app.purge.SpooledReceiptPath(receipt.Notebook, receipt.Purge)
	stored := func() purgehttp.Receipt {
		t.Helper()
		_, raw, err := app.db.ReadDocument(ctx, app.db.Pool, purgehttp.ReceiptKind, receipt.Notebook+"/"+receipt.Purge, -1)
		var kept purgehttp.Receipt
		if err != nil || json.Unmarshal(raw, &kept) != nil {
			t.Fatal(string(raw), err)
		}
		return kept
	}

	// Every update of a purge receipt is lost, as to a concurrent write.
	name := "test_lose_receipts_" + app.db.Org
	if _, err := app.db.Pool.Exec(ctx, "CREATE FUNCTION "+name+"() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NULL; END $$"); err != nil {
		t.Fatal(err)
	}
	drop := func() {
		if _, err := app.db.Pool.Exec(ctx, "DROP TRIGGER IF EXISTS "+name+" ON documents; DROP FUNCTION IF EXISTS "+name+"()"); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(drop)
	if _, err := app.db.Pool.Exec(ctx, "CREATE TRIGGER "+name+" BEFORE UPDATE ON documents FOR EACH ROW WHEN (NEW.org = '"+app.db.Org+"' AND NEW.kind = '"+purgehttp.ReceiptKind+"') EXECUTE FUNCTION "+name+"()"); err != nil {
		t.Fatal(err)
	}
	if waiting := app.purge.KeepSpooledReceipts(ctx); !waiting["default/purge-spooled"] {
		t.Errorf("a receipt the database did not take is not waiting: %v", waiting)
	}
	if _, err := os.Stat(spooled); err != nil {
		t.Fatal("the spool lost a receipt the database did not take:", err)
	}
	if !stored().Interrupted {
		t.Fatal("the stored receipt changed while its updates were lost")
	}

	drop()
	if waiting := app.purge.KeepSpooledReceipts(ctx); len(waiting) != 0 {
		t.Errorf("receipts still waiting: %v", waiting)
	}
	if _, err := os.Stat(spooled); !os.IsNotExist(err) {
		t.Fatal("the spool keeps a receipt the database took:", err)
	}
	if kept := stored(); kept.Interrupted || len(kept.Warnings) != 0 {
		t.Fatalf("the stored receipt: %+v", kept)
	}
}
