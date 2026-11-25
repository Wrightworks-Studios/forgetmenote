package web

import (
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/connectors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDataQueuedWriteRechecksRevocation(t *testing.T) {
	app, _ := fixture(t)
	var requests atomic.Int32
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			close(entered)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"saved":true}`))
	}))
	defer upstream.Close()
	defer unblock()
	manager, err := connectors.New(connectors.Config{Version: 1, Connections: []connectors.Connection{{ID: "metrics", Kind: "http", Endpoint: upstream.URL, Concurrent: 1, Operations: []connectors.Operation{{Name: "write", Method: "POST", Path: "/", ReadOnly: false}}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	app.dataConnections = manager
	if err = app.catalog.SetUser(catalog.Bootstrap, catalog.User{ID: "alice"}); err != nil {
		t.Fatal(err)
	}
	if err = app.catalog.Member(catalog.Bootstrap, "default", "alice", "read"); err != nil {
		t.Fatal(err)
	}
	grant, err := app.catalog.SetConnectionAccess(catalog.Bootstrap, catalog.ConnectionGrant{Connection: "metrics", Users: map[string][]string{"alice": {"write"}}})
	if err != nil {
		t.Fatal(err)
	}
	cred, err := issueCredential(app.catalog, app.oauth, "alice", CredentialSpec{Name: "data writer", Projects: []string{"default"}, Connections: []string{"metrics"}, Scopes: []string{"data:read", "data:write"}, Audiences: []string{"api"}, Days: 1})
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan *httptest.ResponseRecorder, 2)
	invoke := func() {
		result <- apiCall(app, "POST", "/api/v1/actions/call_data_operation", `{"connection":"metrics","operation":"write","arguments":{}}`, cred.Token)
	}
	go invoke()
	select {
	case <-entered:
	case out := <-result:
		t.Fatalf("first write not admitted: %d %s", out.Code, out.Body)
	case <-time.After(5 * time.Second):
		t.Fatal("first write timeout")
	}
	go invoke()
	deadline := time.Now().Add(5 * time.Second)
	for {
		organization, e := app.catalog.Organization(catalog.Bootstrap)
		if e != nil {
			t.Fatal(e)
		}
		count := 0
		for _, a := range organization.Activity {
			if a.Action == "data.call" && a.Target == "metrics" && a.Outcome == "pending" {
				count++
			}
		}
		if count == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("second request never reached pending intent")
		}
		time.Sleep(time.Millisecond)
	}
	grant.Users = map[string][]string{}
	if _, err = app.catalog.SetConnectionAccess(catalog.Bootstrap, grant); err != nil {
		t.Fatal(err)
	}
	unblock()
	for range 2 {
		select {
		case out := <-result:
			if out.Code != 403 {
				t.Fatalf("revoked result returned: %d %s", out.Code, out.Body)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("result timeout")
		}
	}
	if requests.Load() != 1 {
		t.Fatalf("revoked queued write reached upstream: requests=%d want 1", requests.Load())
	}
}
