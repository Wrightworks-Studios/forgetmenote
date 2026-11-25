package web

import (
	"encoding/base64"
	"encoding/json"
	"forgetmenote/internal/automation"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/connectors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestFinancePackageThroughGoogleHostAndPython(t *testing.T) {
	root := os.Getenv("FMN_TEST_AUTOMATIONS_ROOT")
	if root == "" {
		t.Skip("set FMN_TEST_AUTOMATIONS_ROOT to the private package checkout")
	}
	root = filepath.Join(root, "automations", "finance-inbox")
	raw, err := os.ReadFile(filepath.Join(root, "automation.json"))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := automation.ParseManifest(raw)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{"automation.json": string(raw)}
	for _, name := range manifest.Files {
		raw, err = os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		files[name] = string(raw)
	}
	app, e := packageFixture(t, manifest, files)
	raw, err = os.ReadFile(filepath.Join(root, "examples", "parameters.json"))
	if err != nil {
		t.Fatal(err)
	}
	json.Unmarshal(raw, &e.run.Spec.Script.Parameters)
	p := e.run.Spec.Script.Parameters
	p["decisions"] = false
	p["attachmentNames"] = []string{}
	e.run.Spec.TimeoutSeconds = 120
	e.run.Spec.Script.RetentionHours = 24
	store, err := app.catalog.Access(e.run.Spec.Actor, "default", "edit")
	if err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(filepath.Join(root, "examples", "approved-policy.md"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Save(t.Context(), "approved-policy.md", raw, "", "fixture"); err != nil {
		t.Fatal(err)
	}
	var mailReads, sheetReads, writes atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/token" {
			w.Write([]byte(`{"access_token":"synthetic","token_type":"Bearer","expires_in":3600}`))
			return
		}
		if r.Method != "GET" {
			writes.Add(1)
			t.Error("unexpected external write")
			http.Error(w, "forbidden", 403)
			return
		}
		switch r.URL.Path {
		case "/gmail/v1/users/me/profile":
			w.Write([]byte(`{"emailAddress":"finance@example.test","historyId":"99"}`))
		case "/oauth2/v3/userinfo":
			w.Write([]byte(`{"email":"finance@example.test","email_verified":true}`))
		case "/gmail/v1/users/me/messages":
			w.Write([]byte(`{"messages":[{"id":"m1","threadId":"t1"}]}`))
		case "/gmail/v1/users/me/history":
			w.Write([]byte(`{"historyId":"101","history":[]}`))
		case "/gmail/v1/users/me/messages/m1":
			mailReads.Add(1)
			body := base64.RawURLEncoding.EncodeToString([]byte("Example Agency for Customer One asks about invoice 0012. USD 100.00. PO: PO-123"))
			json.NewEncoder(w).Encode(map[string]any{"id": "m1", "threadId": "t1", "historyId": "99", "internalDate": "1791288000000", "labelIds": []string{"INBOX"}, "payload": map[string]any{"mimeType": "text/plain", "headers": []map[string]string{{"name": "From", "value": "client@example.test"}, {"name": "Subject", "value": "Invoice 0012"}, {"name": "Message-ID", "value": "<m1@example.test>"}}, "body": map[string]string{"data": body}}})
		case "/v4/spreadsheets/book":
			if r.URL.Query().Get("ranges") == "" {
				w.Write([]byte(`{"spreadsheetId":"book","properties":{"locale":"en_US","timeZone":"America/Denver"},"sheets":[{"properties":{"sheetId":7,"title":"Invoices","gridProperties":{"rowCount":100,"columnCount":10}}}]}`))
				return
			}
			sheetReads.Add(1)
			rows := []any{}
			for _, values := range [][]string{{"ID", "Agency", "Customer", "Invoice", "Currency", "Amount", "Credit", "Due", "PO", "Status"}, {"row-1", "Example Agency", "Customer One", "0012", "USD", "100.10", "0.10", "2026-10-01", "", "open"}} {
				cells := []any{}
				for _, v := range values {
					cells = append(cells, map[string]any{"userEnteredValue": map[string]string{"stringValue": v}, "effectiveValue": map[string]string{"stringValue": v}})
				}
				rows = append(rows, map[string]any{"values": cells})
			}
			json.NewEncoder(w).Encode(map[string]any{"spreadsheetId": "book", "sheets": []any{map[string]any{"properties": map[string]any{"sheetId": 7, "title": "Invoices"}, "data": []any{map[string]any{"rowData": rows}}}}})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	t.Setenv("FINANCE_CLIENT", "synthetic")
	t.Setenv("FINANCE_REFRESH", "synthetic")
	cfg := connectors.Config{Version: 1}
	e.run.Spec.Operations = map[string][]string{}
	for alias, ops := range manifest.Connections {
		c := connectors.Connection{ID: alias, Endpoint: upstream.URL, ExternalProcessing: true, Projects: []string{"default"}, OAuthRefresh: &connectors.OAuthRefresh{TokenURL: upstream.URL + "/token", ClientID: "fixture", ClientSecretEnv: "FINANCE_CLIENT", RefreshTokenEnv: "FINANCE_REFRESH"}}
		if alias == "mail" {
			c.Kind = "email"
			c.Email = &connectors.EmailConfig{Provider: "gmail", Mailbox: "finance@example.test", Workflow: "finance"}
		} else {
			c.Kind = "sheets"
			c.Sheets = &connectors.SheetsConfig{Account: "finance@example.test", Spreadsheet: "book", Areas: []connectors.SheetArea{{SheetID: 7, Range: "'Invoices'!A1:J100"}}}
		}
		for _, op := range ops {
			c.Operations = append(c.Operations, connectors.Operation{Name: op, ReadOnly: true})
		}
		cfg.Connections = append(cfg.Connections, c)
		e.run.Spec.Connections = append(e.run.Spec.Connections, alias)
		e.run.Spec.Operations[alias] = ops
		e.run.Spec.Script.Connections[alias] = alias
		if _, err = app.catalog.SetConnectionAccess(catalog.Bootstrap, catalog.ConnectionGrant{Connection: alias, Users: map[string][]string{e.run.Spec.Actor: ops}}); err != nil {
			t.Fatal(err)
		}
	}
	app.dataConnections, err = connectors.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// First preview exercises real Python and host parsing, but persists neither
	// notebook publication nor the mailbox cursor.
	e.run.Script = nil
	e.run.DryRun = true
	if err = app.runAutomation(t.Context(), e.run, e.save); err != nil {
		t.Fatal("preview", err)
	}
	state, err := app.automations.Queue.JobState(e.run.Spec)
	if err != nil || state.Revision != "" {
		t.Fatal("preview advanced mailbox", err)
	}
	if _, err = store.Read(t.Context(), "reports/finance-inbox.md", ""); err == nil {
		t.Fatal("preview published a note")
	}
	for i := 0; i < 2; i++ {
		e.run.ID = automation.Hash([]any{"finance-live-local", i})
		e.run.Script = nil
		e.run.DryRun = false
		if err = app.runAutomation(t.Context(), e.run, e.save); err != nil {
			t.Fatalf("scheduled run %d: %v", i, err)
		}
	}
	note, err := store.Read(t.Context(), "reports/finance-inbox.md", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"0012", "PO-123", "missingInformation", "recurringInquiries"} {
		if !strings.Contains(string(note.Content), text) {
			t.Fatal("missing expected report", text, string(note.Content))
		}
	}
	if writes.Load() != 0 || sheetReads.Load() != 3 || mailReads.Load() != 4 {
		t.Fatal("duplicate ingestion or missed authoritative refresh", writes.Load(), sheetReads.Load(), mailReads.Load())
	}
	state, err = app.automations.Queue.JobState(e.run.Spec)
	if err != nil || !strings.Contains(string(state.Data), `"cursor":"101"`) {
		t.Fatal("durable progress missing", err, string(state.Data))
	}
}
