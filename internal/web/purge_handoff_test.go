package web

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
)

// Every writer of this release, the purge's rewrite included, is fenced on
// the notebook's authority record once a handoff is prepared. A revision
// that a writer without that fence still commits, such as an earlier
// release during a rolling deploy, aborts the handoff before publication,
// which cannot be undone, instead of leaving it stuck in publishing with a
// notebook neither side can write.
func TestHandoffAbortsWhenTheNotebookMovedAfterItWasPrepared(t *testing.T) {
	app, store, _ := authorityFixture(t)
	ctx := t.Context()
	invoke := func(name string, in any) (int, string) {
		t.Helper()
		raw, _ := json.Marshal(in)
		w := apiCall(app, "POST", "/api/v1/actions/"+name, string(raw), testToken)
		return w.Code, w.Body.String()
	}
	code, body := invoke("preview_notebook_authority_change", authorityPreviewInput{Project: "default", To: "github", Repository: "example/notebook", OperatorWritersStopped: true})
	var p catalog.AuthorityTransfer
	if code != 200 || json.Unmarshal([]byte(body), &p) != nil {
		t.Fatalf("preview: %d %s", code, body)
	}
	if code, body = invoke("apply_notebook_authority_change", authorityApplyInput{Project: "default", Preview: p.ID}); code != 200 || !strings.Contains(body, `"phase":"preparing"`) {
		t.Fatalf("apply before the worker drained: %d %s", code, body)
	}

	// A writer that does not fence on the authority record commits.
	notes := instanceNotes(t, app).Notebook("default")
	head, err := notes.Head(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := notes.Apply(ctx, notestore.ChangeSet{ExpectHead: &head.Commit, Files: []notestore.FileChange{{Path: "unfenced.md", Content: []byte("# Unfenced\n")}},
		Author: gitobj.NewSignature("Earlier release", "earlier@example.test", time.Now()), Subject: "Write without the fence"}); err != nil {
		t.Fatal(err)
	}

	b, err := app.backups.manager.Status("default")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"project": "default", "handoff": p.ID, "generation": b.Generation})
	r := httptest.NewRequest("POST", app.origin+"/internal/backups/v1/drain", strings.NewReader(string(raw)))
	r.Header.Set("Authorization", "Bearer "+testToken)
	r.Header.Set("X-Backup-Worker", "fixture")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	app.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal("worker acknowledgement", w.Code, w.Body.String())
	}
	if code, body = invoke("apply_notebook_authority_change", authorityApplyInput{Project: "default", Preview: p.ID}); code != 409 {
		t.Fatalf("apply after the notebook moved: %d %s", code, body)
	}
	if a := app.catalog.Authority("default"); a.Type != "forgetmenote" || a.Generation != 1 {
		t.Fatalf("authority after the aborted handoff: %+v", a)
	}
	if p, err = app.catalog.AuthorityTransfer(catalog.Bootstrap, "default"); err != nil || p.Phase != "failed" {
		t.Fatalf("the handoff: %+v %v", p, err)
	}
	// The notebook takes writes again, the unfenced revision kept.
	if _, err := store.Read(ctx, "unfenced.md", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save(ctx, "after.md", []byte("# After\n"), "", "fixture"); err != nil {
		t.Fatal("a write after the aborted handoff:", err)
	}
}
