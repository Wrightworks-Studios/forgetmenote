package jev

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"forgetmenote/internal/blob"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/decision"
	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
	"forgetmenote/internal/outgoing"
	"forgetmenote/internal/pg"
	"forgetmenote/internal/pg/pgtest"
	"forgetmenote/internal/quicknotes"
	"forgetmenote/internal/search"
	"forgetmenote/internal/webcore"
	"forgetmenote/internal/workspace"
)

func TestMain(m *testing.M) {
	catalog.UseTestPasswordCost()
	pgtest.Main(m)
}

// These tests exercise Jev's own behaviour on real dependencies: a catalog of
// real Git notebooks in test Postgres, Jev's records in their own
// organization, real search indexes, the decision policy for consent, and a
// model stub over httptest. The routes are served without the instance's
// guard and take the person from a header, so nothing here tests who may use
// Jev: web tests that through the real routes.

const testUserHeader = "X-Test-User"

// testInstance is what a fixture serves Jev with: the routes and the judge an
// instance's gateway would hold.
type testInstance struct {
	data   string
	mux    *http.ServeMux
	judge  atomic.Pointer[decision.Service]
	config decision.Config
	ledger *decision.Ledger
	policy *decision.Policy
}

var instances sync.Map // *Service -> *testInstance

func instanceOf(t testing.TB, app *Service) *testInstance {
	t.Helper()
	inst, ok := instances.Load(app)
	if !ok {
		t.Fatal("the service is no fixture")
	}
	return inst.(*testInstance)
}

// openJev opens Jev on c and db with the routes and judge of inst.
func openJev(t testing.TB, c *catalog.Catalog, db *pg.DB, indexes *search.Indexes, inst *testInstance) *Service {
	t.Helper()
	var app *Service
	judge := func() (*decision.Service, bool) { return inst.judge.Load(), false }
	app, err := New(c, db, &outgoing.Mode{}, indexes, Hooks{
		Identity: func(r *http.Request) (string, error) {
			if user := r.Header.Get(testUserHeader); user != "" {
				return user, nil
			}
			return "", workspace.ErrForbidden
		},
		RejectNotebookAI: func(w http.ResponseWriter) bool {
			if _, managed := c.AISettings(); managed {
				http.Error(w, catalog.ErrManagedAI.Error(), http.StatusConflict)
				return true
			}
			return false
		},
		Judge: judge,
		Authorize: func(ctx context.Context, user, project, feature string, expected *decision.Service) error {
			return inst.policy.Authorize(ctx, user, project, feature, expected)
		},
		InboxAIAllowed:          func(string, catalog.Project) error { return nil },
		ProtectedInboxAIAllowed: func(string, string) error { return nil },
		Preferences:             func(user string) (decision.Preferences, error) { return inst.policy.Preferences(user) },
		SyncInstanceAI:          func(context.Context) {},
		Quicknotes:              func() *quicknotes.Service { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	inst.policy = decision.NewPolicy(c, judge, app.StoredPreferences)
	inst.mux = http.NewServeMux()
	app.Register(func(pattern, _ string, handler http.HandlerFunc) {
		method, path, _ := strings.Cut(pattern, " ")
		inst.mux.HandleFunc(method+" /p/{notebook}"+path, func(w http.ResponseWriter, r *http.Request) {
			handler(w, r.WithContext(context.WithValue(r.Context(), webcore.NotebookIDKey{}, r.PathValue("notebook"))))
		})
	}, inst.mux.HandleFunc)
	instances.Store(app, inst)
	t.Cleanup(func() { instances.Delete(app) })
	return app
}

// chatFixture opens Jev with no judge configured, for an instance where alice
// and bob may read the default notebook.
func chatFixture(t *testing.T) *Service {
	t.Helper()
	dir := t.TempDir()
	projects, err := catalog.Open(dir, nil, pgtest.Dir(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { projects.Close() })
	db := pgtest.Org(t)
	indexes := search.NewIndexes(db, projects, nil)
	if err = indexes.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(indexes.Close)
	app := openJev(t, projects, db, indexes, &testInstance{data: dir})
	for _, user := range []string{"alice", "bob"} {
		if err := app.catalog.Member(catalog.Bootstrap, "default", user, "read"); err != nil {
			t.Fatal(err)
		}
	}
	return app
}

// configureSyntheticJev makes handler the model Jev asks. Replacing a judge
// withdraws organization grants, as the instance does.
func configureSyntheticJev(t *testing.T, app *Service, handler http.HandlerFunc) {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	previous := http.DefaultTransport
	http.DefaultTransport = server.Client().Transport
	t.Cleanup(func() {
		app.Close()
		http.DefaultTransport = previous
	})
	key := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(key, []byte("synthetic-key"), 0600); err != nil {
		t.Fatal(err)
	}
	inst := instanceOf(t, app)
	inst.config = decision.Config{Enabled: true, Model: "jev-1.13.0", Endpoint: server.URL + "/v1/systemone", SecretFile: key}
	installJudge(t, app, inst)
}

// installJudge opens the configured judge on a ledger of its own, as an
// instance does when its AI configuration is installed.
func installJudge(t testing.TB, app *Service, inst *testInstance) {
	t.Helper()
	ledger, err := decision.OpenLedger(app.db)
	if err != nil {
		t.Fatal(err)
	}
	judge, err := decision.New(inst.config, ledger, false)
	if err != nil {
		t.Fatal(err)
	}
	// Jev's runs stop before the judge they use closes.
	t.Cleanup(func() {
		app.Close()
		judge.Close()
		ledger.Close()
	})
	inst.ledger = ledger
	unlock := app.OrganizationGuard()
	defer unlock()
	if inst.judge.Swap(judge) != nil {
		if err = app.InvalidateOrganizationJobs(); err != nil {
			t.Fatal(err)
		}
	}
}

// usageLedger is the usage ledger of the configured judge.
func usageLedger(t testing.TB, app *Service) *decision.Ledger {
	t.Helper()
	return instanceOf(t, app).ledger
}

// reviewFixture opens Jev with handler as its model, for an instance where
// alice owns the default notebook with two notes and has consented.
func reviewFixture(t *testing.T, handler http.HandlerFunc) *Service {
	app := chatFixture(t)
	if err := app.catalog.Member(catalog.Bootstrap, "default", "alice", "owner"); err != nil {
		t.Fatal(err)
	}
	configureSyntheticJev(t, app, handler)
	store, _ := app.catalog.Access("alice", "default", "edit")
	for path, text := range map[string]string{"a.md": "# Workshop\nThe workshop opens at 09:00 on weekdays.", "b.md": "# Workshop\nThe workshop opens at 11:00 on weekdays."} {
		if _, err := store.Save(context.Background(), path, []byte(text), "", "alice"); err != nil {
			t.Fatal(err)
		}
	}
	if w := proxyCall(app, "alice", "POST", "/api/jev/settings", `{"consent":true,"rerank":true,"citations":true}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	return app
}

// reopen closes app and opens Jev again on the same catalog and database, with
// the same model, as a restarted instance does.
func reopen(t *testing.T, app *Service) *Service {
	t.Helper()
	inst := instanceOf(t, app)
	app.Close()
	next := &testInstance{data: inst.data, config: inst.config}
	reopened := openJev(t, app.catalog, app.db, app.indexes, next)
	installJudge(t, reopened, next)
	return reopened
}

// gatewayOf opens Jev as a second gateway of app's instance, with the same
// model.
func gatewayOf(t *testing.T, app *Service) *Service {
	t.Helper()
	db, leave := pgtest.Gateway(t, app.db)
	t.Cleanup(leave)
	inst := instanceOf(t, app)
	next := &testInstance{data: inst.data, config: inst.config}
	other := openJev(t, app.catalog, db, app.indexes, next)
	if inst.judge.Load() != nil {
		installJudge(t, other, next)
	}
	return other
}

// proxyCall makes a JSON request to app's routes as user.
func proxyCall(app *Service, user, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://localhost"+path, strings.NewReader(body))
	r.Header.Set(testUserHeader, user)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	inst, _ := instances.Load(app)
	inst.(*testInstance).mux.ServeHTTP(w, r)
	return w
}

func startReview(t *testing.T, app *Service, body string) string {
	t.Helper()
	w := proxyCall(app, "alice", "POST", "/p/default/api/jev/jobs", body)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var value struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &value); err != nil || value.ID == "" {
		t.Fatal(w.Body.String())
	}
	return value.ID
}

func waitReview(t *testing.T, app *Service, id string) map[string]any {
	t.Helper()
	// Each status read validates real Git evidence, competing with the worker
	// for the notebook lock. These are completion tests, not latency targets.
	deadline := time.Now().Add(30 * time.Second)
	var value map[string]any
	for time.Now().Before(deadline) {
		w := proxyCall(app, "alice", "GET", "/p/default/api/jev/jobs/"+id, "")
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		if err := json.Unmarshal(w.Body.Bytes(), &value); err != nil {
			t.Fatal(err)
		}
		if value["state"] != "running" && value["state"] != "queued" {
			return value
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("job did not settle: state=%v examined=%v total=%v message=%v", value["state"], value["examined"], value["total"], value["message"])
	return nil
}

func syntheticReviewAnswer(w http.ResponseWriter, r *http.Request) {
	var request struct {
		State struct {
			Evidence []any `json:"evidence"`
		} `json:"state"`
		Questions map[string]decision.Question `json:"questions"`
	}
	json.NewDecoder(r.Body).Decode(&request)
	answers := map[string]any{}
	for id, q := range request.Questions {
		choices := q.Criteria.(map[string]any)
		selected := "compatible"
		if _, ok := choices["contradiction"]; ok && len(request.State.Evidence) > 1 {
			selected = "contradiction"
		}
		if _, ok := choices["preserved"]; ok {
			selected = "partial"
		}
		probabilities := map[string]float64{}
		for key := range choices {
			probabilities[key] = 0
		}
		probabilities[selected] = 1
		answers[id] = map[string]any{"type": "choice", "choice": selected, "confidence": 1, "probabilities": probabilities}
	}
	json.NewEncoder(w).Encode(map[string]any{"model": "jev-1.13.0", "answers": answers, "usage": map[string]int{"input_tokens": 60, "output_tokens": 20}})
}

func syntheticOrganizationAnswer(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Questions map[string]decision.Question `json:"questions"`
	}
	json.NewDecoder(r.Body).Decode(&request)
	answers := map[string]any{}
	for id, q := range request.Questions {
		if q.Type == "noul" {
			score := .02
			if id == "tag0" {
				score = .95
			}
			answers[id] = map[string]any{"type": "noul", "noul": score}
			continue
		}
		probabilities := map[string]float64{}
		for key := range q.Criteria.(map[string]any) {
			probabilities[key] = 0
		}
		probabilities["related"] = 1
		answers[id] = map[string]any{"type": "choice", "choice": "related", "confidence": 1, "probabilities": probabilities}
	}
	json.NewEncoder(w).Encode(map[string]any{"model": "jev-1.13.0", "answers": answers, "usage": map[string]int{"input_tokens": 60, "output_tokens": 20}})
}

// fixtureAuthor signs fixture commits made beneath the workspace.
var fixtureAuthor = gitobj.Signature{Name: "Fixture", Email: "fixture@example.invalid", When: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC).Unix(), TZ: "+0000"}

// commitElsewhere commits cs to store's notebook through the note store, as
// another gateway would, and waits until store serves it.
func commitElsewhere(t testing.TB, app *Service, store *workspace.Store, cs notestore.ChangeSet) string {
	t.Helper()
	ctx := context.Background()
	inst := instanceOf(t, app)
	blobs, err := blob.OpenDir(inst.data)
	if err != nil {
		t.Fatal(err)
	}
	notes, err := notestore.Open(ctx, pgtest.Dir(t, inst.data), blobs, notestore.Options{ImportEntryID: workspace.ImportEntryID})
	if err != nil {
		blobs.Close()
		t.Fatal(err)
	}
	defer func() {
		notes.Close()
		blobs.Close()
	}()
	notebook := ""
	for _, p := range app.catalog.List(catalog.Bootstrap) {
		if s, err := app.catalog.Access(catalog.Bootstrap, p.ID, "read"); err == nil && s == store {
			notebook = p.ID
		}
	}
	if notebook == "" {
		t.Fatal("the store is no notebook of the catalog")
	}
	result, err := notes.Notebook(notebook).Apply(ctx, cs)
	if err != nil {
		t.Fatal(err)
	}
	revision := result.Head.Commit.String()
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		status, err := store.Status(ctx)
		if err == nil && status.Revision == revision {
			return revision
		}
		if time.Now().After(deadline) {
			t.Fatalf("the store did not serve revision %s committed elsewhere: %+v %v", revision, status, err)
		}
	}
}

type lockedLogBuffer struct {
	mu sync.Mutex
	bytes.Buffer
}

func (b *lockedLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.Write(p)
}
func (b *lockedLogBuffer) String() string { b.mu.Lock(); defer b.mu.Unlock(); return b.Buffer.String() }
