package dataops

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/connectors"
	"forgetmenote/internal/pg"
	"forgetmenote/internal/pg/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

type observed struct {
	receipt string
	err     error
}

// fixture serves one HTTP connection whose upstream waits at each request
// until the test lets it answer, so the test can act between the call and its
// outcome record.
func fixture(t *testing.T, status int) (*Service, *pg.DB, *atomic.Int32, chan struct{}, chan struct{}, *[]observed) {
	t.Helper()
	reached, respond := make(chan struct{}), make(chan struct{})
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		reached <- struct{}{}
		<-respond
		w.WriteHeader(status)
		w.Write([]byte(`{"id":"message-1"}`))
	}))
	t.Cleanup(upstream.Close)
	text := json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"],"additionalProperties":false}`)
	manager, err := connectors.New(connectors.Config{Version: 1, Connections: []connectors.Connection{{ID: "webhook", Kind: "http", Endpoint: upstream.URL, Operations: []connectors.Operation{
		{Name: "notify", ReadOnly: false, Method: "POST", Path: "/notify", Body: true, InputSchema: text},
		{Name: "read", ReadOnly: true, Method: "GET", Path: "/read"},
	}}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Close)
	db := pgtest.Org(t)
	projects, err := catalog.Open(t.TempDir(), nil, db)
	if err != nil {
		t.Fatal(err)
	}
	observations := &[]observed{}
	s, err := New(projects, Hooks{
		Manager:      func() *connectors.Manager { return manager },
		Allowed:      func(context.Context, string, string) bool { return true },
		WriteAllowed: func(context.Context, Call) bool { return true },
		ReadProfile:  func(context.Context, Call) error { return nil },
		Submit: func(context.Context, Call, json.RawMessage) (connectors.Result, error) {
			t.Error("an unmanaged write was submitted as a managed one")
			return connectors.Result{}, nil
		},
		Observe: func(_ context.Context, _ Call, receipt connectors.Receipt, err error, _ time.Time) {
			*observations = append(*observations, observed{receipt.ID, err})
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return s, db, &requests, reached, respond, observations
}

// executeWhileOutcomeFails runs in, and once its call reaches the upstream makes
// the activity log refuse writes, so only the outcome record fails.
func executeWhileOutcomeFails(t *testing.T, s *Service, db *pg.DB, reached, respond chan struct{}, in Call) (Result, error) {
	t.Helper()
	type outcome struct {
		result Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := s.Execute(context.Background(), in)
		done <- outcome{result, err}
	}()
	select {
	case <-reached:
	case <-time.After(10 * time.Second):
		t.Fatal("the call did not reach the upstream")
	}
	restore := pgtest.FailTable(t, db, "catalog_activity")
	defer restore()
	close(respond)
	select {
	case got := <-done:
		return got.result, got.err
	case <-time.After(10 * time.Second):
		t.Fatal("the call did not finish")
	}
	return Result{}, nil
}

// A write that completed upstream is returned even when its outcome could not be
// recorded: refusing it would invite the caller to repeat the external effect.
func TestCompletedWriteSurvivesAFailedOutcomeRecord(t *testing.T) {
	s, db, requests, reached, respond, observations := fixture(t, http.StatusOK)
	result, err := executeWhileOutcomeFails(t, s, db, reached, respond, Call{Connection: "webhook", Operation: "notify", Arguments: map[string]any{"text": "hello"}})
	if err != nil {
		t.Fatal("a completed write was reported as failed", err)
	}
	data, _ := result.Data.(map[string]any)
	if data["id"] != "message-1" || result.Receipt.ID == "" || !result.Receipt.Complete {
		t.Fatalf("the completed write's result was lost: %+v", result)
	}
	if !strings.Contains(result.Warning, "do not repeat") {
		t.Fatalf("the missing outcome record was not reported: %q", result.Warning)
	}
	if requests.Load() != 1 {
		t.Fatal("the write was sent more than once", requests.Load())
	}
	if len(*observations) != 1 || (*observations)[0].receipt != result.Receipt.ID || (*observations)[0].err != nil {
		t.Fatalf("the completed write was not observed: %+v", *observations)
	}
}

func TestFailedCallKeepsItsOwnErrorWhenItsOutcomeIsNotRecorded(t *testing.T) {
	for _, in := range []Call{
		{Connection: "webhook", Operation: "notify", Arguments: map[string]any{"text": "hello"}},
		{Connection: "webhook", Operation: "read", Arguments: map[string]any{}},
	} {
		t.Run(in.Operation, func(t *testing.T) {
			s, db, _, reached, respond, observations := fixture(t, http.StatusBadGateway)
			_, err := executeWhileOutcomeFails(t, s, db, reached, respond, in)
			if err == nil || !strings.Contains(err.Error(), "upstream HTTP 502") {
				t.Fatal("the call's own failure was replaced", err)
			}
			if len(*observations) != 1 || (*observations)[0].err == nil {
				t.Fatalf("the failed call was not observed: %+v", *observations)
			}
		})
	}
}

// A read discloses nothing without its outcome record; repeating it is harmless.
func TestReadFailsClosedWhenItsOutcomeIsNotRecorded(t *testing.T) {
	s, db, _, reached, respond, observations := fixture(t, http.StatusOK)
	result, err := executeWhileOutcomeFails(t, s, db, reached, respond, Call{Connection: "webhook", Operation: "read", Arguments: map[string]any{}})
	if err == nil || result.Data != nil || result.Receipt.ID != "" {
		t.Fatalf("a read without its outcome record was disclosed: %+v %v", result, err)
	}
	if len(*observations) != 1 {
		t.Fatalf("the upstream call was not observed: %+v", *observations)
	}
}

func TestRecordedWriteHasNoWarning(t *testing.T) {
	s, _, _, reached, respond, _ := fixture(t, http.StatusOK)
	go func() {
		<-reached
		close(respond)
	}()
	result, err := s.Execute(context.Background(), Call{Connection: "webhook", Operation: "notify", Arguments: map[string]any{"text": "hello"}})
	if err != nil || result.Warning != "" || result.Receipt.ID == "" {
		t.Fatalf("an ordinary write: %+v %v", result, err)
	}
}
