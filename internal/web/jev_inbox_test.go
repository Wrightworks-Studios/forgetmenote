package web

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"forgetmenote/internal/catalog"
)

// A protected Inbox reaches a model only for its owner, with that owner's
// personal cloud consent. Admin and Owner authority over the notebook does
// not extend to automatic checks, their settings or chat corpora.
func TestProtectedInboxAIStaysWithItsConsentingOwner(t *testing.T) {
	var calls atomic.Int32
	app := reviewFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		syntheticReviewAnswer(w, r)
	})
	if err := app.catalog.SetUser(catalog.Bootstrap, catalog.User{ID: "admin", Admin: true}); err != nil {
		t.Fatal(err)
	}
	if w := proxyCall(app, "admin", "POST", "/api/jev/settings", `{"consent":true,"rerank":true,"citations":true}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	inbox, err := app.catalog.EnsureInbox("bob")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.catalog.Access("admin", inbox.ID, "owner"); err != nil {
		t.Fatal("fixture: Admin authority over the Inbox", err)
	}
	store, _ := app.catalog.Access("bob", inbox.ID, "edit")
	if _, err := store.Save(t.Context(), "capture.md", []byte("# Capture\nBOB_PRIVATE one"), "", "bob"); err != nil {
		t.Fatal(err)
	}

	if w := proxyCall(app, "admin", "POST", "/p/"+inbox.ID+"/api/jev/settings", `{"autoChecks":true}`); w.Code != 403 {
		t.Errorf("Admin enabled automatic checks on another person's Inbox: %d %s", w.Code, w.Body)
	}
	if w := proxyCall(app, "admin", "POST", "/p/"+inbox.ID+"/api/jev/jobs", `{"kind":"consistency","operationId":"admin-review"}`); w.Code == 200 {
		t.Errorf("Admin started a Jev review of another person's Inbox: %d %s", w.Code, w.Body)
	}

	// A setting retained from before this rule never scans the Inbox.
	admin, err := app.catalog.Account("admin")
	if err != nil {
		t.Fatal(err)
	}
	err = writeJevSettings(app, inbox.ID, "admin", admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	app.jev.ScanAutomatic(context.Background(), now)
	app.jev.ScanAutomatic(context.Background(), now.Add(3*time.Minute))
	time.Sleep(500 * time.Millisecond)
	ids, _ := app.jev.JobIDs()
	if calls.Load() != 0 || len(ids) != 0 {
		t.Errorf("automatic checks processed another person's Inbox: calls=%d jobs=%d", calls.Load(), len(ids))
	}

	corpus := `{"version":1,"archives":"exclude","drafts":"exclude","history":false,"noteProjects":["` + inbox.ID + `"]}`
	if w := proxyCall(app, "admin", "POST", "/p/default/api/chat", `{"connection":"local","model":"synthetic","mode":"ask","corpus":`+corpus+`}`); w.Code == 200 {
		t.Errorf("Admin chat corpus included another person's Inbox: %d %s", w.Code, w.Body)
	}
	if w := proxyCall(app, "admin", "POST", "/p/"+inbox.ID+"/api/chat", `{"connection":"local","model":"synthetic","mode":"ask"}`); w.Code == 200 {
		t.Errorf("Admin chat opened in another person's Inbox: %d %s", w.Code, w.Body)
	}

	// The owner needs personal cloud consent too.
	if w := proxyCall(app, "bob", "POST", "/p/"+inbox.ID+"/api/jev/settings", `{"autoChecks":true}`); w.Code != 403 {
		t.Errorf("owner enabled automatic checks without cloud consent: %d %s", w.Code, w.Body)
	}
	if w := proxyCall(app, "bob", "POST", "/p/"+inbox.ID+"/api/chat", `{"connection":"local","model":"synthetic","mode":"ask"}`); w.Code == 200 {
		t.Errorf("owner chat in the Inbox without cloud consent: %d %s", w.Code, w.Body)
	}
	if w := proxyCall(app, "bob", "POST", "/p/"+inbox.ID+"/api/jev/jobs", `{"kind":"consistency","operationId":"owner-review"}`); w.Code == 200 {
		t.Errorf("owner started a Jev review of the Inbox without cloud consent: %d %s", w.Code, w.Body)
	}
	ids, _ = app.jev.JobIDs()
	if calls.Load() != 0 || len(ids) != 0 {
		t.Errorf("explicit Jev work processed the Inbox without its owner's consent: calls=%d jobs=%d", calls.Load(), len(ids))
	}
	settings, err := app.quicknotes.Settings("bob")
	if err != nil {
		t.Fatal(err)
	}
	settings.CloudConsent = true
	if _, err = app.quicknotes.UpdateSettings("bob", settings); err != nil {
		t.Fatal(err)
	}
	if w := proxyCall(app, "bob", "POST", "/p/"+inbox.ID+"/api/jev/settings", `{"autoChecks":true}`); w.Code != 200 {
		t.Errorf("consenting owner cannot enable automatic checks: %d %s", w.Code, w.Body)
	}
	if w := proxyCall(app, "bob", "POST", "/p/"+inbox.ID+"/api/chat", `{"connection":"local","model":"synthetic","mode":"ask"}`); w.Code != 200 {
		t.Errorf("consenting owner cannot chat in the Inbox: %d %s", w.Code, w.Body)
	}
}
