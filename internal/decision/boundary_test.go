package decision

import (
	"context"
	"errors"
	"fmt"
	"forgetmenote/internal/pg/pgtest"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestEvaluationProhibitsLocalOnlyAndRequiresFreshScope(t *testing.T) {
	for _, kind := range []string{"localOnly", "missingCheck", "revoked", "staleAfterCall", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			var calls atomic.Int32
			s, l := fixture(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); goodResponse(w) })
			defer s.Close()
			e := evaluation()
			ctx := context.Background()
			rejected := errors.New("scope no longer authorized or fresh")
			switch kind {
			case "localOnly":
				e.LocalOnly = true
			case "missingCheck":
				e.Check = nil
			case "revoked":
				e.Check = func(context.Context) error { return rejected }
			case "staleAfterCall":
				e.Check = func(context.Context) error {
					if calls.Load() > 0 {
						return rejected
					}
					return nil
				}
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			result, err := s.Evaluate(ctx, e)
			if err == nil || len(result.Answers) != 0 {
				t.Fatalf("unsafe result escaped: %+v %v", result, err)
			}
			want := int32(0)
			if kind == "staleAfterCall" {
				want = 1
			}
			if calls.Load() != want {
				t.Fatalf("unexpected paid calls: %d", calls.Load())
			}
			records, _ := l.Records()
			if len(records) != int(want) {
				t.Fatalf("accounting missing: %+v", records)
			}
		})
	}
}

func TestProviderAnswerValidationAndUnknownUsage(t *testing.T) {
	tests := []struct {
		name, answer, usage string
		known               bool
	}{
		{"missing answer", `{}`, `{"input_tokens":50,"output_tokens":8}`, true},
		{"wrong type", `{"support":{"type":"choice","choice":"yes"}}`, `{"input_tokens":50,"output_tokens":8}`, true},
		{"non-native boolean", `{"support":{"type":"boolean","noul":0.8}}`, `{"input_tokens":50,"output_tokens":8}`, true},
		{"out of range", `{"support":{"type":"noul","noul":1.1}}`, `{"input_tokens":50,"output_tokens":8}`, true},
		{"missing probability", `{"support":{"type":"noul"}}`, `{"input_tokens":50,"output_tokens":8}`, true},
		{"missing usage", `{"support":{"type":"noul","noul":0.8}}`, `null`, false},
		{"partial usage", `{"support":{"type":"noul","noul":0.8}}`, `{"input_tokens":50}`, false},
		{"negative usage", `{"support":{"type":"noul","noul":0.8}}`, `{"input_tokens":-1,"output_tokens":8}`, false},
		{"extra answer", `{"support":{"type":"noul","noul":0.8},"invented":{"type":"noul","noul":0.8}}`, `{"input_tokens":50,"output_tokens":8}`, true},
		{"duplicate answer", `{"support":{"type":"noul","noul":0.8},"support":{"type":"noul","noul":0.2}}`, `{"input_tokens":50,"output_tokens":8}`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, l := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"model":"jev-1.13.0","answers":%s,"usage":%s}`, tt.answer, tt.usage)
			})
			defer s.Close()
			result, err := s.Evaluate(context.Background(), evaluation())
			if !errors.Is(err, ErrInvalid) || len(result.Answers) != 0 {
				t.Fatalf("accepted malformed answer: %+v %v", result, err)
			}
			records, _ := l.Records()
			if len(records) != 1 || records[0].Tokens() <= 0 || (records[0].Usage != nil) != tt.known {
				t.Fatalf("lost failed-call usage: %+v", records)
			}
		})
	}
}

func TestTypedChoiceAndScore(t *testing.T) {
	for _, mutate := range []string{"valid", "wrong choice", "missing level", "wrong legend", "wrong model", "score range", "invalid sum"} {
		t.Run(mutate, func(t *testing.T) {
			body := `{"model":"jev-1.13.0","answers":{"choice":{"type":"choice","choice":"support","probabilities":{"support":0.95,"uncertain":0.05},"confidence":0.9},"score":{"type":"score","score":1.94,"probabilities":{"0":0.02,"1":0.03,"2":0.95},"confidence":0.91,"legend":{"0":"unrelated","1":"related","2":"answers"}}},"usage":{"input_tokens":100,"output_tokens":60}}`
			switch mutate {
			case "wrong choice":
				body = strings.Replace(body, `"choice":"support"`, `"choice":"uncertain"`, 1)
			case "missing level":
				body = strings.Replace(body, `"0":0.02,`, "", 1)
			case "wrong legend":
				body = strings.Replace(body, `"0":"unrelated"`, `"0":"injected"`, 1)
			case "wrong model":
				body = strings.Replace(body, "jev-1.13.0", "other-model", 1)
			case "score range":
				body = strings.Replace(body, `"score":1.94`, `"score":2.1`, 1)
			case "invalid sum":
				body = strings.Replace(body, `"2":0.95`, `"2":0.15`, 1)
			}
			s, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) })
			defer s.Close()
			e := evaluation()
			e.Request.Questions = map[string]Question{"choice": {Type: "choice", Instructions: "Support?", Criteria: map[string]string{"support": "Source supports", "uncertain": "Unknown"}}, "score": {Type: "score", Instructions: "Relevance?", Criteria: []string{"unrelated", "related", "answers"}}}
			_, err := s.Evaluate(context.Background(), e)
			if (err == nil) != (mutate == "valid") {
				t.Fatalf("%s: %v", mutate, err)
			}
		})
	}
}

func TestPaidRetriesAreSeparatelyReservedAndReauthorized(t *testing.T) {
	var calls, checks atomic.Int32
	s, l := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(429)
			w.Write([]byte("private provider body"))
			return
		}
		goodResponse(w)
	})
	defer s.Close()
	s.config.Limits.Retries = 1
	e := evaluation()
	e.Check = func(context.Context) error { checks.Add(1); return nil }
	if _, err := s.Evaluate(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	records, _ := l.Records()
	if calls.Load() != 2 || checks.Load() != 3 || len(records) != 2 || records[0].Status != "unknown" || records[1].Status != "settled" {
		t.Fatalf("retry lost identity/usage: %+v calls %d checks %d", records, calls.Load(), checks.Load())
	}
	if _, err := s.Evaluate(context.Background(), e); !errors.Is(err, ErrAlreadyAttempted) {
		t.Fatalf("replayed a durable operation: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatal("replay made a duplicate call")
	}
}

func TestProviderRedirectAndErrorsCannotLeakSecrets(t *testing.T) {
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1) }))
	defer target.Close()
	for _, status := range []int{307, 401, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			s, l := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", target.URL)
				w.WriteHeader(status)
				w.Write([]byte("private-synthetic-marker"))
			})
			defer s.Close()
			_, err := s.Evaluate(context.Background(), evaluation())
			if err == nil || strings.Contains(err.Error(), "private-synthetic-marker") || strings.Contains(err.Error(), "synthetic-key") {
				t.Fatalf("provider failure was unsafe: %v", err)
			}
			records, _ := l.Records()
			if len(records) != 1 || records[0].Status != "unknown" {
				t.Fatal("failed call was free")
			}
		})
	}
	if redirected.Load() != 0 {
		t.Fatal("redirect followed with private request")
	}
}

func TestCancelledPaidCallKeepsUnknownUsage(t *testing.T) {
	entered := make(chan struct{})
	finish := make(chan struct{})
	defer close(finish)
	s, l := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		select {
		case <-r.Context().Done():
		case <-finish:
		}
	})
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := s.Evaluate(ctx, evaluation()); done <- err }()
	<-entered
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation stalled")
	}
	records, _ := l.Records()
	if len(records) != 1 || records[0].Status != "unknown" || records[0].Tokens() < 1 {
		t.Fatal("cancelled paid call lost accounting")
	}
}

func TestResourceLimitsApplyBeforeNetwork(t *testing.T) {
	for _, kind := range []string{"bytes", "context", "question context", "questions", "records"} {
		t.Run(kind, func(t *testing.T) {
			var calls atomic.Int32
			s, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); goodResponse(w) })
			defer s.Close()
			e := evaluation()
			switch kind {
			case "bytes":
				s.config.Limits.RequestBytes = 10
			case "context":
				e.Request.State = strings.Repeat("x", 33000)
			case "question context":
				e.Request.State = strings.Repeat("x", 16000)
				q := e.Request.Questions["support"]
				q.Instructions = strings.Repeat("q", 17000)
				e.Request.Questions["support"] = q
			case "questions":
				s.config.Limits.Questions = 0
			case "records":
				s.config.Limits.LedgerRecords = 0
			}
			if _, err := s.Evaluate(context.Background(), e); err == nil {
				t.Fatal("ignored bound")
			}
			if calls.Load() != 0 {
				t.Fatal("bound checked after transmission")
			}
		})
	}
}

func TestBackgroundYieldsCapacityToInteractive(t *testing.T) {
	started := make(chan string, 4)
	release := make(chan struct{})
	s, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) { started <- "call"; <-release; goodResponse(w) })
	defer s.Close()
	done := make(chan error, 3)
	run := func(op string, background bool) {
		e := evaluation()
		e.Scope.Operation = op
		e.Background = background
		_, err := s.Evaluate(context.Background(), e)
		done <- err
	}
	go run("background-1", true)
	<-started
	go run("background-2", true)
	// One background request must leave the second slot available for interaction.
	go run("interactive", false)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("interactive work starved")
	}
	s.mu.Lock()
	active, bg := s.active, s.background
	s.mu.Unlock()
	if active != 2 || bg != 1 {
		t.Fatalf("background consumed interactive capacity: active %d background %d", active, bg)
	}
	close(release)
	for i := 0; i < 3; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

// Gateways share the ledger: an attempt reserved by one is refused to every
// other, survives a restart, and is never settled except by its reserver.
func TestLedgerFencesAttemptsAcrossGatewaysAndRestarts(t *testing.T) {
	db := pgtest.Org(t)
	l, err := OpenLedger(db)
	if err != nil {
		t.Fatal(err)
	}
	other, err := OpenLedger(db)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	c := Config{Enabled: true, Model: "jev-1.13.0"}
	if err := c.Validate(false); err != nil {
		t.Fatal(err)
	}
	e := evaluation()
	id, err := l.reserve(e.Scope, 1, []byte("synthetic"), 500, c)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.reserve(e.Scope, 1, []byte("synthetic"), 500, c); !errors.Is(err, ErrAlreadyAttempted) {
		t.Fatalf("another gateway reserved the same attempt: %v", err)
	}
	if err := other.settle(id, Result{Usage: &Usage{InputTokens: 1, OutputTokens: 1}}); !errors.Is(err, ErrAlreadyAttempted) {
		t.Fatalf("another gateway settled the attempt: %v", err)
	}
	if records, _ := l.Records(); len(records) != 1 || records[0].Status != "reserved" {
		t.Fatalf("the reserving gateway lost its in-flight attempt: %+v", records)
	}
	l.Close()
	l, err = OpenLedger(db)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	records, _ := l.Records()
	if len(records) != 1 || records[0].Status != "unknown" || records[0].Tokens() != 500 {
		t.Fatalf("restart forgave interrupted work: %+v", records)
	}
	if err := l.settle(id, Result{Usage: &Usage{InputTokens: 1, OutputTokens: 1}}); !errors.Is(err, ErrAlreadyAttempted) {
		t.Fatalf("restart settled interrupted work: %v", err)
	}
	if _, err := l.reserve(e.Scope, 1, []byte("synthetic"), 500, c); !errors.Is(err, ErrAlreadyAttempted) {
		t.Fatalf("restart replayed operation: %v", err)
	}
	c.Limits.DailyTokens = 600
	e.Scope.Operation = "next"
	if _, err := l.reserve(e.Scope, 1, []byte("synthetic"), 101, c); err != nil {
		t.Fatalf("retained usage became an admission quota: %v", err)
	}
}

func TestLedgerRefusesDamagedRecordsAndUnreachableStorage(t *testing.T) {
	s, l := fixture(t, func(w http.ResponseWriter, r *http.Request) { goodResponse(w) })
	if _, err := s.Evaluate(context.Background(), evaluation()); err != nil {
		t.Fatal(err)
	}
	if _, err := l.db.Pool.Exec(context.Background(), `UPDATE jev_usage SET record = jsonb_set(record, '{attempt}', '9') WHERE org = $1`, l.db.Org); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Records(); !errors.Is(err, ErrStorage) {
		t.Fatal("damaged ledger accepted", err)
	}
	var calls atomic.Int32
	db := pgtest.Database(t)
	s, _ = fixtureOn(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); goodResponse(w) }, db)
	defer s.Close()
	db.Close()
	if _, err := s.Evaluate(context.Background(), evaluation()); !errors.Is(err, ErrStorage) {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatal("called provider after failed reservation")
	}
}
