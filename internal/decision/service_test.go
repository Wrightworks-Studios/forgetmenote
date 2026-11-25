package decision

import (
	"context"
	"encoding/json"
	"forgetmenote/internal/pg"
	"forgetmenote/internal/pg/pgtest"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func fixture(t *testing.T, handler http.HandlerFunc) (*Service, *Ledger) {
	t.Helper()
	return fixtureOn(t, handler, pgtest.Org(t))
}
func fixtureOn(t *testing.T, handler http.HandlerFunc, db *pg.DB) (*Service, *Ledger) {
	t.Helper()
	ts := httptest.NewTLSServer(handler)
	t.Cleanup(ts.Close)
	dir := t.TempDir()
	secret := filepath.Join(dir, "key")
	if err := os.WriteFile(secret, []byte("synthetic-key\n"), 0600); err != nil {
		t.Fatal(err)
	}
	l, err := OpenLedger(db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.Close)
	s, err := New(Config{Enabled: true, Endpoint: ts.URL + "/v1/systemone", Model: "jev-1.13.0", SecretFile: secret}, l, false)
	if err != nil {
		t.Fatal(err)
	}
	s.provider.(*typesafeClient).http.Transport = ts.Client().Transport
	return s, l
}
func evaluation() Evaluation {
	return Evaluation{Scope: Scope{Actor: "alice", Notebook: "notebook", Feature: "citations", Run: "run", Chat: "chat", Operation: "claim-1"}, Request: Request{State: "Synthetic claim and evidence", Questions: map[string]Question{"support": {Type: Boolean, Instructions: "Does evidence support the claim?"}}}, Check: func(context.Context) error { return nil }}
}
func goodResponse(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"model":"jev-1.13.0","answers":{"support":{"type":"noul","noul":0.92}},"usage":{"input_tokens":321,"output_tokens":17}}`))
}

func TestUnsupportedFeatureNeverCallsProvider(t *testing.T) {
	for _, feature := range []string{"repository-discovery", "unknown-feature"} {
		t.Run(feature, func(t *testing.T) {
			var calls atomic.Int32
			s, ledger := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				goodResponse(w)
			})
			e := evaluation()
			e.Scope.Feature = feature
			if _, err := s.Evaluate(t.Context(), e); err == nil {
				t.Error("unsupported feature accepted")
			}
			if calls.Load() != 0 {
				t.Error("unsupported feature reached the provider")
			}
			records, err := ledger.Records()
			if err != nil || len(records) != 0 {
				t.Fatal("unsupported feature created a usage record", err)
			}
		})
	}
}

func TestEvaluationUsesNativeContractAndDurableReservation(t *testing.T) {
	var calls atomic.Int32
	var l *Ledger
	s, ledger := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "POST" || r.URL.Path != "/v1/systemone" || r.Header.Get("Authorization") != "Bearer synthetic-key" {
			t.Errorf("wrong native request")
		}
		var body map[string]json.RawMessage
		if json.NewDecoder(r.Body).Decode(&body) != nil || len(body) != 3 || string(body["model"]) != `"jev-1.13.0"` {
			t.Errorf("unexpected body fields: %v", body)
		}
		var questions map[string]Question
		if json.Unmarshal(body["questions"], &questions) != nil || questions["support"].Type != "noul" {
			t.Errorf("boolean question was not translated to TypeSafe: %s", body["questions"])
		}
		records, err := l.Records()
		if err != nil || len(records) != 1 || records[0].Status != "reserved" {
			t.Errorf("network call without durable reservation: %v %v", records, err)
		}
		goodResponse(w)
	})
	l = ledger
	var checks atomic.Int32
	e := evaluation()
	e.Check = func(context.Context) error { checks.Add(1); return nil }
	result, err := s.Evaluate(context.Background(), e)
	if err != nil || result.Answers["support"].Type != Boolean || result.Answers["support"].Probability == nil || *result.Answers["support"].Probability != .92 {
		t.Fatalf("native judgment missing: %+v %v", result, err)
	}
	records, err := l.Records()
	if err != nil || len(records) != 1 || records[0].Status != "settled" || records[0].Tokens() != 338 || checks.Load() < 2 || calls.Load() != 1 {
		t.Fatalf("incorrect accounting or scope checks: %+v %v", records, err)
	}
}

func TestExistingAnswerCheckpointKeepsItsEncoding(t *testing.T) {
	raw := []byte(`{"support":{"type":"noul","noul":0.92}}`)
	var answers map[string]Answer
	if !DecodeRecord(raw, &answers) || answers["support"].Type != Boolean || answers["support"].Probability == nil || *answers["support"].Probability != .92 {
		t.Fatal("existing boolean checkpoint was not restored", answers)
	}
	saved, err := json.Marshal(answers)
	if err != nil || string(saved) != string(raw) {
		t.Fatalf("existing checkpoint encoding changed: %s, %v", saved, err)
	}
}
