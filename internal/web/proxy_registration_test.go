package web

import (
	"encoding/json"
	"forgetmenote/internal/pg/pgtest"
	"net/http/httptest"
	"strings"
	"testing"

	"forgetmenote/internal/catalog"
)

func TestVerifiedProxyLoginRegistersDirectoryWithoutGrants(t *testing.T) {
	app, _ := fixture(t)
	if err := app.ConfigureProxy(proxyTestSecret, []string{"127.0.0.1/32"}); err != nil {
		t.Fatal(err)
	}
	w := proxyCall(app, "chayla", "GET", "/api/me", "")
	if w.Code != 200 {
		t.Fatalf("verified login: %d %s", w.Code, w.Body.String())
	}
	r := httptest.NewRequest("GET", app.origin+"/api/people?project=default&q=chayla", nil)
	r.Header.Set("Authorization", "Bearer "+testToken)
	w = httptest.NewRecorder()
	app.ServeHTTP(w, r)
	var people []catalog.Person
	if err := json.Unmarshal(w.Body.Bytes(), &people); err != nil || w.Code != 200 || len(people) != 1 || people[0].ID != "chayla" {
		t.Fatalf("verified user missing from member picker: %d %s (%v)", w.Code, w.Body.String(), err)
	}
	if app.catalog.IsAdmin("chayla") || app.catalog.CanCreateProjects("chayla") || len(app.catalog.List("chayla")) != 0 {
		t.Fatal("registration granted authority or notebook access")
	}
	if w := proxyCall(app, "chayla", "GET", "/p/default/api/notes", ""); w.Code != 403 {
		t.Fatalf("private notebook exposed: %d", w.Code)
	}
}

func TestUnverifiedProxyRequestsDoNotRegisterAccounts(t *testing.T) {
	for _, row := range []struct{ name, peer, secret, user, origin string }{
		{"wrong secret", "127.0.0.1:10", "wrong", "forged", ""},
		{"untrusted peer", "192.0.2.1:10", proxyTestSecret, "forged", ""},
		{"missing secret", "127.0.0.1:10", "", "forged", ""},
		{"invalid subject", "127.0.0.1:10", proxyTestSecret, "../forged", ""},
		{"cross origin", "127.0.0.1:10", proxyTestSecret, "forged", "https://invalid.example"},
	} {
		t.Run(row.name, func(t *testing.T) {
			app, _ := fixture(t)
			if err := app.ConfigureProxy(proxyTestSecret, []string{"127.0.0.1/32"}); err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest("GET", app.origin+"/api/me", nil)
			r.RemoteAddr = row.peer
			r.Header.Set("Remote-User", row.user)
			r.Header.Set("X-ForgetMeNote-Proxy-Secret", row.secret)
			r.Header.Set("Origin", row.origin)
			w := httptest.NewRecorder()
			app.ServeHTTP(w, r)
			if w.Code != 401 && w.Code != 403 {
				t.Fatalf("unverified request accepted: %d", w.Code)
			}
			db := pgtest.Dir(t, app.data)
			_, raw, err := db.ReadDocument(t.Context(), db.Pool, "catalog", "state", -1)
			if err != nil {
				t.Fatal(err)
			}
			var persisted struct {
				Data struct{ Users, Accounts map[string]json.RawMessage }
			}
			if err := json.Unmarshal(raw, &persisted); err != nil {
				t.Fatal(err)
			}
			if _, ok := persisted.Data.Users[row.user]; ok {
				t.Fatal("unverified user registered")
			}
			if _, ok := persisted.Data.Accounts[row.user]; ok {
				t.Fatal("unverified account created")
			}
		})
	}
}

func TestProxyRegistrationFailureIsNotAuthenticatedSuccess(t *testing.T) {
	app, _ := fixture(t)
	if err := app.ConfigureProxy(proxyTestSecret, []string{"127.0.0.1/32"}); err != nil {
		t.Fatal(err)
	}
	restore := pgtest.FailWrites(t, pgtest.Dir(t, app.data), "catalog")
	// The catalog is unavailable: the response says so without the
	// database's error, which only the request log carries.
	if w := proxyCall(app, "newcomer", "GET", "/api/me", ""); w.Code != 503 || !strings.Contains(w.Body.String(), "catalog authority unavailable") || strings.Contains(w.Body.String(), "injected") {
		t.Fatalf("failed registration should fail closed: %d %s", w.Code, w.Body.String())
	}
	people, err := app.catalog.FindPeople(catalog.Bootstrap, "default", "newcomer")
	if err != nil || len(people) != 0 {
		t.Fatalf("failed registration leaked user: %v %v", people, err)
	}
	restore()
	if w := proxyCall(app, "newcomer", "GET", "/api/me", ""); w.Code != 200 {
		t.Fatalf("registration retry failed: %d %s", w.Code, w.Body.String())
	}
	people, err = app.catalog.FindPeople(catalog.Bootstrap, "default", "newcomer")
	if err != nil || len(people) != 1 {
		t.Fatalf("retry not registered: %v %v", people, err)
	}
}
