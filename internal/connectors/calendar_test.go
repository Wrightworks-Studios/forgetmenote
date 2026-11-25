package connectors

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

func TestCalendarListsEventsOnItsOneCalendar(t *testing.T) {
	var calls atomic.Int32
	var account atomic.Value
	account.Store("assistant@example.test")
	m := googleFixture(t, "calendar", `"calendar":{"account":"assistant@example.test","calendar":"c_holidays@group.calendar.google.com"},"operations":[{"name":"events","readOnly":true},{"name":"status","readOnly":true}]`, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth2/v3/userinfo" {
			json.NewEncoder(w).Encode(map[string]any{"email": account.Load(), "email_verified": true})
			return
		}
		calls.Add(1)
		q := r.URL.Query()
		if r.URL.Path != "/calendar/v3/calendars/c_holidays@group.calendar.google.com/events" || q.Get("timeMin") != "2026-11-01T00:00:00Z" || q.Get("timeMax") != "2027-01-01T00:00:00Z" || q.Get("singleEvents") != "true" || strings.Contains(q.Get("fields"), "description") || strings.Contains(q.Get("fields"), "attendees") {
			t.Errorf("unexpected request %s", r.URL)
		}
		if q.Get("pageToken") == "" {
			w.Write([]byte(`{"timeZone":"America/Denver","nextPageToken":"page-2","items":[
				{"id":"h1","status":"confirmed","summary":"Holiday - Christmas Eve","start":{"date":"2026-12-24"},"end":{"date":"2026-12-25"}},
				{"id":"m1","status":"confirmed","summary":"Quarterly planning","start":{"dateTime":"2026-11-18T10:00:00-07:00"},"end":{"dateTime":"2026-11-18T11:00:00-07:00"}},
				{"id":"x1","status":"cancelled"}]}`))
			return
		}
		if q.Get("pageToken") != "page-2" {
			t.Errorf("page token = %q", q.Get("pageToken"))
		}
		w.Write([]byte(`{"timeZone":"America/Denver","items":[{"id":"h2","status":"confirmed","start":{"date":"2026-12-31"},"end":{"date":"2027-01-02"}}]}`))
	})
	call := func(args string) (map[string]any, error) {
		t.Helper()
		result, err := m.Call(context.Background(), "google", "events", json.RawMessage(args), "")
		if err != nil {
			return nil, err
		}
		raw, _ := json.Marshal(result.Data)
		var out map[string]any
		json.Unmarshal(raw, &out)
		return out, nil
	}
	first, err := call(`{"from":"2026-11-01","to":"2027-01-01"}`)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(first)
	want := `{"calendar":"c_holidays@group.calendar.google.com","events":[{"allDay":true,"end":"2026-12-25","id":"h1","start":"2026-12-24","title":"Holiday - Christmas Eve"},{"allDay":false,"end":"2026-11-18T11:00:00-07:00","id":"m1","start":"2026-11-18T10:00:00-07:00","title":"Quarterly planning"}],"hasMore":true,"nextPageToken":"page-2","timeZone":"America/Denver"}`
	if string(raw) != want {
		t.Fatalf("first page:\n%s\nwant:\n%s", raw, want)
	}
	second, err := call(`{"from":"2026-11-01","to":"2027-01-01","pageToken":"page-2"}`)
	if err != nil {
		t.Fatal(err)
	}
	if events := second["events"].([]any); len(events) != 1 || second["hasMore"] != false || events[0].(map[string]any)["end"] != "2027-01-02" {
		t.Fatalf("second page: %v", second)
	}

	before := calls.Load()
	for _, args := range []string{`{"from":"2026-11-01"}`, `{"from":"2026-11-01","to":"2026-11-01"}`, `{"from":"2027-01-01","to":"2026-11-01"}`, `{"from":"2026-01-01","to":"2027-01-03"}`, `{"from":"Nov 1","to":"2027-01-01"}`, `{"from":"2026-11-01","to":"2027-01-01","calendar":"primary"}`} {
		if _, err := call(args); err == nil {
			t.Fatal("an out-of-policy range was read", args)
		}
	}
	if calls.Load() != before {
		t.Fatal("a refused request reached the Calendar API")
	}
	account.Store("someone@example.test")
	if _, err := call(`{"from":"2026-11-01","to":"2027-01-01"}`); err == nil || !strings.Contains(err.Error(), "account mismatch") {
		t.Fatal("another account's grant read the calendar", err)
	}
}

func TestCalendarConnectionsOnlyRead(t *testing.T) {
	for _, policy := range []string{
		`"calendar":{"account":"assistant@example.test","calendar":"c_holidays@group.calendar.google.com"},"operations":[{"name":"events","readOnly":false}]`,
		`"calendar":{"account":"assistant@example.test","calendar":"c_holidays@group.calendar.google.com"},"operations":[{"name":"create","readOnly":false}]`,
		`"calendar":{"account":"assistant@example.test","calendar":"c holidays/../x"},"operations":[{"name":"events","readOnly":true}]`,
		`"calendar":{"account":"assistant@example.test"},"operations":[{"name":"events","readOnly":true}]`,
		`"sheets":{"account":"assistant@example.test","spreadsheet":"book","areas":[{"sheetID":7,"range":"'Invoices'!A1:C20"}]},"operations":[{"name":"events","readOnly":true}]`,
	} {
		raw := `{"version":1,"connections":[{"id":"google","kind":"calendar","endpoint":"http://127.0.0.1:1","oauthRefresh":{"tokenURL":"http://127.0.0.1:1/token","clientID":"fixture","clientSecretEnv":"FMN_GOOGLE_CLIENT","refreshTokenEnv":"FMN_GOOGLE_REFRESH"},` + policy + `}]}`
		var config Config
		if err := json.Unmarshal([]byte(raw), &config); err != nil {
			t.Fatal(err)
		}
		if m, err := New(config); err == nil {
			m.Close()
			t.Fatal("accepted", policy)
		}
	}
}

func TestGoogleExampleConfigurationLoads(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/integrations/google/connections.example.json")
	if err != nil {
		t.Fatal(err)
	}
	var config Config
	if err = json.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	m, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	for _, id := range []string{"company-holidays", "us-holidays"} {
		if info, ok := m.Info(id); !ok || info.Kind != "calendar" {
			t.Fatalf("the example calendar connection %s = %+v", id, info)
		}
	}
}
