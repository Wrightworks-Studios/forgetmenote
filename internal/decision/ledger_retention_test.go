package decision

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"forgetmenote/internal/pg"
	"forgetmenote/internal/pg/pgtest"
)

type usageTotal struct {
	Day, Feature, Actor, Notebook, Requested, Returned string
	Attempts, Input, Output, Estimate                  int64
}

func usageTotals(t *testing.T, db *pg.DB) []usageTotal {
	t.Helper()
	rows, err := db.Pool.Query(t.Context(), `SELECT day::text, feature, actor, notebook, requested_model, returned_model, attempts, input_tokens, output_tokens, estimate
		FROM jev_usage_totals WHERE org = $1 ORDER BY day, feature`, db.Org)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := []usageTotal{}
	for rows.Next() {
		var u usageTotal
		if err := rows.Scan(&u.Day, &u.Feature, &u.Actor, &u.Notebook, &u.Requested, &u.Returned, &u.Attempts, &u.Input, &u.Output, &u.Estimate); err != nil {
			t.Fatal(err)
		}
		out = append(out, u)
	}
	if rows.Err() != nil {
		t.Fatal(rows.Err())
	}
	return out
}

// A full ledger made every later judgment fail for good. It now makes room by
// turning its oldest settled attempts into daily totals, while an uncertain
// attempt stays and still fences its operation.
func TestFullLedgerTurnsOldSettledAttemptsIntoDailyTotals(t *testing.T) {
	db := pgtest.Org(t)
	var calls atomic.Int32
	s, ledger := fixtureOn(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); goodResponse(w) }, db)
	defer s.Close()
	s.config.Limits.LedgerRecords = 8
	old := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	ledger.now = func() time.Time { return old }

	// Another gateway's ledger stopped with this call in flight.
	other, err := OpenLedger(db)
	if err != nil {
		t.Fatal(err)
	}
	other.now = ledger.now
	defer other.Close()
	uncertain := evaluation()
	uncertain.Scope.Operation = "uncertain"
	raw, estimate, err := s.provider.prepare(uncertain.Request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = other.reserve(uncertain.Scope, 1, raw, estimate, *s.config); err != nil {
		t.Fatal(err)
	}
	for i := range 7 {
		e := evaluation()
		e.Scope.Operation = fmt.Sprintf("old-%d", i)
		if _, err := s.Evaluate(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}

	ledger.now = func() time.Time { return old.AddDate(0, 0, 30) }
	fresh := evaluation()
	fresh.Scope.Operation = "fresh"
	if _, err := s.Evaluate(context.Background(), fresh); err != nil {
		t.Fatalf("a full ledger refused new work: %v (step %+v)", err, failureOf(err))
	}
	records, err := ledger.Records()
	if err != nil {
		t.Fatal(err)
	}
	kept := map[string]string{}
	for _, r := range records {
		kept[r.Scope.Operation] = r.Status
	}
	// Three-quarters of eight stay: the uncertain call and the five newest
	// settled ones, then the fresh attempt.
	if len(records) != 7 || kept["uncertain"] != "unknown" || kept["old-0"] != "" || kept["old-1"] != "" || kept["old-2"] != "settled" || kept["fresh"] != "settled" {
		t.Fatalf("ledger after compaction: %v", kept)
	}
	totals := usageTotals(t, db)
	if len(totals) != 1 || totals[0] != (usageTotal{Day: "2026-10-01", Feature: "citations", Actor: "alice", Notebook: "notebook", Requested: "jev-1.13.0", Returned: "jev-1.13.0", Attempts: 2, Input: 642, Output: 34, Estimate: 2 * records[1].Estimate}) {
		t.Fatalf("daily totals: %+v", totals)
	}
	if _, err := s.Evaluate(context.Background(), uncertain); !errors.Is(err, ErrAlreadyAttempted) || calls.Load() != 8 {
		t.Fatalf("an uncertain call lost its fence: %v, %d calls", err, calls.Load())
	}
}

// Recent attempts are never compacted: an operation retried after a restart
// must still find its attempt, so the ledger fails closed and says where.
func TestFullLedgerOfRecentAttemptsFailsClosedAtReserve(t *testing.T) {
	db := pgtest.Org(t)
	var calls atomic.Int32
	s, ledger := fixtureOn(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); goodResponse(w) }, db)
	defer s.Close()
	s.config.Limits.LedgerRecords = 3
	now := time.Date(2026, 11, 9, 15, 33, 0, 0, time.UTC)
	ledger.now = func() time.Time { return now }
	for i := range 3 {
		e := evaluation()
		e.Scope.Operation = fmt.Sprintf("recent-%d", i)
		if _, err := s.Evaluate(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
	ledger.now = func() time.Time { return now.AddDate(0, 0, 6) }
	e := evaluation()
	e.Scope.Operation = "next"
	_, err := s.Evaluate(context.Background(), e)
	if !errors.Is(err, ErrLimit) || err.Error() != ErrLimit.Error() || failureOf(err) != (Failure{Step: "reserve"}) || calls.Load() != 3 {
		t.Fatalf("full ledger: %v %+v, %d calls", err, failureOf(err), calls.Load())
	}
	if records, _ := ledger.Records(); len(records) != 3 || len(usageTotals(t, db)) != 0 {
		t.Fatalf("recent attempts were compacted: %d records", len(records))
	}
}

func failureOf(err error) Failure {
	f, _ := Failed(err)
	return f
}

// A caller logs why Evaluate failed without seeing the request: the step and,
// for a provider response, its status. The error itself is unchanged.
func TestEvaluateReportsTheStepThatFailed(t *testing.T) {
	t.Run("provider", func(t *testing.T) {
		s, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"error":"invalid key"}`, http.StatusUnauthorized)
		})
		defer s.Close()
		_, err := s.Evaluate(context.Background(), evaluation())
		if !errors.Is(err, ErrUnavailable) || err.Error() != ErrUnavailable.Error() || failureOf(err) != (Failure{Step: "provider", Status: 401}) {
			t.Fatalf("provider failure: %v %+v", err, failureOf(err))
		}
	})
	t.Run("check", func(t *testing.T) {
		s, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) { goodResponse(w) })
		defer s.Close()
		denied := errors.New("synthetic permission change")
		e := evaluation()
		e.Check = func(context.Context) error { return denied }
		if _, err := s.Evaluate(context.Background(), e); !errors.Is(err, denied) || failureOf(err) != (Failure{Step: "check"}) {
			t.Fatalf("check failure: %v %+v", err, failureOf(err))
		}
	})
	t.Run("not Evaluate", func(t *testing.T) {
		if f, ok := Failed(ErrLimit); ok || f != (Failure{}) {
			t.Fatalf("a bare error named a step: %+v", f)
		}
	})
}
