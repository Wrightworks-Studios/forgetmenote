package web

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"forgetmenote/internal/purgehttp"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/pg/pgtest"
	"forgetmenote/internal/webcore"
	"forgetmenote/internal/workspace"
)

// An export that waits too long for a purge to end is told to start again
// shortly with a typed answer an agent can retry on.
func TestExportFencedByAPurgeIsRetryable(t *testing.T) {
	app, store := fixture(t)
	defer func(wait time.Duration) { exportLockWait = wait }(exportLockWait)
	exportLockWait = 200 * time.Millisecond
	savePurgeNote(t, store, "notes/a.md", "# A\n", "")
	session := actionClient(t, app, catalog.Bootstrap, "notes:read")
	ctx := context.Background()
	fence, err := app.db.Exclusive(ctx, "exports")
	if err != nil {
		t.Fatal(err)
	}
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "export_project_backup", Arguments: map[string]any{"project": "default"}})
	fence()
	if err != nil {
		t.Fatal(err)
	}
	typed, _ := json.Marshal(result.Meta[mcpErrorMeta])
	if !result.IsError || string(typed) != `{"code":"timeout","retryable":true,"status":503}` {
		t.Fatalf("an export a purge fenced: %s", typed)
	}
}

// An export that expired or that a purge ended is a typed answer: a later
// chunk is gone, and the client must start again at offset zero, which that
// same call cannot do; an export a purge ended while its first chunk was
// made is retryable as it is.
func TestEndedExportsAreTypedForAgents(t *testing.T) {
	app, store := fixture(t)
	savePurgeNote(t, store, "notes/a.md", "# A\n", "")
	session := actionClient(t, app, catalog.Bootstrap, "notes:read")
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "export_project_backup", Arguments: map[string]any{"project": "default", "offset": 64, "sha256": strings.Repeat("0", 64)}})
	if err != nil {
		t.Fatal(err)
	}
	typed, _ := json.Marshal(result.Meta[mcpErrorMeta])
	if !result.IsError || string(typed) != `{"code":"export_expired","status":410}` {
		t.Fatalf("a chunk of an export that ended: %s", typed)
	}
	if detail := webcore.ErrorOf(errExportPurged, 500); detail.Status != http.StatusServiceUnavailable || !detail.Retryable() {
		t.Fatalf("an export a purge ended as it began: %+v", detail)
	}
}

// Every refusal the web layer can make comes before a purge ends the bundle
// exports in progress: a purge refused for its reason, its confirmation, a
// changed notebook or a plan that cannot run leaves a running export to
// finish.
func TestRefusedPurgeLeavesExportsInProgress(t *testing.T) {
	app, store := fixture(t)
	ctx := context.Background()
	secret := webPurgeSecret()
	savePurgeNote(t, store, "notes/keys.md", "# Keys\n\nThe key is "+secret+".\n", "")
	first, err := app.backupChunk(ctx, store, "default", 0, 64, "")
	if err != nil || first.NextOffset == 0 {
		t.Fatalf("export: %+v %v", first, err)
	}
	plan := preparePurgeHTTP(t, app, workspace.PurgeScope{Kind: workspace.PurgeText, Text: secret})
	blocked := preparePurgeHTTP(t, app, workspace.PurgeScope{Kind: workspace.PurgeText, Text: "keys"})
	if blocked.Blocked == "" {
		t.Fatalf("a plan that cannot run: %+v", blocked)
	}
	for _, refused := range []struct {
		plan   webPurgePlan
		reason string
		status int
	}{
		{plan, "", http.StatusBadRequest},
		{plan, "The key " + secret + " leaked", http.StatusBadRequest},
		{webPurgePlan{Scope: plan.Scope, Revision: plan.Revision, Confirmation: blocked.Confirmation}, "Leaked key", http.StatusConflict},
		{webPurgePlan{Scope: plan.Scope, Revision: blocked.Revision + "0", Confirmation: plan.Confirmation}, "Leaked key", http.StatusConflict},
		{blocked, "A file name", http.StatusBadRequest},
	} {
		if code, _, body := executePurgeHTTP(app, refused.plan, refused.reason); code != refused.status {
			t.Fatalf("refused purge: HTTP %d %s", code, body)
		}
	}
	next, err := app.backupChunk(ctx, store, "default", first.NextOffset, 64, first.SHA256)
	if err != nil || next.Data == "" {
		t.Fatalf("a refused purge ended the export: %v", err)
	}
}

// A purge that the workspace refuses after the web layer's checks passed,
// for a write that landed while it waited for the notebook's purge lock,
// ends nothing either: exports end only once every check has passed.
func TestPurgeRefusedByTheWorkspaceLeavesExportsInProgress(t *testing.T) {
	app, store := fixture(t)
	ctx := context.Background()
	secret := webPurgeSecret()
	saved := savePurgeNote(t, store, "notes/keys.md", "# Keys\n\nThe key is "+secret+".\n", "")
	first, err := app.backupChunk(ctx, store, "default", 0, 64, "")
	if err != nil || first.NextOffset == 0 {
		t.Fatalf("export: %+v %v", first, err)
	}
	plan := preparePurgeHTTP(t, app, workspace.PurgeScope{Kind: workspace.PurgeText, Text: secret})
	running, err := app.db.Exclusive(ctx, purgehttp.RunKey("default"))
	if err != nil {
		t.Fatal(err)
	}
	type answer struct {
		code int
		body string
	}
	done := make(chan answer, 1)
	go func() {
		code, _, body := executePurgeHTTP(app, plan, "Leaked key")
		done <- answer{code, body}
	}()
	waiting := pgtest.WaitingFor(t, app.db, pgtest.ExclusiveHolder(t, app.db, purgehttp.RunKey("default")))
	for deadline := time.Now().Add(10 * time.Second); waiting(0) == 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			running()
			t.Fatal("the purge never waited for its lock")
		}
	}
	savePurgeNote(t, store, "notes/keys.md", "# Keys\n\nThe key is "+secret+", rotated.\n", saved.Revision)
	running()
	if got := <-done; got.code != http.StatusConflict {
		t.Fatalf("a purge of a notebook that changed: HTTP %d %s", got.code, got.body)
	}
	next, err := app.backupChunk(ctx, store, "default", first.NextOffset, 64, first.SHA256)
	if err != nil || next.Data == "" {
		t.Fatalf("a refused purge ended the export: %v", err)
	}
}

// The previous release kept each notebook as a Git working tree, with live
// edits and captures beside it, which the cutover leaves in the data
// directory; no purge reaches them. While they are there, a plan and a
// receipt say so; without them, neither does.
func TestPurgeWarnsOfThePreviousReleasesFiles(t *testing.T) {
	for _, leftover := range []string{"", "workspace/.git", "collaboration", "captures", "write-journal.json"} {
		t.Run(leftover, func(t *testing.T) {
			app, store := fixture(t)
			if leftover != "" {
				path := filepath.Join(filepath.Dir(store.Root), filepath.FromSlash(leftover))
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				create := func() error { return os.Mkdir(path, 0o700) }
				if strings.HasSuffix(leftover, ".json") {
					create = func() error { return os.WriteFile(path, []byte("{}"), 0o600) }
				}
				err := create()
				if err != nil {
					t.Fatal(err)
				}
			}
			secret := webPurgeSecret()
			savePurgeNote(t, store, "notes/keys.md", "# Keys\n\n"+secret+"\n", "")
			warned := func(warnings []string) bool {
				return slices.ContainsFunc(warnings, func(w string) bool { return strings.Contains(w, "previous release") })
			}
			body, _ := json.Marshal(map[string]any{"scope": workspace.PurgeScope{Kind: workspace.PurgeText, Text: secret}})
			w := call(app, "POST", "/api/purge/prepare", string(body), testToken)
			var plan struct {
				webPurgePlan
				Warnings []string `json:"warnings"`
			}
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &plan) != nil || warned(plan.Warnings) != (leftover != "") {
				t.Fatalf("plan with %q: HTTP %d %s", leftover, w.Code, w.Body)
			}
			code, receipt, out := executePurgeHTTP(app, plan.webPurgePlan, "Leaked key")
			if code != 200 || warned(receipt.Warnings) != (leftover != "") || strings.Contains(out, store.Root) {
				t.Fatalf("receipt with %q: HTTP %d %s", leftover, code, out)
			}
		})
	}
}
