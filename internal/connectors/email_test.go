package connectors

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

func TestEmailContractNormalizesGmail(t *testing.T) {
	m := googleFixture(t, "email", `"email":{"provider":"gmail","mailbox":"finance@example.test","workflow":"finance"},"operations":[{"name":"status","readOnly":true},{"name":"messages","readOnly":true},{"name":"changes","readOnly":true},{"name":"message","readOnly":true}]`, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/gmail/v1/users/me/profile":
			w.Write([]byte(`{"emailAddress":"finance@example.test","historyId":"9007199254740995"}`))
		case "/gmail/v1/users/me/history":
			if r.URL.Query().Get("startHistoryId") != "9007199254740995" {
				t.Error("opaque cursor changed")
			}
			w.Write([]byte(`{"historyId":"9007199254740999","history":[{"id":"9007199254740996","messagesAdded":[{"message":{"id":"m1"}}],"labelsRemoved":[{"message":{"id":"m2"}}]},{"id":"9007199254740998","messagesDeleted":[{"message":{"id":"m3"}}]}]}`))
		case "/gmail/v1/users/me/messages":
			q := r.URL.Query().Get("q")
			for _, want := range []string{"after:1790812800", "from:client@example.test", "in:inbox", "-in:spam"} {
				if !strings.Contains(q, want) {
					t.Errorf("filter lost: %s in %s", want, q)
				}
			}
			w.Write([]byte(`{"messages":[{"id":"m1","threadId":"t1"}],"nextPageToken":"page2"}`))
		case "/gmail/v1/users/me/messages/m1":
			w.Write([]byte(`{"id":"m1","threadId":"t1","historyId":"9007199254740996","labelIds":["INBOX","UNREAD","custom-label"],"internalDate":"1791288000000","payload":{"mimeType":"text/plain","body":{"data":"aGVsbG8"}}}`))
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})
	call := func(op, args string) map[string]any {
		t.Helper()
		result, err := m.Call(t.Context(), "google", op, json.RawMessage(args), "")
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(result.Data)
		var v map[string]any
		json.Unmarshal(raw, &v)
		return v
	}
	status := call("status", `{}`)
	if status["cursor"] != "9007199254740995" || object(status["capabilities"])["incrementalChanges"] != true || status["profile"] != nil {
		t.Fatal(status)
	}
	changes := call("changes", `{"cursor":"9007199254740995"}`)
	if changes["cursor"] != "9007199254740999" || len(array(changes["changes"])) != 3 || changes["history"] != nil {
		t.Fatal(changes)
	}
	last := object(array(changes["changes"])[2])
	if last["id"] != "m3" || last["kind"] != "deleted" {
		t.Fatal(last)
	}
	list := call("messages", `{"filter":{"after":"2026-10-01T00:00:00Z","from":"client@example.test","flags":["inbox"],"excludeFlags":["spam"]}}`)
	if list["nextPageToken"] != "page2" || list["hasMore"] != true {
		t.Fatal(list)
	}
	message := call("message", `{"id":"m1"}`)
	if message["revision"] != "9007199254740996" || message["historyId"] != nil || message["labels"] != nil || len(array(message["flags"])) != 2 {
		t.Fatal(message)
	}
	if !m.ManagedWrites("google") {
		t.Fatal("email escaped durable submission ownership")
	}
}

func TestEmailRejectsUnsupportedFiltersWithoutSearching(t *testing.T) {
	var searches atomic.Int32
	m := googleFixture(t, "email", `"email":{"provider":"gmail","mailbox":"finance@example.test","workflow":"finance"},"operations":[{"name":"messages","readOnly":true}]`, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/gmail/v1/users/me/profile" {
			w.Write([]byte(`{"emailAddress":"finance@example.test","historyId":"1"}`))
			return
		}
		searches.Add(1)
		w.Write([]byte(`{"messages":[]}`))
	})
	for _, args := range []string{
		`{"filter":{"flags":["invented"]}}`, `{"filter":{"after":"yesterday"}}`,
		`{"filter":{"subject":"invoice\" OR in:anywhere"}}`, `{"filter":{"from":"a@example.test OR in:anywhere"}}`,
		`{"providerQuery":{"provider":"other","query":"all"}}`, `{"query":"in:anywhere"}`,
	} {
		if _, err := m.Call(t.Context(), "google", "messages", json.RawMessage(args), ""); err == nil {
			t.Errorf("accepted unsupported filter: %s", args)
		}
	}
	if searches.Load() != 0 {
		t.Fatal("invalid filter reached message search")
	}
}

func TestEmailProviderMustBeExplicitAndSupported(t *testing.T) {
	for _, provider := range []string{"", "unimplemented"} {
		_, err := New(Config{Version: 1, Connections: []Connection{{ID: "mail", Kind: "email", Email: &EmailConfig{Provider: provider, Mailbox: "finance@example.test", Workflow: "finance"}}}})
		if err == nil || !strings.Contains(err.Error(), "unsupported email provider") {
			t.Fatal(provider, err)
		}
	}
}

func TestEmailMalformedPageCannotAdvanceCursor(t *testing.T) {
	var response string
	m := googleFixture(t, "email", `"email":{"provider":"gmail","mailbox":"finance@example.test","workflow":"finance"},"operations":[{"name":"messages","readOnly":true},{"name":"changes","readOnly":true}]`, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/profile") {
			w.Write([]byte(`{"emailAddress":"finance@example.test","historyId":"1"}`))
			return
		}
		w.Write([]byte(response))
	})
	for _, c := range []struct{ operation, args, body string }{
		{"messages", `{}`, `{"messages":true}`},
		{"messages", `{}`, `{"messages":[],"nextPageToken":42}`},
		{"changes", `{"cursor":"1"}`, `{"history":{},"historyId":"2"}`},
		{"changes", `{"cursor":"1"}`, `{"history":[{"messagesAdded":{}}],"historyId":"2"}`},
	} {
		response = c.body
		if _, err := m.Call(t.Context(), "google", c.operation, json.RawMessage(c.args), ""); err == nil {
			t.Error("malformed page accepted as complete", c.body)
		}
	}
}
