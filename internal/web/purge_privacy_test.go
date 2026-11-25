package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/workspace"
)

// visiblePlanHash is what a confirmation is when it binds only what the
// plan shows: computed from the plan as its caller received it.
func visiblePlanHash(t *testing.T, project string, raw []byte) (string, string) {
	t.Helper()
	var plan workspace.PurgePlan
	if err := json.Unmarshal(raw, &plan); err != nil {
		t.Fatal(err)
	}
	confirmation := plan.Confirmation
	plan.Confirmation = ""
	for i := range plan.Live {
		plan.Live[i].Pending = false
	}
	hashed, _ := json.Marshal(struct {
		Notebook string              `json:"notebook"`
		Plan     workspace.PurgePlan `json:"plan"`
	}{project, plan})
	sum := sha256.Sum256(hashed)
	return confirmation, hex.EncodeToString(sum[:])
}

// purgeOtherNotebooks is what a plan reports of the notebooks around the
// one it erases: the occurrences it lists, which may name other notebooks.
type purgeOtherNotebooks struct {
	Shared    []workspace.PurgeOccurrence `json:"shared"`
	Elsewhere []workspace.PurgeOccurrence `json:"elsewhere"`
}

func credentialMCPSession(t *testing.T, app *Server, token string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "restricted-agent", Version: "1"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: app.origin + "/mcp", HTTPClient: &http.Client{Transport: attributionTransport{token: token}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

// A notebook stores the same content as another when it holds a
// byte-identical file, such as an attachment or a note without an fmn-id.
// Whoever plans or runs a purge learns nothing of notebooks they cannot read
// from it: a guess that another notebook, or another person's private Inbox,
// holds gets the same plan, the same confirmation (which binds only what the
// plan shows) and the same receipt as a guess that nothing holds, through
// the browser, an agent's prepare_purge, a person's own Inbox and a
// credential restricted to one notebook. An Admin, who may read every
// notebook, still sees which keep the content.
func TestPurgePlansAndReceiptsTellNothingOfNotebooksTheCallerCannotRead(t *testing.T) {
	app, store := fixture(t)
	if err := app.ConfigureProxy(proxyTestSecret, []string{"127.0.0.1/32"}); err != nil {
		t.Fatal(err)
	}
	if err := app.catalog.Create(catalog.Bootstrap, "hr-private", "HR"); err != nil {
		t.Fatal(err)
	}
	hr, err := app.catalog.Access(catalog.Bootstrap, "hr-private", "edit")
	if err != nil {
		t.Fatal(err)
	}
	const salary = "# Salaries\n\nAlice Example: 182000\n"
	const scan = "%PDF-1.4\nscan of passport 123456789\n%%EOF\n"
	savePurgeNote(t, hr, "pay/2026.md", salary, "")
	savePurgeNote(t, hr, "files/passport.pdf", scan, "")
	inbox, err := app.catalog.EnsureInbox("alice")
	if err != nil {
		t.Fatal(err)
	}
	alice, err := app.catalog.Access("alice", inbox.ID, "edit")
	if err != nil {
		t.Fatal(err)
	}
	const quick = "Call the clinic about the biopsy result\n"
	savePurgeNote(t, alice, "quick.md", quick, "")
	for user, role := range map[string]string{"keeper": "owner", "reader": "read"} {
		if err := app.catalog.Member(catalog.Bootstrap, "default", user, role); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"hr-private", inbox.ID} {
		if _, err := app.catalog.Access("keeper", id, "read"); err == nil {
			t.Fatalf("keeper can read %s", id)
		}
	}
	agent := purgeAgents(t, app, "notes:read notes:write notes:manage", "keeper")[0]

	prepare := func(user, project string, scope workspace.PurgeScope) (purgeOtherNotebooks, []byte) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"scope": scope})
		w := proxyCall(app, user, "POST", "/p/"+project+"/api/purge/prepare", string(body))
		var other purgeOtherNotebooks
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &other) != nil {
			t.Fatalf("%s prepares %+v in %s: HTTP %d %s", user, scope, project, w.Code, w.Body)
		}
		if confirmation, visible := visiblePlanHash(t, project, w.Body.Bytes()); confirmation != visible {
			t.Errorf("%s's plan of %+v in %s has a confirmation that binds more than the plan shows", user, scope, project)
		}
		return other, w.Body.Bytes()
	}
	execute := func(user, project string, raw []byte) purgeReceipt {
		t.Helper()
		var plan webPurgePlan
		json.Unmarshal(raw, &plan)
		body, _ := json.Marshal(map[string]any{"scope": plan.Scope, "revision": plan.Revision, "confirmation": plan.Confirmation, "reason": "Cleanup"})
		w := proxyCall(app, user, "POST", "/p/"+project+"/api/purge/execute", string(body))
		var receipt purgeReceipt
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &receipt) != nil {
			t.Fatalf("%s executes in %s: HTTP %d %s", user, project, w.Code, w.Body)
		}
		return receipt
	}
	type outcome struct {
		Agent, Note, Text purgeOtherNotebooks
		Removed           [2]int
	}
	guess := func(path, content string) outcome {
		t.Helper()
		savePurgeNote(t, store, path, content, "")
		var o outcome
		raw := actionCall(t, agent, "prepare_purge", map[string]any{"project": "default", "kind": "note", "path": path}, false)
		var out struct {
			Plan purgeOtherNotebooks `json:"plan"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		o.Agent = out.Plan
		if content != scan {
			o.Text, _ = prepare("keeper", "default", workspace.PurgeScope{Kind: workspace.PurgeText, Text: content[:12], Paths: []string{path}})
		}
		var plan []byte
		o.Note, plan = prepare("keeper", "default", workspace.PurgeScope{Kind: workspace.PurgeNote, Path: path})
		receipt := execute("keeper", "default", plan)
		o.Removed = [2]int{receipt.RemovedVersions, receipt.RemovedBlocks}
		return o
	}
	for _, pair := range []struct {
		name, path, wrong, right string
	}{
		{"a salary", "notes/guess.md", "# Salaries\n\nAlice Example: 150000\n", salary},
		{"an attachment", "files/guess.pdf", "%PDF-1.4\nscan of passport 000000000\n%%EOF\n", scan},
		{"another person's Inbox note", "notes/inbox-guess.md", "Call the clinic about the dentist\n", quick},
	} {
		wrong := guess(pair.path, pair.wrong)
		right := guess(pair.path, pair.right)
		if !jsonEqual(wrong, right) {
			t.Errorf("guessing %s: a right guess is told apart from a wrong one:\nwrong %+v\nright %+v", pair.name, wrong, right)
		}
	}
	// Stored receipts list the same counts.
	w := proxyCall(app, "keeper", "GET", "/p/default/api/purge", "")
	var listed struct {
		Receipts []purgeReceipt `json:"receipts"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &listed) != nil || len(listed.Receipts) != 6 {
		t.Fatalf("receipts: HTTP %d %s", w.Code, w.Body)
	}
	counts := map[[2]int]int{}
	for _, r := range listed.Receipts {
		counts[[2]int{r.RemovedVersions, r.RemovedBlocks}]++
	}
	if len(counts) != 1 {
		t.Errorf("the listed receipts tell right guesses from wrong ones: %v", counts)
	}

	// A person who owns no shared notebook probes through their own Inbox.
	own, err := app.catalog.EnsureInbox("reader")
	if err != nil {
		t.Fatal(err)
	}
	readerInbox, err := app.catalog.Access("reader", own.ID, "edit")
	if err != nil {
		t.Fatal(err)
	}
	var seen []purgeOtherNotebooks
	for i, content := range []string{"# Salaries\n\nAlice Example: 150000\n", salary} {
		path := []string{"wrong.md", "right.md"}[i]
		savePurgeNote(t, readerInbox, path, content, "")
		other, _ := prepare("reader", own.ID, workspace.PurgeScope{Kind: workspace.PurgeNote, Path: path})
		seen = append(seen, other)
	}
	if !jsonEqual(seen[0], seen[1]) {
		t.Errorf("a plan in a person's own Inbox tells a right guess from a wrong one: %+v %+v", seen[0], seen[1])
	}

	// A notebook the person can read is listed, but not to a credential
	// restricted to another notebook.
	if err := app.catalog.Create(catalog.Bootstrap, "team-notes", "Team"); err != nil {
		t.Fatal(err)
	}
	if err := app.catalog.Member(catalog.Bootstrap, "team-notes", "keeper", "read"); err != nil {
		t.Fatal(err)
	}
	team, _ := app.catalog.Access(catalog.Bootstrap, "team-notes", "edit")
	const plan = "# Team plan\n\nShip on Friday\n"
	savePurgeNote(t, team, "plan.md", plan, "")
	savePurgeNote(t, store, "notes/team.md", plan, "")
	if other, _ := prepare("keeper", "default", workspace.PurgeScope{Kind: workspace.PurgeNote, Path: "notes/team.md"}); !slices.Contains(other.Shared, workspace.PurgeOccurrence{Path: "team-notes", Kind: "notebook", Versions: 1}) {
		t.Errorf("the Owner's plan does not list a notebook they can read: %+v", other.Shared)
	}
	grant, err := issueCredential(app.catalog, app.oauth, "keeper", CredentialSpec{Name: "only-default", Projects: []string{"default"}, Scopes: []string{"notes:read", "notes:write", "notes:manage"}, Audiences: []string{"mcp"}, Days: 1})
	if err != nil {
		t.Fatal(err)
	}
	restricted := credentialMCPSession(t, app, grant.Token)
	raw := actionCall(t, restricted, "prepare_purge", map[string]any{"project": "default", "kind": "note", "path": "notes/team.md"}, false)
	var out struct {
		Plan purgeOtherNotebooks `json:"plan"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || len(out.Plan.Shared) != 0 {
		t.Errorf("a credential restricted to default learns of another notebook: %s", raw)
	}

	// An Admin may read every notebook, and sees which keep the content.
	if err := app.catalog.SetUser(catalog.Bootstrap, catalog.User{ID: "admin", Admin: true}); err != nil {
		t.Fatal(err)
	}
	savePurgeNote(t, store, "notes/salary.md", salary, "")
	if other, _ := prepare("admin", "default", workspace.PurgeScope{Kind: workspace.PurgeNote, Path: "notes/salary.md"}); !slices.Contains(other.Shared, workspace.PurgeOccurrence{Path: "hr-private", Kind: "notebook", Versions: 1}) {
		t.Errorf("the Admin's plan does not name the notebook that keeps the content: %+v", other.Shared)
	}
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
