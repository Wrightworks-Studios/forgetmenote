package web

import (
	"context"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"io"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type blockedRequestBody struct {
	entered, release chan struct{}
	started          atomic.Bool
}

func (b *blockedRequestBody) Read([]byte) (int, error) {
	if b.started.CompareAndSwap(false, true) {
		close(b.entered)
	}
	<-b.release
	return 0, io.EOF
}

type observedRequestBody struct {
	io.Reader
	read atomic.Bool
}

func (b *observedRequestBody) Read(p []byte) (int, error) {
	b.read.Store(true)
	return b.Reader.Read(p)
}

func TestRPCBodyDecodeAdmissionRemainsBoundedBeforeReading(t *testing.T) {
	t.Setenv("FMN_LIMIT_DECODE_REQUESTS", "1")
	app, _ := fixture(t)
	body := &blockedRequestBody{entered: make(chan struct{}), release: make(chan struct{})}
	request := httptest.NewRequest("POST", app.origin+"/api/v1/actions/list_projects", body)
	request.Header.Set("Authorization", "Bearer "+testToken)
	request.Header.Set("Content-Type", "application/json")
	done := make(chan struct{})
	go func() { defer close(done); app.ServeHTTP(httptest.NewRecorder(), request) }()
	defer func() { close(body.release); <-done }()
	<-body.entered
	second := &observedRequestBody{Reader: strings.NewReader("invalid")}
	request = httptest.NewRequest("POST", app.origin+"/api/v1/actions/list_projects", second)
	request.Header.Set("Authorization", "Bearer "+testToken)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	app.ServeHTTP(response, request)
	if response.Code != 429 || second.read.Load() {
		t.Fatalf("unbounded RPC decoding: status=%d bodyRead=%v", response.Code, second.read.Load())
	}
}

func TestReadActionBurstCannotStarveSaveAdmission(t *testing.T) {
	app, _ := fixture(t)
	for i := 0; i < cap(app.rpcWork); i++ {
		app.rpcWork <- struct{}{}
	}

	// Each admission class remains available while the other is saturated.
	w := apiCall(app, "POST", "/api/v1/actions/list_projects", `{}`, testToken)
	if w.Code != 200 {
		t.Fatalf("read actions share write admission: %d %s", w.Code, w.Body.String())
	}
	for i := 0; i < cap(app.rpcWork); i++ {
		<-app.rpcWork
	}
	for i := 0; i < cap(app.rpcReadWork); i++ {
		app.rpcReadWork <- struct{}{}
	}
	defer func() {
		for i := 0; i < cap(app.rpcReadWork); i++ {
			<-app.rpcReadWork
		}
	}()
	w = apiCall(app, "POST", "/api/v1/actions/save_note", `{"project":"default","path":"admission.md","content":"# Save remains available","revision":""}`, testToken)
	if w.Code != 200 {
		t.Fatalf("read burst starved a save: %d %s", w.Code, w.Body.String())
	}
	w = apiCall(app, "POST", "/api/v1/actions/list_projects", `{}`, testToken)
	if w.Code != 429 {
		t.Fatalf("read admission is not bounded: %d %s", w.Code, w.Body.String())
	}

}

func TestSlowReadReleasesDecodeAdmissionBeforeSave(t *testing.T) {
	t.Setenv("FMN_LIMIT_DECODE_REQUESTS", "1")
	t.Setenv("FMN_LIMIT_READ_REQUESTS", "1")
	t.Setenv("FMN_LIMIT_WRITE_REQUESTS", "1")
	app, _ := fixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	addAction(app, mcp.NewServer(&mcp.Implementation{Name: "synthetic-admission", Version: "1"}, nil), &mcp.Tool{Name: "list_projects", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, func(ctx context.Context, _ struct{}) (actionResult, error) {
		close(entered)
		select {
		case <-release:
			return actionResult{OK: true}, nil
		case <-ctx.Done():
			return actionResult{}, ctx.Err()
		}
	})
	done := make(chan int, 1)
	go func() { done <- apiCall(app, "POST", "/api/v1/actions/list_projects", `{}`, testToken).Code }()
	defer func() {
		close(release)
		if status := <-done; status != 200 {
			t.Errorf("read action did not complete: %d", status)
		}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("read action did not start")
	}
	w := apiCall(app, "POST", "/api/v1/actions/save_note", `{"project":"default","path":"during-read.md","content":"# Save during a slow read","revision":""}`, testToken)
	if w.Code != 200 {
		t.Fatalf("slow action held decode admission: %d %s", w.Code, w.Body.String())
	}
}
