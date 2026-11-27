package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"testing"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/purgehttp"
	"forgetmenote/internal/workspace"
)

// A purge redacts everyone's records that quoted the content, but only an
// instance Admin is told how many. An Owner who is not an Admin is told how
// many of their own chats, proposals and judgment records quote it, and that
// other people's are redacted too, without a number, in the browser plan,
// in the plan of an agent's prepare_purge they open, in the purge's receipt
// and in the receipts listed later; another Owner is told no counts of
// their purge. So a dictionary of guesses written into a note tells an
// Owner nothing of their colleagues' private records.
func TestOnlyAnAdminIsToldHowManyOfOtherPeoplesRecordsQuoteAPurge(t *testing.T) {
	app, store := fixture(t)
	if err := app.ConfigureProxy(proxyTestSecret, []string{"127.0.0.1/32"}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := app.catalog.SetUser(catalog.Bootstrap, catalog.User{ID: "admin", Admin: true}); err != nil {
		t.Fatal(err)
	}
	for user, role := range map[string]string{"keeper": "owner", "partner": "owner", "reader": "read"} {
		if err := app.catalog.Member(catalog.Bootstrap, "default", user, role); err != nil {
			t.Fatal(err)
		}
	}
	secret := webPurgeSecret()
	savePurgeNote(t, store, "notes/keys.md", "# Keys\n\nThe key is "+secret+".\n", "")
	// keeper's own records and reader's quote the key; reader's chat also
	// quotes a PIN no note holds.
	quote := func(user, text string) {
		t.Helper()
		c, err := app.chat.Store.Create(user, "default", chat.Options{})
		if err != nil {
			t.Fatal(err)
		}
		if err := app.chat.Store.Update(user, "default", c.ID, func(c *chat.Conversation) error {
			c.AddEvent("run-1", "user", map[string]any{"text": text})
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	quote("keeper", "Where did "+secret+" leak?")
	quote("reader", "The key "+secret+" and my bank PIN is 7391")
	for i, owner := range []string{"keeper", "reader"} {
		job := strings.Repeat(string(rune('a'+i)), 64)
		if err := app.judgments.Records().Write("jobs/"+job+".json", map[string]any{"version": 2, "project": "default", "owner": owner, "message": "Checked " + secret}); err != nil {
			t.Fatal(err)
		}
		if err := app.judgments.Records().Write("results/"+job+"-00000001.json", map[string]any{"version": 1, "job": job, "quote": secret}); err != nil {
			t.Fatal(err)
		}
	}
	engine, release, err := app.acquireEngine(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	for _, author := range []string{"keeper", "reader"} {
		if _, err := engine.Propose(ctx, author, workspace.EditGroup{ID: "quote-" + author, Summary: "Add the key", Edits: []workspace.NoteEdit{{Path: "notes/proposed-" + author + ".md", Content: "# Proposed\n\nUse " + secret + ".\n"}}}); err != nil {
			t.Fatal(err)
		}
	}
	release()

	prepare := func(user string, body any) webPurgePlan {
		t.Helper()
		raw, _ := json.Marshal(body)
		w := proxyCall(app, user, "POST", "/p/default/api/purge/prepare", string(raw))
		var plan webPurgePlan
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &plan) != nil {
			t.Fatalf("%s prepares: HTTP %d %s", user, w.Code, w.Body)
		}
		return plan
	}
	scope := workspace.PurgeScope{Kind: workspace.PurgeText, Text: secret}
	own := purgehttp.Derived{Chats: 1, Proposals: 1, JudgmentRecords: 2, Own: true, Others: purgehttp.OthersRedacted}
	all := purgehttp.Derived{Chats: 2, Proposals: 2, JudgmentRecords: 4}
	if got := prepare("keeper", map[string]any{"scope": scope}).Derived; got != own {
		t.Errorf("the Owner's plan counts %+v, want %+v", got, own)
	}
	if got := prepare("admin", map[string]any{"scope": scope}).Derived; got != all {
		t.Errorf("the Admin's plan counts %+v, want %+v", got, all)
	}

	// An agent's plan counts nothing; the person who opens it gets the
	// counts their role allows.
	sessions := purgeAgents(t, app, "notes:read notes:write notes:manage", "keeper", "admin")
	for i, user := range []string{"keeper", "admin"} {
		raw := actionCall(t, sessions[i], "prepare_purge", map[string]any{"project": "default", "kind": "text", "text": secret}, false)
		var out struct {
			Plan       map[string]json.RawMessage `json:"plan"`
			ConfirmURL string                     `json:"confirmUrl"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		if _, ok := out.Plan["derived"]; ok || string(out.Plan["recordsCountedOnReview"]) != "true" {
			t.Errorf("%s's agent's plan counts records outside the notebook: %s", user, raw)
		}
		link, _ := url.Parse(out.ConfirmURL)
		want := map[string]purgehttp.Derived{"keeper": own, "admin": all}[user]
		if got := prepare(user, map[string]any{"token": link.Query().Get("purge")}).Derived; got != want {
			t.Errorf("%s opens their agent's plan: counts %+v, want %+v", user, got, want)
		}
	}

	// A guess that only another person's record holds is told apart from a
	// wrong one by nothing.
	var guesses []purgehttp.Derived
	for _, pin := range []string{"7391", "7392"} {
		savePurgeNote(t, store, "notes/scratch-"+pin+".md", "bank PIN is "+pin+"\n", "")
		guesses = append(guesses, prepare("keeper", map[string]any{"scope": workspace.PurgeScope{Kind: workspace.PurgeText, Text: "bank PIN is " + pin, Paths: []string{"notes/scratch-" + pin + ".md"}}}).Derived)
	}
	if guesses[0] != guesses[1] {
		t.Errorf("a right guess is told apart from a wrong one: %+v %+v", guesses[0], guesses[1])
	}

	// The Owner runs the purge: every record is redacted, and the receipt
	// counts their own.
	plan := prepare("keeper", map[string]any{"scope": scope})
	body, _ := json.Marshal(map[string]any{"scope": plan.Scope, "revision": plan.Revision, "confirmation": plan.Confirmation, "reason": "Leaked key"})
	w := proxyCall(app, "keeper", "POST", "/p/default/api/purge/execute", string(body))
	var receipt purgehttp.Receipt
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &receipt) != nil {
		t.Fatalf("execute: HTTP %d %s", w.Code, w.Body)
	}
	if receipt.Derived != own || receipt.ActorDerived != nil {
		t.Errorf("the Owner's receipt counts %+v, want %+v", receipt.Derived, own)
	}
	var left int
	if err := app.db.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM chat_conversations WHERE position(convert_to($1, 'UTF8') IN record) > 0)
		+ (SELECT count(*) FROM jev_records WHERE position($1 IN body::text) > 0)`, secret).Scan(&left); err != nil || left != 0 {
		t.Fatalf("%d records outside the notebook still quote the key (%v)", left, err)
	}
	listed := func(user string) purgehttp.Derived {
		t.Helper()
		w := proxyCall(app, user, "GET", "/p/default/api/purge", "")
		var out struct {
			Receipts []purgehttp.Receipt `json:"receipts"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil || len(out.Receipts) != 1 || out.Receipts[0].ActorDerived != nil {
			t.Fatalf("%s lists receipts: HTTP %d %s", user, w.Code, w.Body)
		}
		return out.Receipts[0].Derived
	}
	if got := listed("keeper"); got != own {
		t.Errorf("the Owner's listed receipt counts %+v, want %+v", got, own)
	}
	if got := listed("partner"); got != (purgehttp.Derived{Withheld: true, Others: purgehttp.OthersWithheld}) {
		t.Errorf("another Owner's listed receipt counts %+v", got)
	}
	if got := listed("admin"); got.Chats != 2 || got.JudgmentRecords != 4 || got.Proposals != 2 || got.Own || got.Withheld || got.Others != "" {
		t.Errorf("the Admin's listed receipt counts %+v", got)
	}
}

// A record that quoted a purge's content and cannot be redacted, here
// another person's chat transcript holding a field this release does not
// know, as a later release may write, is left as it is and reported. An
// Owner who is not an instance Admin is told so without a number, in the
// purge's receipt and in the receipts listed later, as such records may be
// other people's; an Admin is told how many. judgment records that cannot be
// redacted are reported the same way.
func TestAnOwnerIsToldWithoutANumberThatRecordsCouldNotBeRedacted(t *testing.T) {
	app, store := fixture(t)
	if err := app.ConfigureProxy(proxyTestSecret, []string{"127.0.0.1/32"}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := app.catalog.SetUser(catalog.Bootstrap, catalog.User{ID: "admin", Admin: true}); err != nil {
		t.Fatal(err)
	}
	for user, role := range map[string]string{"keeper": "owner", "reader": "read"} {
		if err := app.catalog.Member(catalog.Bootstrap, "default", user, role); err != nil {
			t.Fatal(err)
		}
	}
	secret := webPurgeSecret()
	savePurgeNote(t, store, "notes/keys.md", "# Keys\n\nThe key is "+secret+".\n", "")
	c, err := app.chat.Store.Create("reader", "default", chat.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.chat.Store.Update("reader", "default", c.ID, func(c *chat.Conversation) error {
		c.AddEvent("run-1", "user", map[string]any{"text": "Is " + secret + " the key?"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if tag, err := app.db.Pool.Exec(ctx, `UPDATE chat_conversations SET version = version + 1, record = convert_to('{"laterField":true,', 'UTF8') || substring(record from 2)
		WHERE org = $1 AND id = $2`, app.db.Org, c.ID); err != nil || tag.RowsAffected() != 1 {
		t.Fatal(tag, err)
	}

	w := proxyCall(app, "keeper", "POST", "/p/default/api/purge/prepare", `{"scope":{"kind":"text","text":"`+secret+`"}}`)
	var plan webPurgePlan
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &plan) != nil {
		t.Fatalf("prepare: HTTP %d %s", w.Code, w.Body)
	}
	body, _ := json.Marshal(map[string]any{"scope": plan.Scope, "revision": plan.Revision, "confirmation": plan.Confirmation, "reason": "Leaked key"})
	w = proxyCall(app, "keeper", "POST", "/p/default/api/purge/execute", string(body))
	var receipt purgehttp.Receipt
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &receipt) != nil {
		t.Fatalf("execute: HTTP %d %s", w.Code, w.Body)
	}
	numberless := "some chat transcripts, perhaps other people's, cannot be redacted"
	told := func(who string, warnings []string, want, not string) {
		t.Helper()
		joined := strings.Join(warnings, "\n")
		if !strings.Contains(joined, want) || strings.Contains(joined, not) {
			t.Errorf("%s is warned %q", who, joined)
		}
	}
	told("the Owner running the purge", receipt.Warnings, numberless, "1 chat transcripts")
	listed := func(user string) []string {
		t.Helper()
		w := proxyCall(app, user, "GET", "/p/default/api/purge", "")
		var out struct {
			Receipts []purgehttp.Receipt `json:"receipts"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil || len(out.Receipts) != 1 {
			t.Fatalf("%s lists receipts: HTTP %d %s", user, w.Code, w.Body)
		}
		return out.Receipts[0].Warnings
	}
	told("the Owner listing receipts", listed("keeper"), numberless, "1 chat transcripts")
	told("the Admin listing receipts", listed("admin"), "1 chat transcripts cannot be redacted", numberless)

	if err := app.judgments.Records().Write("jobs/"+strings.Repeat("c", 64)+".json", map[string]any{"version": 2, "project": "default", "owner": "reader"}); err != nil {
		t.Fatal(err)
	}
	_, refused := app.judgments.Records().RedactNotebook(ctx, "default", func([]byte) ([]byte, bool, error) { return nil, false, errors.New("refused") }, false)
	if refused == nil {
		t.Fatal("a judgment record the redaction refuses is not reported")
	}
	judgments := purgehttp.Receipt{Actor: "keeper", Warnings: []string{"Some records outside the notebook could not be cleaned: judgment records: " + refused.Error()}}
	told("the Owner, of judgment records", app.purge.ReceiptFor("keeper", judgments).Warnings, "some judgment records, perhaps other people's, cannot be redacted", "1 judgment records")
	told("the Admin, of judgment records", app.purge.ReceiptFor("admin", judgments).Warnings, "1 judgment records cannot be redacted", "perhaps")
}
