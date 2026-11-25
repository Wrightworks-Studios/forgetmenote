package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/workspace"
)

func webPurgeSecret() string {
	raw := make([]byte, 12)
	rand.Read(raw)
	return "sk-live-" + hex.EncodeToString(raw)
}

func savePurgeNote(t *testing.T, store *workspace.Store, path, content, revision string) workspace.Document {
	t.Helper()
	doc, err := store.Save(context.Background(), path, []byte(content), revision, "test")
	if err != nil {
		t.Fatal(path, err)
	}
	return doc
}

type webPurgePlan struct {
	Scope              workspace.PurgeScope        `json:"scope"`
	Revision           string                      `json:"revision"`
	From               string                      `json:"from"`
	RevisionsRewritten int                         `json:"revisionsRewritten"`
	Notes              []workspace.PurgeFile       `json:"notes"`
	Versions           int                         `json:"versions"`
	Elsewhere          []workspace.PurgeOccurrence `json:"elsewhere"`
	Live               []workspace.PurgeLive       `json:"live"`
	ReferencesToRemap  int                         `json:"referencesToRemap"`
	ExportForcePush    bool                        `json:"exportForcePush"`
	Blocked            string                      `json:"blocked"`
	Confirmation       string                      `json:"confirmation"`
	Derived            purgeDerived                `json:"derived"`
	NotebookUnchanged  bool                        `json:"notebookUnchanged"`
	RecordsUnsearched  bool                        `json:"recordsUnsearched"`
	Exposure           string                      `json:"exposure"`
}

func preparePurgeHTTP(t *testing.T, app *Server, scope workspace.PurgeScope) webPurgePlan {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"scope": scope})
	w := call(app, "POST", "/api/purge/prepare", string(body), testToken)
	if w.Code != 200 {
		t.Fatalf("prepare: HTTP %d %s", w.Code, w.Body)
	}
	var plan webPurgePlan
	if err := json.Unmarshal(w.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	return plan
}

func executePurgeHTTP(app *Server, plan webPurgePlan, reason string) (int, purgeReceipt, string) {
	body, _ := json.Marshal(map[string]any{"scope": plan.Scope, "revision": plan.Revision, "confirmation": plan.Confirmation, "reason": reason})
	w := call(app, "POST", "/api/purge/execute", string(body), testToken)
	var receipt purgeReceipt
	json.Unmarshal(w.Body.Bytes(), &receipt)
	return w.Code, receipt, w.Body.String()
}

// An Owner or Admin plans a text purge, sees where else the text is, and
// runs exactly that plan with a reason; the notes keep their shape, an old
// revision reads its replacement, and the receipt keeps who, why and how
// much, never the text.
func TestPurgeTextThroughTheBrowserRoutes(t *testing.T) {
	app, store := fixture(t)
	secret := webPurgeSecret()
	first := savePurgeNote(t, store, "notes/keys.md", "# Keys\n\nThe staging key is "+secret+".\n", "")
	second := savePurgeNote(t, store, "notes/keys.md", "# Keys\n\nThe staging key is "+secret+", rotated soon.\n", first.Revision)
	savePurgeNote(t, store, "notes/other.md", "# Other\n\nAlso "+secret+" here.\n", "")

	plan := preparePurgeHTTP(t, app, workspace.PurgeScope{Kind: workspace.PurgeText, Text: secret, Paths: []string{"notes/keys.md"}})
	if plan.Confirmation == "" || plan.Blocked != "" || len(plan.Notes) != 1 || plan.Notes[0].Path != "notes/keys.md" || plan.Versions != 2 {
		t.Fatalf("plan: %+v", plan)
	}
	if len(plan.Elsewhere) != 1 || plan.Elsewhere[0].Path != "notes/other.md" {
		t.Fatalf("the plan must report the text outside its scope: %+v", plan.Elsewhere)
	}
	if !strings.Contains(plan.Exposure, "rotate") || !strings.Contains(plan.Exposure, "GitHub") || !strings.Contains(plan.Exposure, "backups") {
		t.Fatalf("exposure: %q", plan.Exposure)
	}
	// Including the other occurrence is a new plan with its own confirmation.
	all := preparePurgeHTTP(t, app, workspace.PurgeScope{Kind: workspace.PurgeText, Text: secret, Paths: []string{"notes/keys.md", "notes/other.md"}})
	if all.Confirmation == plan.Confirmation || len(all.Notes) != 2 || len(all.Elsewhere) != 0 {
		t.Fatalf("including the other note: %+v", all)
	}

	if code, _, body := executePurgeHTTP(app, webPurgePlan{Scope: all.Scope, Revision: all.Revision, Confirmation: plan.Confirmation}, "Leaked staging key"); code != http.StatusConflict {
		t.Fatalf("another plan's confirmation: HTTP %d %s", code, body)
	}
	if code, _, body := executePurgeHTTP(app, all, ""); code != http.StatusBadRequest {
		t.Fatalf("no reason: HTTP %d %s", code, body)
	}
	if code, _, body := executePurgeHTTP(app, all, "Remove "+secret); code != http.StatusBadRequest || strings.Contains(body, secret) {
		t.Fatalf("a reason quoting the text: HTTP %d %s", code, body)
	}
	code, receipt, body := executePurgeHTTP(app, all, "Leaked staging key")
	if code != 200 || receipt.Purge == "" || receipt.Rewritten < 3 || receipt.Kind != workspace.PurgeText || receipt.Reason != "Leaked staging key" || receipt.Notes != 2 || receipt.Actor != catalog.Bootstrap {
		t.Fatalf("execute: HTTP %d %s", code, body)
	}
	if strings.Contains(body, secret) || strings.Contains(body, "notes/keys.md") {
		t.Fatalf("the receipt holds purged content or paths: %s", body)
	}
	doc, err := store.Read(t.Context(), "notes/keys.md", "")
	if err != nil || string(doc.Content) != "# Keys\n\nThe staging key is "+workspace.PurgedText+", rotated soon.\n" {
		t.Fatalf("after the purge: %q %v", doc.Content, err)
	}
	// An old revision, as a history pin or citation holds it, reads what
	// stands for it now.
	w := call(app, "GET", "/api/note?path=notes/keys.md&revision="+second.Revision, "", testToken)
	var old noteOutput
	json.Unmarshal(w.Body.Bytes(), &old)
	if w.Code != 200 || old.Revision == second.Revision || !strings.Contains(old.Content, workspace.PurgedText) || strings.Contains(w.Body.String(), secret) {
		t.Fatalf("old revision: HTTP %d %s", w.Code, w.Body)
	}
	// Running the same plan again is refused: the notebook changed.
	if code, _, body := executePurgeHTTP(app, all, "Again"); code != http.StatusConflict {
		t.Fatalf("replayed plan: HTTP %d %s", code, body)
	}
	w = call(app, "GET", "/api/purge", "", testToken)
	if w.Code != 200 || !strings.Contains(w.Body.String(), receipt.Purge) || !strings.Contains(w.Body.String(), "Leaked staging key") || strings.Contains(w.Body.String(), secret) {
		t.Fatalf("receipts: HTTP %d %s", w.Code, w.Body)
	}
}

// Purging a whole note removes it from every version; a pin to one of its
// versions reads as purged, not as missing.
func TestPurgeNoteMakesItsPinsReadPurged(t *testing.T) {
	app, store := fixture(t)
	first := savePurgeNote(t, store, "notes/customer.md", "# Customer\n\nAccount holder: Example Person, card ending 4242.\n", "")
	savePurgeNote(t, store, "notes/kept.md", "# Kept\n", "")
	plan := preparePurgeHTTP(t, app, workspace.PurgeScope{Kind: workspace.PurgeNote, Path: "notes/customer.md"})
	if plan.Blocked != "" || len(plan.Notes) != 1 || plan.Versions != 1 || plan.Notes[0].Versions[0] != first.Revision {
		t.Fatalf("plan: %+v", plan)
	}
	code, receipt, body := executePurgeHTTP(app, plan, "Customer data must not be kept")
	if code != 200 || receipt.Kind != workspace.PurgeNote || strings.Contains(body, "customer.md") {
		t.Fatalf("execute: HTTP %d %s", code, body)
	}
	if _, err := store.Read(t.Context(), "notes/customer.md", ""); err == nil {
		t.Fatal("the purged note is still there")
	}
	w := call(app, "GET", "/api/note?path=notes/customer.md&revision="+first.Revision, "", testToken)
	if w.Code != http.StatusGone || !strings.Contains(w.Body.String(), "purged") || strings.Contains(w.Body.String(), "4242") {
		t.Fatalf("a pin to the purged note: HTTP %d %s", w.Code, w.Body)
	}
}

// Only a notebook Owner or an Admin prepares or runs a purge.
func TestPurgeRoutesNeedAnOwner(t *testing.T) {
	app, store := fixture(t)
	if err := app.ConfigureProxy(proxyTestSecret, []string{"127.0.0.1/32"}); err != nil {
		t.Fatal(err)
	}
	secret := webPurgeSecret()
	savePurgeNote(t, store, "notes/a.md", "# A\n\n"+secret+"\n", "")
	for user, role := range map[string]string{"reader": "read", "writer": "edit", "keeper": "owner"} {
		if err := app.catalog.Member(catalog.Bootstrap, "default", user, role); err != nil {
			t.Fatal(err)
		}
	}
	scope, _ := json.Marshal(map[string]any{"scope": workspace.PurgeScope{Kind: workspace.PurgeText, Text: secret}})
	for _, user := range []string{"reader", "writer"} {
		if w := proxyCall(app, user, "POST", "/p/default/api/purge/prepare", string(scope)); w.Code != http.StatusForbidden || strings.Contains(w.Body.String(), secret) {
			t.Fatalf("%s prepares: HTTP %d %s", user, w.Code, w.Body)
		}
		if w := proxyCall(app, user, "POST", "/p/default/api/purge/execute", `{"scope":{"kind":"text","text":"x"},"revision":"","confirmation":"x","reason":"x"}`); w.Code != http.StatusForbidden {
			t.Fatalf("%s executes: HTTP %d %s", user, w.Code, w.Body)
		}
	}
	w := proxyCall(app, "keeper", "POST", "/p/default/api/purge/prepare", string(scope))
	var plan webPurgePlan
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &plan) != nil || plan.Confirmation == "" {
		t.Fatalf("an owner prepares: HTTP %d %s", w.Code, w.Body)
	}
	body, _ := json.Marshal(map[string]any{"scope": plan.Scope, "revision": plan.Revision, "confirmation": plan.Confirmation, "reason": "Leaked key"})
	w = proxyCall(app, "keeper", "POST", "/p/default/api/purge/execute", string(body))
	var receipt purgeReceipt
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &receipt) != nil || receipt.Actor != "keeper" {
		t.Fatalf("an owner executes: HTTP %d %s", w.Code, w.Body)
	}
	// An operator label belongs to the instance access key alone.
	body, _ = json.Marshal(map[string]any{"scope": plan.Scope, "revision": plan.Revision, "confirmation": plan.Confirmation, "reason": "x", "operator": "someone"})
	if w := proxyCall(app, "keeper", "POST", "/p/default/api/purge/execute", string(body)); w.Code != http.StatusBadRequest {
		t.Fatalf("an owner naming an operator: HTTP %d %s", w.Code, w.Body)
	}
}

// Text the notebook no longer holds, but private records outside it still
// quote, is redacted from those records only by an Admin; an Owner cannot
// rewrite other people's transcripts with it.
func TestPurgeOfRecordsOnlyNeedsAnAdmin(t *testing.T) {
	app, store := fixture(t)
	if err := app.ConfigureProxy(proxyTestSecret, []string{"127.0.0.1/32"}); err != nil {
		t.Fatal(err)
	}
	if err := app.catalog.Member(catalog.Bootstrap, "default", "keeper", "owner"); err != nil {
		t.Fatal(err)
	}
	savePurgeNote(t, store, "notes/a.md", "# A\n", "")
	secret := webPurgeSecret()
	conversation, err := app.chat.Store.Create("reader", "default", chat.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.chat.Store.Update("reader", "default", conversation.ID, func(c *chat.Conversation) error { c.Title = "Pasted " + secret; return nil }); err != nil {
		t.Fatal(err)
	}
	scope, _ := json.Marshal(map[string]any{"scope": workspace.PurgeScope{Kind: workspace.PurgeText, Text: secret}})
	// The Owner is not told whether other people's records hold text the
	// notebook does not: that would let them read those records by guessing.
	w := proxyCall(app, "keeper", "POST", "/p/default/api/purge/prepare", string(scope))
	var plan webPurgePlan
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &plan) != nil || !plan.NotebookUnchanged || plan.Derived.Chats != 0 || !plan.RecordsUnsearched {
		t.Fatalf("the owner's plan: HTTP %d %s", w.Code, w.Body)
	}
	body, _ := json.Marshal(map[string]any{"scope": plan.Scope, "revision": plan.Revision, "confirmation": plan.Confirmation, "reason": "Pasted key"})
	if w := proxyCall(app, "keeper", "POST", "/p/default/api/purge/execute", string(body)); w.Code != http.StatusForbidden {
		t.Fatalf("an owner redacts records only: HTTP %d %s", w.Code, w.Body)
	}
	plan = preparePurgeHTTP(t, app, workspace.PurgeScope{Kind: workspace.PurgeText, Text: secret})
	if !plan.NotebookUnchanged || plan.Derived.Chats != 1 || plan.RecordsUnsearched {
		t.Fatalf("the admin's plan: %+v", plan)
	}
	code, receipt, out := executePurgeHTTP(app, plan, "Pasted key")
	if code != 200 || !receipt.NotebookUnchanged || receipt.Derived.Chats != 1 || receipt.Rewritten != 0 {
		t.Fatalf("an admin redacts records only: HTTP %d %s", code, out)
	}
	c, err := app.chat.Store.Read("reader", "default", conversation.ID)
	if err != nil || strings.Contains(c.Title, secret) {
		t.Fatalf("transcript: %q %v", c.Title, err)
	}
	status, _ := store.Status(t.Context())
	if status.Revision != plan.Revision {
		t.Fatal("the notebook changed")
	}
}

// A plan tells an Owner which records outside the notebook quote what the
// purge erases only for content the notebook holds: never for guessed text or
// a pattern the notebook does not hold, and an agent's plan never at all, so
// nobody reads other members' private chats or Jev records by probing. A
// pattern purge matches those records against the texts it matched in the
// notebook, never against the pattern, so running it rewrites nothing else
// in them.
func TestPurgePlanCountsOnlyRecordsQuotingWhatTheNotebookHolds(t *testing.T) {
	app, store := fixture(t)
	if err := app.ConfigureProxy(proxyTestSecret, []string{"127.0.0.1/32"}); err != nil {
		t.Fatal(err)
	}
	for user, role := range map[string]string{"keeper": "owner", "reader": "read"} {
		if err := app.catalog.Member(catalog.Bootstrap, "default", user, role); err != nil {
			t.Fatal(err)
		}
	}
	savePurgeNote(t, store, "notes/a.md", "# A\n\nThe token is tok-1111.\n", "")
	conversation, err := app.chat.Store.Create("reader", "default", chat.Options{})
	if err != nil {
		t.Fatal(err)
	}
	err = app.chat.Store.Update("reader", "default", conversation.ID, func(c *chat.Conversation) error {
		c.Title = "my bank PIN is 7391"
		c.AddEvent("run-1", "tool", map[string]any{"result": "Tokens tok-1111 and tok-2222"})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.jevState.records.Write("jobs/"+strings.Repeat("c", 64)+".json", map[string]any{"version": 2, "project": "default", "message": "The PIN is 7391"}); err != nil {
		t.Fatal(err)
	}
	prepare := func(scope workspace.PurgeScope) webPurgePlan {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"scope": scope})
		w := proxyCall(app, "keeper", "POST", "/p/default/api/purge/prepare", string(body))
		var plan webPurgePlan
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &plan) != nil {
			t.Fatalf("prepare %+v: HTTP %d %s", scope, w.Code, w.Body)
		}
		return plan
	}
	session := purgeAgents(t, app, "notes:read notes:write notes:manage", "keeper")[0]
	for _, guess := range []workspace.PurgeScope{{Kind: workspace.PurgeText, Text: "bank PIN is 7391"}, {Kind: workspace.PurgeText, Pattern: `PIN is 7[0-4]`}} {
		plan := prepare(guess)
		if !plan.NotebookUnchanged || !plan.RecordsUnsearched || plan.Derived != (purgeDerived{}) {
			t.Errorf("an Owner's plan for %+v reports records outside the notebook: %+v", guess, plan.Derived)
		}
		raw := actionCall(t, session, "prepare_purge", map[string]any{"project": "default", "kind": "text", "text": guess.Text, "pattern": guess.Pattern}, false)
		var out preparePurgeOutput
		if err := json.Unmarshal(raw, &out); err != nil || out.Plan.Derived != nil {
			t.Errorf("an agent's plan for %+v reports records outside the notebook: %s", guess, raw)
		}
	}
	// The pattern matches tok-1111 in the notebook; the chat also quotes
	// tok-2222, which the notebook never held. The chat is reader's, so the
	// Owner is told only that other people's records are redacted too.
	plan := prepare(workspace.PurgeScope{Kind: workspace.PurgeText, Pattern: `tok-\d{4}`})
	if plan.NotebookUnchanged || plan.RecordsUnsearched || plan.Derived != (purgeDerived{Own: true, Others: purgeOthersRedacted}) {
		t.Fatalf("a pattern the notebook holds: %+v", plan)
	}
	body, _ := json.Marshal(map[string]any{"scope": plan.Scope, "revision": plan.Revision, "confirmation": plan.Confirmation, "reason": "Leaked tokens"})
	if w := proxyCall(app, "keeper", "POST", "/p/default/api/purge/execute", string(body)); w.Code != 200 {
		t.Fatalf("execute: HTTP %d %s", w.Code, w.Body)
	}
	c, err := app.chat.Store.Read("reader", "default", conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(c)
	if strings.Contains(string(raw), "tok-1111") || !strings.Contains(string(raw), "tok-2222") || c.Title != "my bank PIN is 7391" {
		t.Fatalf("the transcript after the purge: %s", raw)
	}
}

// A private chat of another notebook whose corpus drew on this one quotes
// what this notebook held; a purge counts and redacts it like the notebook's
// own chats. Another notebook's chat that did not draw on it is not touched.
func TestPurgeRedactsChatsOfOtherNotebooksThatDrewOnIt(t *testing.T) {
	app, store := fixture(t)
	if err := app.catalog.CreateKind(catalog.Bootstrap, "other", "Other", "notes"); err != nil {
		t.Fatal(err)
	}
	secret := webPurgeSecret()
	savePurgeNote(t, store, "notes/keys.md", "# Keys\n\nThe key is "+secret+".\n", "")
	quote := func(corpus *chat.Corpus) chat.Conversation {
		t.Helper()
		conversation, err := app.chat.Store.Create("admin", "other", chat.Options{Corpus: corpus})
		if err != nil {
			t.Fatal(err)
		}
		err = app.chat.Store.Update("admin", "other", conversation.ID, func(c *chat.Conversation) error {
			c.AddEvent("run-1", "tool", map[string]any{"result": "default/notes/keys.md: The key is " + secret})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return conversation
	}
	drew := quote(&chat.Corpus{NoteProjects: []string{"other", "default"}, Version: 1, Archives: "exclude", Drafts: "exclude"})
	apart := quote(nil)
	plan := preparePurgeHTTP(t, app, workspace.PurgeScope{Kind: workspace.PurgeText, Text: secret})
	if plan.Derived.Chats != 1 {
		t.Fatalf("the plan counts %d chats", plan.Derived.Chats)
	}
	code, receipt, body := executePurgeHTTP(app, plan, "Leaked key")
	if code != 200 || receipt.Derived.Chats != 1 {
		t.Fatalf("execute: HTTP %d %s", code, body)
	}
	for _, c := range []struct {
		conversation chat.Conversation
		holds        bool
	}{{drew, false}, {apart, true}} {
		read, err := app.chat.Store.Read("admin", "other", c.conversation.ID)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(read)
		if strings.Contains(string(raw), secret) != c.holds {
			t.Errorf("conversation %s (holds the key: %v): %s", c.conversation.ID, c.holds, raw)
		}
	}
}

// A versions purge redacts, in records outside the notebook, the lines only
// the erased versions held, not lines the notebook keeps in other notes; and
// its reason may not quote even part of what it erases.
func TestPurgeOfVersionsRedactsOnlyLinesTheNotebookDoesNotKeep(t *testing.T) {
	app, store := fixture(t)
	const template = "This line comes from the shared meeting template."
	const card = "Customer card number 4242 4242 4242 4242."
	first := savePurgeNote(t, store, "notes/draft.md", "# Draft\n\n"+template+"\n\n"+card+"\n", "")
	savePurgeNote(t, store, "notes/draft.md", "# Draft\n\n"+template+"\n\nNo customer data.\n", first.Revision)
	savePurgeNote(t, store, "notes/meeting.md", "# Meeting\n\n"+template+"\n", "")
	conversation, err := app.chat.Store.Create("admin", "default", chat.Options{})
	if err != nil {
		t.Fatal(err)
	}
	err = app.chat.Store.Update("admin", "default", conversation.ID, func(c *chat.Conversation) error {
		c.AddEvent("run-1", "tool", map[string]any{"result": template + "\n" + card})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	plan := preparePurgeHTTP(t, app, workspace.PurgeScope{Kind: workspace.PurgeVersions, Path: "notes/draft.md", Versions: []string{first.Revision}})
	if plan.Derived.Chats != 1 || plan.Blocked != "" {
		t.Fatalf("plan: %+v", plan)
	}
	if code, _, body := executePurgeHTTP(app, plan, "Card 4242 was in an early draft"); code != http.StatusBadRequest || strings.Contains(body, "4242") {
		t.Fatalf("a reason quoting part of an erased line: HTTP %d %s", code, body)
	}
	if code, _, body := executePurgeHTTP(app, plan, "Customer data in an early draft"); code != 200 {
		t.Fatalf("execute: HTTP %d %s", code, body)
	}
	read, err := app.chat.Store.Read("admin", "default", conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	events := string(read.Events[0].Data)
	if strings.Contains(events, "4242") || !strings.Contains(events, template) {
		t.Fatalf("the transcript after the purge: %s", events)
	}
}

// A purge refuses a reason with a word only the erased note holds, even one
// as harmless as a year, and says that is why, so the person can give the
// reason without it.
func TestPurgeReasonRefusalSaysAWordOnlyTheErasedNoteHoldsIsWhy(t *testing.T) {
	app, store := fixture(t)
	savePurgeNote(t, store, "notes/a.md", "# A\n\nWritten in 2026 for the audit.\n", "")
	plan := preparePurgeHTTP(t, app, workspace.PurgeScope{Kind: workspace.PurgeNote, Path: "notes/a.md"})
	if code, _, body := executePurgeHTTP(app, plan, "Cleanup after the 2026 audit"); code != http.StatusBadRequest || !strings.Contains(body, "a word or number") {
		t.Fatalf("a reason naming a year only the note holds: HTTP %d %s", code, body)
	}
	if code, _, body := executePurgeHTTP(app, plan, "Cleanup after the audit"); code != 200 {
		t.Fatalf("execute: HTTP %d %s", code, body)
	}
}

// Records outside the notebook are redacted in their values only: keys and
// the strings that identify or date a record stay, so a pattern such as a
// four-digit number leaves the record readable.
func TestPurgeCleanerRedactsContentValuesOnly(t *testing.T) {
	redactor, err := workspace.NewPurgeRedactor(workspace.PurgeScope{Kind: workspace.PurgeText, Pattern: `\d{4}`})
	if err != nil {
		t.Fatal(err)
	}
	cleaner := &purgeCleaner{text: &redactor}
	raw := []byte(`{"id":"c-2024","createdAt":"2026-11-22T10:00:00Z","title":"PIN 7391","notes":{"k-1234":"v 5678"}}`)
	out, changed, err := cleaner.document(raw)
	want := `{"createdAt":"2026-11-22T10:00:00Z","id":"c-2024","notes":{"k-1234":"v [purged]"},"title":"PIN [purged]"}`
	if err != nil || !changed || string(out) != want {
		t.Fatalf("redacted: %s %v %v", out, changed, err)
	}
}

// A revision a purge rewrote is mapped wherever a record holds it, also under
// the keys that identify a record, which are never redacted.
func TestPurgeCleanerMapsRevisionsUnderEveryKey(t *testing.T) {
	old, now := strings.Repeat("a", 40), strings.Repeat("b", 40)
	redactor, err := workspace.NewPurgeRedactor(workspace.PurgeScope{Kind: workspace.PurgeText, Text: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	cleaner := &purgeCleaner{text: &redactor, remap: func(revision string) (string, bool) { return now, revision == old }}
	raw := []byte(`{"id":"` + old + `","run":"secret ` + old + `","revision":"` + old + `","citations":[{"kind":"notebook","id":"` + old + `"}]}`)
	out, changed, err := cleaner.document(raw)
	want := `{"citations":[{"id":"` + now + `","kind":"notebook"}],"id":"` + now + `","revision":"` + now + `","run":"secret ` + now + `"}`
	if err != nil || !changed || string(out) != want {
		t.Fatalf("remapped: %s %v %v", out, changed, err)
	}
}

// An asset pinned to a revision a purge rewrote reads the revision that
// stands for it now, as a note pinned there does.
func TestPurgeMapsAssetsPinnedToRewrittenRevisions(t *testing.T) {
	app, store := fixture(t)
	secret := webPurgeSecret()
	savePurgeNote(t, store, "notes/keys.md", "# Keys\n\n"+secret+"\n", "")
	image := []byte{0x89, 'P', 'N', 'G', 0}
	asset, err := store.Save(t.Context(), "assets/photo.png", image, "", "fixture")
	if err != nil {
		t.Fatal(err)
	}
	plan := preparePurgeHTTP(t, app, workspace.PurgeScope{Kind: workspace.PurgeText, Text: secret})
	if code, _, body := executePurgeHTTP(app, plan, "Leaked key"); code != 200 {
		t.Fatal(body)
	}
	if w := call(app, "GET", "/assets/assets/photo.png?revision="+asset.Revision, "", testToken); w.Code != 200 || w.Body.String() != string(image) {
		t.Fatalf("an asset pinned to a rewritten revision: HTTP %d %q", w.Code, w.Body)
	}
}
