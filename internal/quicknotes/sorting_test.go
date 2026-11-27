package quicknotes

import (
	"context"
	"encoding/json"
	"errors"
	"forgetmenote/internal/pg/pgtest"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/decision"
)

func testJudge(t *testing.T, handler http.HandlerFunc) *decision.Service {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	previous := http.DefaultTransport
	http.DefaultTransport = server.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = previous })
	dir := t.TempDir()
	secret := filepath.Join(dir, "synthetic-key")
	if err := os.WriteFile(secret, []byte("synthetic-test-key"), 0600); err != nil {
		t.Fatal(err)
	}
	ledger, err := decision.OpenLedger(pgtest.Org(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ledger.Close)
	judge, err := decision.New(decision.Config{Enabled: true, Endpoint: server.URL + "/v1/systemone", Model: "jev-1.13.0", SecretFile: secret}, ledger, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(judge.Close)
	return judge
}
func routeReply(w http.ResponseWriter, r *http.Request, chosen string) {
	var body struct {
		Questions map[string]struct {
			Criteria map[string]any `json:"criteria"`
		} `json:"questions"`
	}
	if json.NewDecoder(r.Body).Decode(&body) != nil {
		http.Error(w, "bad synthetic request", 400)
		return
	}
	probabilities := map[string]float64{}
	for option := range body.Questions["destination"].Criteria {
		probabilities[option] = 0
	}
	probabilities[chosen] = 1
	json.NewEncoder(w).Encode(map[string]any{"model": "jev-1.13.0", "answers": map[string]any{"destination": map[string]any{"type": "choice", "choice": chosen, "confidence": 1, "probabilities": probabilities}}, "usage": map[string]int{"input_tokens": 200, "output_tokens": 20}})
}
func routeTarget(t *testing.T, c *catalog.Catalog) {
	t.Helper()
	if err := c.Create(catalog.Bootstrap, "target", "Planning"); err != nil {
		t.Fatal(err)
	}
	if err := c.Member(catalog.Bootstrap, "target", "alice", "owner"); err != nil {
		t.Fatal(err)
	}
}
func waitCapture(t *testing.T, s *Service, id, status string) Capture {
	t.Helper()
	account, err := s.catalog.Account("alice")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(6 * time.Second)
	var latest record
	for time.Now().Before(deadline) {
		// Observe this worker without repeatedly validating every Git-backed view.
		latest, err = s.readRecord(account.ID, id)
		if err == nil && latest.Status == status {
			list, listErr := s.List(context.Background(), "alice")
			if listErr != nil {
				t.Fatal(listErr)
			}
			for _, item := range list {
				if item.ID == id && item.Status == status {
					return item
				}
			}
			t.Fatalf("capture reached %s but List did not expose it", status)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("capture did not reach %s: status=%s routing=%s error=%q read_error=%v", status, latest.Status, latest.RoutingState, latest.Error, err)
	return Capture{}
}
func TestSuggestSavesBeforeBlockedJudgmentAndRequiresAcceptance(t *testing.T) {
	s, c, _ := fixture(t)
	routeTarget(t, c)
	called := make(chan struct{}, 1)
	release := make(chan struct{})
	judge := testJudge(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case called <- struct{}{}:
		default:
		}
		select {
		case <-release:
			routeReply(w, r, "destination_000")
		case <-r.Context().Done():
		}
	})
	s.ConfigureJudgments(judge, false)
	settings, _ := s.Settings("alice")
	settings.CloudConsent = true
	if _, err := s.UpdateSettings("alice", settings); err != nil {
		t.Fatal(err)
	}
	capture, err := s.Capture(context.Background(), "alice", "save-before-sort", "# Project planning\nA task for this week.")
	if err != nil || capture.Status != "inbox" {
		t.Fatal("save waited for sorting", capture, err)
	}
	select {
	case <-called:
	case <-time.After(5 * time.Second):
		t.Fatal("classification not queued")
	}
	if _, err = s.Capture(context.Background(), "alice", "independent-save", "second capture"); err != nil {
		t.Fatal("blocked inference prevented independent capture", err)
	}
	close(release)
	suggested := waitCapture(t, s, capture.ID, "suggested")
	if suggested.Suggestion == nil || suggested.CanUndo {
		t.Fatal("missing suggestion or unexpected filing", suggested)
	}
	target, _ := c.Access("alice", "target", "read")
	if _, err = target.Read(context.Background(), capture.Path, ""); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("Suggest filed without acceptance", err)
	}
	if _, err = s.File(context.Background(), "alice", capture.ID, suggested.Suggestion.Destination, suggested.Suggestion.Version); err != nil {
		t.Fatal("suggestion acceptance", err)
	}
}
func TestStaleCloudResultCannotFileNewerCapture(t *testing.T) {
	s, c, _ := fixture(t)
	routeTarget(t, c)
	called := make(chan struct{}, 1)
	release := make(chan struct{})
	judge := testJudge(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case called <- struct{}{}:
		default:
		}
		select {
		case <-release:
			routeReply(w, r, "destination_000")
		case <-r.Context().Done():
		}
	})
	s.ConfigureJudgments(judge, false)
	available, _ := s.Destinations(context.Background(), "alice")
	settings, _ := s.Settings("alice")
	settings.Mode = "automatic"
	settings.CloudConsent = true
	settings.ApprovedDestinations = available
	if _, err := s.UpdateSettings("alice", settings); err != nil {
		t.Fatal(err)
	}
	saved, err := s.Capture(context.Background(), "alice", "stale-sort", "planning")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-called:
	case <-time.After(5 * time.Second):
		t.Fatal("classification not queued")
	}
	store, _ := c.Access("alice", saved.Project, "owner")
	if _, err = store.Save(context.Background(), saved.Path, []byte("newer unrelated private work"), saved.Version, "alice"); err != nil {
		t.Fatal(err)
	}
	close(release)
	account, _ := c.Account("alice")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r, e := s.readRecord(account.ID, saved.ID)
		if e == nil && r.RoutingState == "done" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	target, _ := c.Access("alice", "target", "read")
	if _, err = target.Read(context.Background(), saved.Path, ""); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("stale judgment disclosed capture", err)
	}
	now, err := store.Read(context.Background(), saved.Path, "")
	if err != nil || string(now.Content) != "newer unrelated private work" {
		t.Fatal("newer work changed", now, err)
	}
}
func TestAutomaticApprovedDestinationAndLocalOnly(t *testing.T) {
	s, c, _ := fixture(t)
	routeTarget(t, c)
	var calls atomic.Int32
	judge := testJudge(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); routeReply(w, r, "destination_000") })
	s.ConfigureJudgments(judge, true)
	available, _ := s.Destinations(context.Background(), "alice")
	settings, _ := s.Settings("alice")
	settings.Mode = "automatic"
	settings.CloudConsent = true
	settings.ApprovedDestinations = available
	if _, err := s.UpdateSettings("alice", settings); err != nil {
		t.Fatal(err)
	}
	saved, err := s.Capture(context.Background(), "alice", "auto-sort", "planning")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Sort(context.Background(), "alice", saved.ID); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatal("local-only sent capture")
	}
	s.ConfigureJudgments(judge, false)
	filed := waitCapture(t, s, saved.ID, "filed")
	if !filed.CanUndo || filed.Project != "target" || calls.Load() != 1 {
		t.Fatal("automatic approved filing", filed, calls.Load())
	}
}
func TestRoutingDecisionKeepsAmbiguousAndNoMatchInInbox(t *testing.T) {
	confidence := 0.45
	destinations := []Destination{{Project: "one", Name: "Planning"}, {Project: "two", Name: "Planning"}}
	answer := decision.Answer{Type: "choice", Choice: "destination_000", Confidence: &confidence, Probabilities: map[string]float64{"destination_000": 0.55, "destination_001": 0.44, "inbox": 0.01}}
	d, automatic, err := RoutingDecision(decision.Result{Answers: map[string]decision.Answer{"destination": answer}}, destinations)
	if err != nil || d.Project != "one" || automatic {
		t.Fatal("ambiguous result auto-filed", d, automatic, err)
	}
	answer.Choice = "inbox"
	d, automatic, err = RoutingDecision(decision.Result{Answers: map[string]decision.Answer{"destination": answer}}, destinations)
	if err != nil || d.Project != "" || automatic {
		t.Fatal("no match filed", d, automatic, err)
	}
	if _, err = BuildRoutingRequest("note", make([]Destination, 65)); !errors.Is(err, ErrInput) {
		t.Fatal("silent candidate truncation", err)
	}
}
func TestExplicitRetryAfterProviderFailureUsesNewRecordedOperation(t *testing.T) {
	s, c, _ := fixture(t)
	routeTarget(t, c)
	var calls atomic.Int32
	judge := testJudge(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, "synthetic provider outage", 503)
			return
		}
		routeReply(w, r, "destination_000")
	})
	s.ConfigureJudgments(judge, false)
	settings, _ := s.Settings("alice")
	settings.CloudConsent = true
	if _, err := s.UpdateSettings("alice", settings); err != nil {
		t.Fatal(err)
	}
	saved, err := s.Capture(context.Background(), "alice", "retry-sort", "planning")
	if err != nil {
		t.Fatal(err)
	}
	account, _ := c.Account("alice")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r, e := s.readRecord(account.ID, saved.ID)
		if e == nil && r.RoutingState == "done" && r.Error != "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	retried, err := s.Sort(context.Background(), "alice", saved.ID)
	if err != nil || retried.Suggestion == nil || calls.Load() != 2 {
		t.Fatal("explicit retry did not use a fresh bounded operation", retried, err, calls.Load())
	}
}
