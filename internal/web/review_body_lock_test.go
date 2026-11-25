package web

import (
	"forgetmenote/internal/catalog"
	"io"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type slowReviewJSON struct {
	entered, release chan struct{}
	once             sync.Once
}

func (b *slowReviewJSON) Read([]byte) (int, error) {
	b.once.Do(func() { close(b.entered) })
	<-b.release
	return 0, io.EOF
}
func (b *slowReviewJSON) Close() error { return nil }
func TestSlowReviewBodyDoesNotBlockAnotherNotebook(t *testing.T) {
	for _, path := range []string{"/api/jev/settings", "/p/default/api/jev/settings", "/p/default/api/jev/organization", "/p/default/api/jev/jobs/{job}/organization/dismiss"} {
		t.Run(path, func(t *testing.T) { testSlowReviewBody(t, path) })
	}
}
func testSlowReviewBody(t *testing.T, requestPath string) {
	app, _ := fixture(t)
	if err := app.ConfigureProxy(proxyTestSecret, []string{"127.0.0.1/32"}); err != nil {
		t.Fatal(err)
	}
	if err := app.catalog.Create(catalog.Bootstrap, "second-review", "Second review"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"default", "second-review"} {
		if err := app.catalog.Member(catalog.Bootstrap, id, "alice", "owner"); err != nil {
			t.Fatal(err)
		}
	}
	path := "/p/second-review/api/jev/organization"
	if w := proxyCall(app, "alice", "GET", path, ""); w.Code != 200 {
		t.Fatal("baseline GET", w.Code, w.Body.String())
	}
	if strings.Contains(requestPath, "{job}") {
		id := digest("slow-client-control")
		job := jevJob{Version: 2, ID: id, Owner: "alice", AccountID: "account", Project: "default", RequestHash: "request", Inventory: "inventory", Model: "model", State: "paused", Notes: []jevJobNote{}, Work: []jevJobWork{}, Organization: &organizationRun{}}
		if err := app.jevState.write(jobName(id), job); err != nil {
			t.Fatal(err)
		}
		requestPath = strings.Replace(requestPath, "{job}", id, 1)
	}
	body := &slowReviewJSON{entered: make(chan struct{}), release: make(chan struct{})}
	r := httptest.NewRequest("POST", app.origin+requestPath, body)
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("Remote-User", "alice")
	r.Header.Set("X-ForgetMeNote-Proxy-Secret", proxyTestSecret)
	r.Header.Set("Origin", app.origin)
	r.Header.Set("Content-Type", "application/json")
	firstDone := make(chan struct{})
	go func() { app.ServeHTTP(httptest.NewRecorder(), r); close(firstDone) }()
	select {
	case <-body.entered:
	case <-time.After(5 * time.Second):
		close(body.release)
		<-firstDone
		t.Fatal("request never reached body decode")
	}
	otherDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { otherDone <- proxyCall(app, "alice", "GET", path, "") }()
	blocked := false
	var response *httptest.ResponseRecorder
	select {
	case response = <-otherDone:
	case <-time.After(500 * time.Millisecond):
		blocked = true
	}
	close(body.release)
	<-firstDone
	if response == nil {
		response = <-otherDone
	}
	if response.Code != 200 {
		t.Fatal("other notebook GET", response.Code, response.Body.String())
	}
	if blocked {
		t.Fatal("an unfinished preferences request held global organizationMu and blocked another notebook's settings read")
	}
}
