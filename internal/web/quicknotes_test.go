package web

import (
	"encoding/json"
	"fmt"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/quicknotes"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestQuicknotesInboxCountTracksNotesBeyondRecentHistory(t *testing.T) {
	app := chatFixture(t)
	if err := app.catalog.Member(catalog.Bootstrap, "default", "alice", "owner"); err != nil {
		t.Fatal(err)
	}
	checkCount := func(user string, want int) int {
		t.Helper()
		summary := proxyCall(app, user, "GET", "/api/quicknotes?summary=1", "")
		var fields map[string]json.RawMessage
		if summary.Code != 200 || json.Unmarshal(summary.Body.Bytes(), &fields) != nil || string(fields["inboxCount"]) != fmt.Sprint(want) || fields["captures"] != nil {
			t.Fatalf("%s Inbox summary want only count %d: %d %s", user, want, summary.Code, summary.Body)
		}
		w := proxyCall(app, user, "GET", "/api/quicknotes", "")
		var listing struct {
			InboxCount *int                 `json:"inboxCount"`
			Captures   []quicknotes.Capture `json:"captures"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &listing) != nil || listing.InboxCount == nil || *listing.InboxCount != want {
			t.Fatalf("%s Inbox count want %d: %d %s", user, want, w.Code, w.Body)
		}
		return len(listing.Captures)
	}
	checkCount("alice", 0)
	if _, err := app.catalog.Inbox("alice"); err == nil {
		t.Fatal("reading a count provisioned an Inbox")
	}
	// Recent history holds 100 captures; a smaller cap reaches its edge in four.
	app.quicknotes.Recent = 3
	var captured quicknotes.Capture
	for i := 0; i < 4; i++ {
		body, _ := json.Marshal(map[string]string{"operationId": fmt.Sprintf("count-%d", i), "content": fmt.Sprintf("# Count capture %d\nSynthetic private note.", i)})
		w := proxyCall(app, "alice", "POST", "/api/quicknotes", string(body))
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &captured) != nil {
			t.Fatalf("capture %d: %d %s", i, w.Code, w.Body)
		}
	}
	if got := checkCount("alice", 4); got != 3 {
		t.Fatalf("expected capped recent history, got %d captures", got)
	}
	checkCount("bob", 0)
	body, _ := json.Marshal(map[string]any{"id": captured.ID, "version": captured.Version, "destination": map[string]string{"project": "default", "folder": ""}})
	if w := proxyCall(app, "alice", "POST", "/api/quicknotes/file", string(body)); w.Code != 200 {
		t.Fatalf("file capture: %d %s", w.Code, w.Body)
	}
	checkCount("alice", 3)
	body, _ = json.Marshal(map[string]string{"id": captured.ID})
	if w := proxyCall(app, "alice", "POST", "/api/quicknotes/undo", string(body)); w.Code != 200 {
		t.Fatalf("undo filing: %d %s", w.Code, w.Body)
	}
	checkCount("alice", 4)
	w := proxyCall(app, "alice", "GET", "/p/"+captured.InboxProject+"/api/note?path="+url.QueryEscape(captured.InboxPath), "")
	var note noteOutput
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &note) != nil {
		t.Fatalf("read returned capture: %d %s", w.Code, w.Body)
	}
	body, _ = json.Marshal(map[string]any{"paths": []string{note.Path}, "revision": note.Revision})
	if w = proxyCall(app, "alice", "POST", "/p/"+captured.InboxProject+"/api/trash", string(body)); w.Code != 200 {
		t.Fatalf("trash capture: %d %s", w.Code, w.Body)
	}
	checkCount("alice", 3)
}

func TestQuicknotesCaptureIsPrivateAndIdempotentWithoutCreateGrant(t *testing.T) {
	app := chatFixture(t)
	body := `{"operationId":"capture-http-first","content":"# Private capture\nThe fictional Larch shuttle departs hourly.\n"}`
	var captured struct {
		ID      string `json:"id"`
		Project string `json:"project"`
		Path    string `json:"path"`
		Version string `json:"version"`
		Status  string `json:"status"`
	}
	for attempt := 0; attempt < 2; attempt++ {
		w := proxyCall(app, "alice", "POST", "/api/quicknotes", body)
		if w.Code != 200 {
			t.Fatalf("private capture failed: %d %s", w.Code, w.Body)
		}
		var current = captured
		if err := json.Unmarshal(w.Body.Bytes(), &current); err != nil || current.ID == "" || current.Project == "" || current.Path == "" || current.Version == "" {
			t.Fatalf("capture acknowledgement missing: %s %v", w.Body, err)
		}
		if attempt > 0 && current.ID != captured.ID {
			t.Fatal("retry created a second capture")
		}
		captured = current
	}
	w := proxyCall(app, "alice", "GET", "/api/quicknotes", "")
	var listing struct {
		Captures []json.RawMessage `json:"captures"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &listing) != nil || len(listing.Captures) != 1 {
		t.Fatalf("recent captures not durable: %d %s", w.Code, w.Body)
	}
	w = proxyCall(app, "bob", "GET", "/api/quicknotes", "")
	if w.Code != 200 || strings.Contains(w.Body.String(), captured.ID) || strings.Contains(w.Body.String(), "Private capture") {
		t.Fatalf("capture list leaked across users: %d %s", w.Code, w.Body)
	}
	noteURL := "/p/" + captured.Project + "/api/note?path=" + url.QueryEscape(captured.Path)
	if w = proxyCall(app, "bob", "GET", noteURL, ""); w.Code != 403 {
		t.Fatalf("another user read private capture: %d", w.Code)
	}
	w = proxyCall(app, "alice", "GET", noteURL, "")
	var note noteOutput
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &note) != nil || note.Content != "# Private capture\nThe fictional Larch shuttle departs hourly.\n" {
		t.Fatalf("capture changed Markdown: %d %s", w.Code, w.Body)
	}
	if w = proxyCall(app, "alice", "POST", "/api/projects", `{"id":"still-not-authorized","name":"No"}`); w.Code != 403 {
		t.Fatalf("Inbox granted ordinary creation: %d", w.Code)
	}
	share := fmt.Sprintf(`{"project":%q,"user":"bob","role":"read"}`, captured.Project)
	if w = proxyCall(app, "alice", "POST", "/api/members", share); w.Code != 403 {
		t.Fatalf("Inbox history can be shared: %d %s", w.Code, w.Body)
	}
	if w = proxyCall(app, "alice", "POST", "/api/quicknotes", `{"operationId":"capture-http-first","content":"Changed retry"}`); w.Code != 409 {
		t.Fatalf("changed retry did not conflict: %d %s", w.Code, w.Body)
	}
}

func TestQuicknotesHTTPRejectsPrivilegedAndMalformedCapture(t *testing.T) {
	app := chatFixture(t)
	r := httptest.NewRequest("GET", app.origin+"/api/quicknotes", nil)
	r.Header.Set("Authorization", "Bearer "+testToken)
	w := httptest.NewRecorder()
	app.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("recovery token opened personal Inbox: %d", w.Code)
	}
	for _, body := range []string{`{"operationId":"bad-capture","content":"text","project":"default"}`, `{"operationId":"bad-capture","content":"text","owner":"bob"}`} {
		if w := proxyCall(app, "alice", "POST", "/api/quicknotes", body); w.Code != 400 {
			t.Fatalf("capture accepted external routing/identity: %d %s", w.Code, w.Body)
		}
	}
}

func TestQuicknotesHTTPFilingAndUndoRecheckCurrentWork(t *testing.T) {
	app := chatFixture(t)
	if err := app.catalog.Member(catalog.Bootstrap, "default", "alice", "owner"); err != nil {
		t.Fatal(err)
	}
	captureBody := `{"operationId":"http-file-guard","content":"# Larch capture\nInitial text.\n"}`
	w := proxyCall(app, "alice", "POST", "/api/quicknotes", captureBody)
	var captured quicknotes.Capture
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &captured) != nil {
		t.Fatalf("capture: %d %s", w.Code, w.Body)
	}
	notePath := "/p/" + captured.Project + "/api/note"
	w = proxyCall(app, "alice", "GET", notePath+"?path="+url.QueryEscape(captured.Path), "")
	var before noteOutput
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &before) != nil {
		t.Fatalf("read: %d %s", w.Code, w.Body)
	}
	body, _ := json.Marshal(map[string]string{"path": before.Path, "content": before.Content + "Newer work.\n", "revision": before.Revision})
	w = proxyCall(app, "alice", "POST", notePath, string(body))
	var newer noteOutput
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &newer) != nil {
		t.Fatalf("edit: %d %s", w.Code, w.Body)
	}
	input := map[string]any{"id": captured.ID, "version": captured.Version, "destination": map[string]string{"project": "default", "folder": ""}}
	body, _ = json.Marshal(input)
	if w = proxyCall(app, "alice", "POST", "/api/quicknotes/file", string(body)); w.Code != 409 {
		t.Fatalf("filed stale source: %d %s", w.Code, w.Body)
	}
	input["version"] = newer.Version
	body, _ = json.Marshal(input)
	w = proxyCall(app, "alice", "POST", "/api/quicknotes/file", string(body))
	var filed quicknotes.Capture
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &filed) != nil || filed.Status != "filed" {
		t.Fatalf("fresh filing: %d %s", w.Code, w.Body)
	}
	w = proxyCall(app, "alice", "GET", "/p/default/api/note?path="+url.QueryEscape(filed.Path), "")
	var target noteOutput
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &target) != nil || target.Content != newer.Content {
		t.Fatalf("filing altered content: %d %s", w.Code, w.Body)
	}
	body, _ = json.Marshal(map[string]string{"path": target.Path, "content": target.Content + "Destination edit.\n", "revision": target.Revision})
	if w = proxyCall(app, "alice", "POST", "/p/default/api/note", string(body)); w.Code != 200 {
		t.Fatalf("target edit: %d %s", w.Code, w.Body)
	}
	body, _ = json.Marshal(map[string]string{"id": captured.ID})
	if w = proxyCall(app, "alice", "POST", "/api/quicknotes/undo", string(body)); w.Code != 409 {
		t.Fatalf("Undo removed newer work: %d %s", w.Code, w.Body)
	}
	if err := app.catalog.Member(catalog.Bootstrap, "default", "alice", ""); err != nil {
		t.Fatal(err)
	}
	if w = proxyCall(app, "alice", "POST", "/api/quicknotes/undo", string(body)); w.Code != 403 {
		t.Fatalf("revoked user undid shared note: %d %s", w.Code, w.Body)
	}
	w = proxyCall(app, "alice", "GET", "/api/quicknotes", "")
	if w.Code != 200 || strings.Contains(w.Body.String(), "Larch capture") || strings.Contains(w.Body.String(), filed.Path) {
		t.Fatalf("retained capture leaked revoked destination: %d %s", w.Code, w.Body)
	}
}

func TestQuicknotesHTTPApprovalIncludesCurrentSharingState(t *testing.T) {
	app := chatFixture(t)
	if err := app.catalog.Member(catalog.Bootstrap, "default", "alice", "owner"); err != nil {
		t.Fatal(err)
	}
	w := proxyCall(app, "alice", "GET", "/api/quicknotes/settings", "")
	var settings quicknotes.Settings
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &settings) != nil {
		t.Fatalf("settings: %d %s", w.Code, w.Body)
	}
	settings.Mode = "automatic"
	settings.CloudConsent = true
	settings.ApprovedDestinations = []quicknotes.Destination{{Project: "default", Folder: ""}}
	body, _ := json.Marshal(settings)
	if w = proxyCall(app, "alice", "POST", "/api/quicknotes/settings", string(body)); w.Code != 400 {
		t.Fatalf("unacknowledged shared destination: %d %s", w.Code, w.Body)
	}
	settings.ApprovedDestinations[0].Shared = true
	body, _ = json.Marshal(settings)
	if w = proxyCall(app, "alice", "POST", "/api/quicknotes/settings", string(body)); w.Code != 200 {
		t.Fatalf("explicit shared approval: %d %s", w.Code, w.Body)
	}
	if w = proxyCall(app, "alice", "POST", "/api/quicknotes/settings", string(body)); w.Code != 409 {
		t.Fatalf("stale settings replaced newer choices: %d %s", w.Code, w.Body)
	}
}
