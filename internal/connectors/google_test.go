package connectors

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func googleFixture(t *testing.T, kind, policy string, handler http.HandlerFunc) *Manager {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/token" {
			_, _ = w.Write([]byte(`{"access_token":"synthetic-access","expires_in":3600,"token_type":"Bearer"}`))
			return
		}
		if r.Header.Get("Authorization") != "Bearer synthetic-access" {
			t.Error("missing OAuth access token")
		}
		handler(w, r)
	}))
	t.Cleanup(upstream.Close)
	t.Setenv("FMN_GOOGLE_CLIENT", "synthetic-secret")
	t.Setenv("FMN_GOOGLE_REFRESH", "synthetic-refresh")
	raw := `{"version":1,"connections":[{"id":"google","kind":"` + kind + `","endpoint":"` + upstream.URL + `","oauthRefresh":{"tokenURL":"` + upstream.URL + `/token","clientID":"fixture","clientSecretEnv":"FMN_GOOGLE_CLIENT","refreshTokenEnv":"FMN_GOOGLE_REFRESH"},` + policy + `}]}`
	var config Config
	if err := json.Unmarshal([]byte(raw), &config); err != nil {
		t.Fatal(err)
	}
	m, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	return m
}

func TestGmailAccountAndMIMEBoundary(t *testing.T) {
	var wrong atomic.Bool
	m := googleFixture(t, "email", `"email":{"provider":"gmail","mailbox":"finance@example.test","workflow":"finance"},"operations":[{"name":"message","readOnly":true},{"name":"status","readOnly":true}]`, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/gmail/v1/users/me/profile" {
			account := "finance@example.test"
			if wrong.Load() {
				account = "other@example.test"
			}
			json.NewEncoder(w).Encode(map[string]string{"emailAddress": account, "historyId": "9007199254740995"})
			return
		}
		if r.URL.Path != "/gmail/v1/users/me/messages/123" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]any{"id": "123", "threadId": "t1", "historyId": "9007199254740995", "internalDate": "1791288000000", "payload": map[string]any{"mimeType": "multipart/alternative", "headers": []any{map[string]string{"name": "From", "value": "Customer <client@example.test>"}}, "parts": []any{map[string]any{"mimeType": "text/plain", "body": map[string]any{"data": base64.RawURLEncoding.EncodeToString([]byte("Invoice 0012\n> old request"))}}, map[string]any{"mimeType": "text/html", "body": map[string]any{"data": base64.RawURLEncoding.EncodeToString([]byte("<img src='https://tracker.invalid'><p>Invoice 0012</p>"))}}}}})
	})
	result, err := m.Call(context.Background(), "google", "message", json.RawMessage(`{"id":"123"}`), "")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(result.Data)
	if strings.Count(string(raw), "Invoice 0012") != 1 || !strings.Contains(string(raw), "quoted") || !strings.Contains(string(raw), "9007199254740995") {
		t.Fatal(string(raw))
	}
	wrong.Store(true)
	if _, err = m.Call(context.Background(), "google", "message", json.RawMessage(`{"id":"123"}`), ""); err == nil || !strings.Contains(err.Error(), "account mismatch") {
		t.Fatal("account mismatch not rejected", err)
	}
}

func TestSheetsRestrictsRangesAndPreservesCellForms(t *testing.T) {
	var calls atomic.Int32
	m := googleFixture(t, "sheets", `"sheets":{"account":"finance@example.test","spreadsheet":"book","areas":[{"sheetID":7,"range":"'Invoices'!A1:C20"}]},"operations":[{"name":"read","readOnly":true},{"name":"metadata","readOnly":true}]`, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth2/v3/userinfo" {
			w.Write([]byte(`{"email":"finance@example.test","email_verified":true}`))
			return
		}
		calls.Add(1)
		if len(r.URL.Query()["ranges"]) == 0 {
			w.Write([]byte(`{"spreadsheetId":"book","properties":{"locale":"en_US","timeZone":"America/Denver"},"sheets":[{"properties":{"sheetId":7,"title":"Invoices","gridProperties":{"rowCount":100,"columnCount":3}}},{"properties":{"sheetId":8,"title":"Private"}}]}`))
			return
		}
		w.Write([]byte(`{"spreadsheetId":"book","sheets":[{"properties":{"sheetId":7,"title":"Invoices"},"data":[{"startRow":0,"startColumn":0,"rowData":[{"values":[{"userEnteredValue":{"stringValue":"0012"},"effectiveValue":{"stringValue":"0012"},"formattedValue":"0012"},{"userEnteredValue":{"formulaValue":"=1+1"},"effectiveValue":{"numberValue":2},"formattedValue":"2"}]}]}]}]}`))
	})
	v, err := m.Call(context.Background(), "google", "read", json.RawMessage(`{"ranges":["'Invoices'!A1:C2"]}`), "")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(v.Data)
	for _, want := range []string{"0012", "formulaValue", "numberValue", "present", "America/Denver"} {
		if !strings.Contains(string(raw), want) {
			t.Fatal("lost cell semantics", string(raw))
		}
	}
	before := calls.Load()
	for _, args := range []string{`{"ranges":["'Private'!A1"]}`, `{"ranges":["Invoices!A1"]}`, `{"ranges":["'Invoices'!A1:D2"]}`, `{"ranges":["'Invoices'!A1:C2","'Private'!A1"]}`} {
		if _, err := m.Call(context.Background(), "google", "read", json.RawMessage(args), ""); err == nil {
			t.Fatal("unscoped range accepted", args)
		}
	}
	if calls.Load() != before {
		t.Fatal("out of scope request reached spreadsheet API")
	}
}

func TestGmailHistoryPaginationAttachmentAndRevocation(t *testing.T) {
	var revoked atomic.Bool
	m := googleFixture(t, "email", `"email":{"provider":"gmail","mailbox":"finance@example.test","workflow":"finance"},"operations":[{"name":"changes","readOnly":true},{"name":"messages","readOnly":true},{"name":"attachment","readOnly":true}]`, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/gmail/v1/users/me/profile" {
			if revoked.Load() {
				w.WriteHeader(403)
				w.Write([]byte(`{"error":"fixture-secret"}`))
				return
			}
			w.Write([]byte(`{"emailAddress":"finance@example.test"}`))
			return
		}
		switch r.URL.Path {
		case "/gmail/v1/users/me/history":
			w.WriteHeader(404)
			w.Write([]byte(`{}`))
		case "/gmail/v1/users/me/messages":
			if r.URL.Query().Get("pageToken") != "second" {
				t.Error("page token lost")
			}
			w.Write([]byte(`{"messages":[{"id":"42","threadId":"thread"}],"nextPageToken":"third"}`))
		case "/gmail/v1/users/me/messages/42":
			w.Write([]byte(`{"id":"42","threadId":"thread","internalDate":"1791288000000","payload":{"mimeType":"application/pdf","filename":"fixture.pdf","body":{"attachmentId":"a1","size":3}}}`))
		case "/gmail/v1/users/me/messages/42/attachments/a1":
			w.Write([]byte(`{"data":"YWJj","size":3}`))
		default:
			t.Fatal(r.URL.Path)
		}
	})
	call := func(op, args string) map[string]any {
		t.Helper()
		v, err := m.Call(t.Context(), "google", op, json.RawMessage(args), "")
		if err != nil {
			t.Fatal(err)
		}
		return v.Data.(map[string]any)
	}
	if v := call("changes", `{"cursor":"9007199254740995"}`); v["expired"] != true || v["complete"] != false {
		t.Fatal(v)
	}
	if v := call("messages", `{"pageToken":"second","filter":{"flags":["inbox"]}}`); v["hasMore"] != true || v["nextPageToken"] != "third" {
		t.Fatal(v)
	}
	if v := call("attachment", `{"messageId":"42","attachmentId":"a1"}`); v["content"] != "YWJj" || v["sha256"] != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatal("invalid attachment digest", v)
	}
	revoked.Store(true)
	if _, err := m.Call(t.Context(), "google", "messages", json.RawMessage(`{}`), ""); err == nil || !strings.Contains(err.Error(), "reauthorize") || strings.Contains(err.Error(), "fixture-secret") {
		t.Fatal(err)
	}
}

func TestSheetsStableRowGuardAndHonestAcknowledgment(t *testing.T) {
	var writes atomic.Int32
	var conflict, unknown, editAfterRead atomic.Bool
	var overwritten atomic.Bool
	var storedText atomic.Value
	storedText.Store("old")
	cells := []any{map[string]any{"present": true, "userEnteredValue": map[string]any{"stringValue": "0012"}}, map[string]any{"present": true, "userEnteredValue": map[string]any{"stringValue": "10.00"}}, map[string]any{"present": true, "userEnteredValue": map[string]any{"stringValue": "old"}}}
	m := googleFixture(t, "sheets", `"sheets":{"account":"finance@example.test","spreadsheet":"book","areas":[{"sheetID":7,"range":"'Invoices'!A1:C5","write":true}]},"operations":[{"name":"update","readOnly":false}]`, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth2/v3/userinfo" {
			w.Write([]byte(`{"email":"finance@example.test","email_verified":true}`))
			return
		}
		if r.Method == "PUT" {
			writes.Add(1)
			// A collaborator's edit after our read is not protected by Google's
			// values API. Model that real race rather than implying a CAS.
			if editAfterRead.Load() && storedText.Load() == "collaborator edit" {
				overwritten.Store(true)
			}
			if !strings.Contains(r.URL.Path, "A3:C3") || r.URL.Query().Get("valueInputOption") != "RAW" {
				t.Error("row number was persisted instead of resolved", r.URL.String())
			}
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			storedText.Store(body["values"].([]any)[0].([]any)[2])
			affected := "Invoices!A3:C3"
			if unknown.Load() {
				affected = "Other!A3:C3"
			}
			json.NewEncoder(w).Encode(map[string]any{"spreadsheetId": "book", "updatedRange": affected, "updatedCells": 3, "updatedData": map[string]any{"values": body["values"]}})
			return
		}
		if r.URL.Query().Get("ranges") == "" {
			w.Write([]byte(`{"spreadsheetId":"book","sheets":[{"properties":{"sheetId":7,"title":"Invoices","gridProperties":{"rowCount":5,"columnCount":3}}}]}`))
			return
		}
		current := []any{}
		for _, c := range cells {
			v := c.(map[string]any)
			current = append(current, map[string]any{"userEnteredValue": v["userEnteredValue"]})
		}
		if conflict.Load() {
			current[1] = map[string]any{"userEnteredValue": map[string]any{"stringValue": "changed"}}
		}
		json.NewEncoder(w).Encode(map[string]any{"spreadsheetId": "book", "sheets": []any{map[string]any{"properties": map[string]any{"sheetId": 7, "title": "Invoices"}, "data": []any{map[string]any{"startRow": 2, "rowData": []any{map[string]any{"values": current}}}}}}})
		if editAfterRead.Load() {
			storedText.Store("collaborator edit")
		}
	})
	args := map[string]any{"actionID": "one", "range": "'Invoices'!A1:C5", "identityColumn": 1, "identity": "0012", "values": [][]string{{"0012", "10.00", "=untrusted()"}}, "expected": []any{cells}}
	raw, _ := json.Marshal(args)
	v, err := m.Call(WithSubmission(t.Context()), "google", "update", raw, "")
	if err != nil {
		t.Fatal(err)
	}
	data := v.Data.(map[string]any)
	if data["atomic"] != false || data["accepted"] != true || data["updatedRange"] != "'Invoices'!A3:C3" {
		t.Fatal(data)
	}
	conflict.Store(true)
	if _, err = m.Call(WithSubmission(t.Context()), "google", "update", raw, ""); !WriteRejected(err) {
		t.Fatal("concurrent edit not rejected", err)
	}
	if writes.Load() != 1 {
		t.Fatal("conflicting row was written")
	}
	conflict.Store(false)
	unknown.Store(true)
	if _, err = m.Call(WithSubmission(t.Context()), "google", "update", raw, ""); err == nil || WriteRejected(err) {
		t.Fatal("unexpected acknowledgment was treated as safe", err)
	}
	unknown.Store(false)
	editAfterRead.Store(true)
	v, err = m.Call(WithSubmission(t.Context()), "google", "update", raw, "")
	if err != nil || !overwritten.Load() || storedText.Load() != "=untrusted()" || v.Data.(map[string]any)["atomic"] != false {
		t.Fatal("read-before-write incorrectly claimed atomic protection", err)
	}
}

func TestGoogleRechecksAuthorityBeforeSubmission(t *testing.T) {
	var revoked atomic.Bool
	var writes atomic.Int32
	m := googleFixture(t, "email", `"email":{"provider":"gmail","mailbox":"finance@example.test","workflow":"finance","recipients":["client@example.test"]},"operations":[{"name":"draft-create","readOnly":false}]`, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			w.Write([]byte(`{"emailAddress":"finance@example.test"}`))
			revoked.Store(true)
			return
		}
		writes.Add(1)
	})
	ctx := WithAuthority(WithSubmission(t.Context()), func(context.Context) error {
		if revoked.Load() {
			return errors.New("permission revoked")
		}
		return nil
	})
	_, err := m.Call(ctx, "google", "draft-create", json.RawMessage(`{"actionID":"one","to":["client@example.test"],"subject":"Invoice","text":"Proposal"}`), "")
	if !WriteRejected(err) || writes.Load() != 0 {
		t.Fatal("permission change between identity read and write was ignored", err)
	}
}

func TestGmailRejectsChangedThreadAndHeaderInjection(t *testing.T) {
	var newReply atomic.Bool
	var sends atomic.Int32
	original := map[string]any{"id": "m1", "threadId": "t1", "internalDate": "1791288000000", "payload": map[string]any{"mimeType": "text/plain", "headers": []map[string]string{{"name": "From", "value": "client@example.test"}, {"name": "Subject", "value": "Invoice 0012"}, {"name": "Message-ID", "value": "<m1@example.test>"}}, "body": map[string]string{"data": "SGVsbG8"}}}
	m := googleFixture(t, "email", `"email":{"provider":"gmail","mailbox":"finance@example.test","workflow":"finance","recipients":["client@example.test"]},"operations":[{"name":"send","readOnly":false}]`, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/gmail/v1/users/me/profile":
			w.Write([]byte(`{"emailAddress":"finance@example.test"}`))
		case "/gmail/v1/users/me/messages/m1":
			json.NewEncoder(w).Encode(original)
		case "/gmail/v1/users/me/threads/t1":
			messages := []any{original}
			if newReply.Load() {
				later := map[string]any{}
				for k, v := range original {
					later[k] = v
				}
				later["id"] = "m2"
				later["internalDate"] = "1791288001000"
				messages = append(messages, later)
			}
			json.NewEncoder(w).Encode(map[string]any{"id": "t1", "historyId": "H1", "messages": messages})
		case "/gmail/v1/users/me/messages/send":
			sends.Add(1)
			var value map[string]string
			json.NewDecoder(r.Body).Decode(&value)
			raw, err := decodeURL64(value["raw"])
			if err != nil || !strings.Contains(string(raw), "In-Reply-To: <m1@example.test>") || value["threadId"] != "t1" {
				t.Error("threading headers absent")
			}
			w.Write([]byte(`{"id":"s1","threadId":"t1"}`))
		default:
			t.Error(r.URL.Path)
		}
	})
	args := map[string]any{"actionID": "one", "to": []string{"client@example.test"}, "subject": "Re: Invoice 0012", "text": "Proposed reply", "replyToMessage": "m1", "threadRevision": "H1"}
	call := func() error {
		raw, _ := json.Marshal(args)
		_, err := m.Call(WithSubmission(t.Context()), "google", "send", raw, "")
		return err
	}
	if err := call(); err != nil {
		t.Fatal(err)
	}
	newReply.Store(true)
	if err := call(); !WriteRejected(err) {
		t.Fatal("new reply ignored", err)
	}
	newReply.Store(false)
	args["subject"] = "Re: Invoice 0012\r\nBcc: other@example.test"
	if err := call(); !WriteRejected(err) {
		t.Fatal("header injection", err)
	}
	if sends.Load() != 1 {
		t.Fatal("rejected sends reached provider", sends.Load())
	}
}

func TestGmailMIMECharsetAndUnsupportedContent(t *testing.T) {
	latin := base64.RawURLEncoding.EncodeToString([]byte{'c', 'a', 'f', 0xe9})
	raw := map[string]any{"id": "m", "threadId": "t", "internalDate": "1791288000000", "payload": map[string]any{"mimeType": "multipart/mixed", "headers": []any{map[string]string{"name": "Subject", "value": "=?ISO-8859-1?Q?caf=E9?="}}, "parts": []any{map[string]any{"mimeType": "text/plain", "headers": []any{map[string]string{"name": "Content-Type", "value": "text/plain; charset=iso-8859-1"}}, "body": map[string]string{"data": latin}}, map[string]any{"mimeType": "application/pkcs7-mime", "body": map[string]string{"data": "YWJj"}}}}}
	view, err := parseMail(raw, "finance@example.test")
	if err != nil {
		t.Fatal(err)
	}
	if view.Subject != "café" || len(view.Text) != 1 || view.Text[0].Text != "café" || view.Complete || len(view.Unsupported) != 1 {
		t.Fatal(view)
	}
}

func TestGmailDraftOwnershipAndConflict(t *testing.T) {
	var unrelated, changed atomic.Bool
	var writes atomic.Int32
	m := googleFixture(t, "email", `"email":{"provider":"gmail","mailbox":"finance@example.test","workflow":"finance","recipients":["client@example.test"]},"operations":[{"name":"draft","readOnly":true},{"name":"draft-update","readOnly":false}]`, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/gmail/v1/users/me/profile" {
			w.Write([]byte(`{"emailAddress":"finance@example.test"}`))
			return
		}
		if r.Method == "PUT" {
			writes.Add(1)
			w.Write([]byte(`{"id":"d1","message":{"id":"m2","threadId":"t1"}}`))
			return
		}
		workflow := "finance"
		subject := "Invoice"
		if unrelated.Load() {
			workflow = "another-workflow"
		}
		if changed.Load() {
			subject = "Human edited draft"
		}
		json.NewEncoder(w).Encode(map[string]any{"id": "d1", "message": map[string]any{"id": "m1", "threadId": "t1", "internalDate": "1791288000000", "payload": map[string]any{"mimeType": "text/plain", "headers": []map[string]string{{"name": "X-ForgetMeNote-Workflow", "value": workflow}, {"name": "Subject", "value": subject}}, "body": map[string]string{"data": "SGVsbG8"}}}})
	})
	read, err := m.Call(t.Context(), "google", "draft", json.RawMessage(`{"id":"d1"}`), "")
	if err != nil {
		t.Fatal(err)
	}
	args := map[string]any{"actionID": "one", "id": "d1", "expectedFingerprint": read.Data.(map[string]any)["fingerprint"], "to": []string{"client@example.test"}, "subject": "Invoice", "text": "Proposed update"}
	raw, _ := json.Marshal(args)
	call := func() error {
		_, err := m.Call(WithSubmission(t.Context()), "google", "draft-update", raw, "")
		return err
	}
	if err = call(); err != nil {
		t.Fatal(err)
	}
	changed.Store(true)
	if err = call(); !WriteRejected(err) {
		t.Fatal("human edit was overwritten", err)
	}
	changed.Store(false)
	unrelated.Store(true)
	if err = call(); !WriteRejected(err) {
		t.Fatal("unrelated draft was overwritten", err)
	}
	if _, err = m.Call(WithSubmission(t.Context()), "google", "send", raw, ""); err == nil {
		t.Fatal("draft grant authorized send")
	}
	if writes.Load() != 1 {
		t.Fatal("unexpected draft writes", writes.Load())
	}
}

func TestSheetsAppendStaysInOwnedAreaAndFindsExistingIdentity(t *testing.T) {
	var writes atomic.Int32
	var duplicate atomic.Bool
	m := googleFixture(t, "sheets", `"sheets":{"account":"finance@example.test","spreadsheet":"book","areas":[{"sheetID":7,"range":"'Output'!A:C","write":true,"append":true,"automationOwned":true}]},"operations":[{"name":"append","readOnly":false}]`, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth2/v3/userinfo" {
			w.Write([]byte(`{"email":"finance@example.test","email_verified":true}`))
			return
		}
		if r.Method == "POST" {
			writes.Add(1)
			if r.URL.Query().Get("valueInputOption") != "RAW" || !strings.HasSuffix(r.URL.Path, "'Output'!A:C:append") {
				t.Error("unexpected append target", r.URL.String())
			}
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			json.NewEncoder(w).Encode(map[string]any{"spreadsheetId": "book", "updates": map[string]any{"updatedRange": "Output!A2:C2", "updatedCells": 3, "updatedData": map[string]any{"values": body["values"]}}})
			return
		}
		if r.URL.Query().Get("ranges") == "" {
			w.Write([]byte(`{"spreadsheetId":"book","sheets":[{"properties":{"sheetId":7,"title":"Output","gridProperties":{"rowCount":10,"columnCount":3}}}]}`))
			return
		}
		var data []any
		if duplicate.Load() {
			data = []any{map[string]any{"startRow": 1, "rowData": []any{map[string]any{"values": []any{map[string]any{"userEnteredValue": map[string]string{"stringValue": "action1"}}}}}}}
		}
		json.NewEncoder(w).Encode(map[string]any{"spreadsheetId": "book", "sheets": []any{map[string]any{"properties": map[string]any{"sheetId": 7, "title": "Output"}, "data": data}}})
	})
	args := map[string]any{"actionID": "action1", "range": "'Output'!A:C", "values": [][]string{{"action1", "0012", "=untrusted()"}}, "identityColumn": 1, "identity": "action1", "expected": []any{}}
	call := func() (Result, error) {
		raw, _ := json.Marshal(args)
		return m.Call(WithSubmission(t.Context()), "google", "append", raw, "")
	}
	v, err := call()
	if err != nil {
		t.Fatal(err)
	}
	if v.Data.(map[string]any)["updatedRange"] != "'Output'!A2:C2" {
		t.Fatal(v.Data)
	}
	duplicate.Store(true)
	if _, err = call(); !WriteRejected(err) {
		t.Fatal("duplicate append accepted", err)
	}
	duplicate.Store(false)
	args["range"] = "'Output'!A1:C10"
	if _, err = call(); !WriteRejected(err) {
		t.Fatal("bounded append could escape its rows", err)
	}
	if writes.Load() != 1 {
		t.Fatal("unexpected provider writes", writes.Load())
	}
}
