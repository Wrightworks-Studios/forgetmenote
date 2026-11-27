package web

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/connectors"
	"forgetmenote/internal/decision"
	"forgetmenote/internal/modules"
	"forgetmenote/internal/pg/pgtest"
	"forgetmenote/internal/workspace"
)

// moduleScopes lets a test client list and call every tool.
const moduleScopes = "notes:read notes:write notes:manage data:read"

// moduleRoute is a route a module registers.
type moduleRoute struct {
	pattern string
	project bool
}

// declaredModule reads what a wired module declares: the actions its tool
// groups add, aliases included, and the routes it registers. Neither changes
// app.
func declaredModule(t *testing.T, app *Server, module wiredModule) ([]string, []moduleRoute) {
	t.Helper()
	probe := &Server{}
	server := mcp.NewServer(&mcp.Implementation{Name: "probe", Version: "1"}, nil)
	for _, tools := range module.tools {
		tools(probe, server)
	}
	var actions []string
	for name := range probe.actions {
		actions = append(actions, name)
	}
	slices.Sort(actions)
	var routes []moduleRoute
	record := routeRegistrar{
		project: func(pattern, _ string, _ http.HandlerFunc) {
			routes = append(routes, moduleRoute{pattern: pattern, project: true})
		},
		global: func(pattern string, _ func(http.ResponseWriter, *http.Request)) {
			routes = append(routes, moduleRoute{pattern: pattern})
		},
	}
	for _, register := range module.routes {
		register(app, record)
	}
	return actions, routes
}

var routeWildcard = regexp.MustCompile(`\{[^}]*\}`)

// request is a request the route's pattern matches, for the given server:
// each wildcard is "x", and a project route is in the default notebook.
func (route moduleRoute) request(app *Server) *http.Request {
	method, path, ok := strings.Cut(route.pattern, " ")
	if !ok {
		method, path = http.MethodGet, route.pattern
	}
	path = strings.TrimSuffix(routeWildcard.ReplaceAllString(path, "x"), "{$}")
	if route.project {
		path = "/p/default" + path
	}
	r := httptest.NewRequest(method, app.origin+path, strings.NewReader("{}"))
	r.Header.Set("Authorization", "Bearer "+testToken)
	r.Header.Set("Content-Type", "application/json")
	return r
}

// registered reports whether the server routes the request to the pattern.
func (route moduleRoute) registered(app *Server) bool {
	r := route.request(app)
	mux := app.mux
	if route.project {
		mux = app.routes.Mux
		r.URL.Path = strings.TrimPrefix(r.URL.Path, "/p/default")
	}
	_, pattern := mux.Handler(r)
	return pattern == route.pattern
}

func listedTools(t *testing.T, session *mcp.ClientSession) map[string]bool {
	t.Helper()
	listed, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tool := range listed.Tools {
		names[tool.Name] = true
	}
	return names
}

// startedModuleServer is fixture for an instance that runs every module but
// off. It is given every module's settings, as an operator would, and then
// started as the runtime starts it.
func startedModuleServer(t *testing.T, off ...string) *Server {
	t.Helper()
	set, err := modules.Parse(off)
	if err != nil {
		t.Fatal(err)
	}
	app, _ := fixtureWithModules(t, set)
	for _, module := range wiredModules {
		if configure := offModuleChecks[module.name].configure; configure != nil {
			configure(t, app)
		}
	}
	if err := app.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	return app
}

// heldModuleDuties waits as long as a started instance takes to claim its
// duties, then reports which of names it holds.
func heldModuleDuties(t *testing.T, app *Server, names []string) []string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for held := heldDuties(t, app); !held["chat-recovery"]; held = heldDuties(t, app) {
		if time.Now().After(deadline) {
			t.Fatalf("the instance took no duties: %v", held)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Every duty starts claiming at once; give the module's as long again.
	time.Sleep(time.Second)
	held := heldDuties(t, app)
	var out []string
	for _, name := range names {
		if held[name] {
			out = append(out, name)
		}
	}
	return out
}

// moduleChecks are what the absence test cannot read from a module's wiring.
type moduleChecks struct {
	// duties are the leases the module's workers hold while it runs.
	duties []string
	// configure gives the module its settings, as an operator would, before
	// the instance starts; installed reports whether they took effect, which
	// they do only while the module runs.
	configure func(t *testing.T, app *Server)
	installed func(app *Server) bool
	// running reports whether the module's hub and its workers are open.
	running func(app *Server) bool
	// cleanup proves that a purge, account deletion or access removal still
	// reaches the module's stored data while it is off.
	cleanup func(t *testing.T, app *Server)
}

var offModuleChecks = map[string]moduleChecks{
	"sources": {
		running: func(app *Server) bool { return app.sourceJobs != nil && app.sourceHTTP != nil },
		cleanup: func(t *testing.T, app *Server) {
			// A gateway that ran the module took a source job and is gone.
			// An instance with the module off restarts well within the job's
			// 24 hours: it runs no source work, so nothing could resume the
			// job, and it retires the job and its files at once rather than
			// keep a private upload past its limit.
			db, leave := pgtest.Gateway(t, app.db)
			on, err := NewWithBlobs(app.catalog, db, app.blobs, modules.Set{}, t.TempDir(), app.origin, app.token)
			if err != nil {
				t.Fatal(err)
			}
			input, _ := json.Marshal(map[string]any{"filename": "notes.txt", "data": base64.StdEncoding.EncodeToString([]byte("Cobalt"))})
			created := call(on, "POST", "/api/sources/jobs", string(input), testToken)
			var out struct {
				Job struct {
					ID string `json:"id"`
				} `json:"job"`
			}
			if created.Code != 200 || json.Unmarshal(created.Body.Bytes(), &out) != nil || out.Job.ID == "" {
				t.Fatalf("source job: HTTP %d %s", created.Code, created.Body)
			}
			on.Close()
			leave()
			jobs := func() (count int) {
				if err := app.db.Pool.QueryRow(t.Context(), `SELECT count(*) FROM source_jobs WHERE org = $1 AND id = $2`, app.db.Org, out.Job.ID).Scan(&count); err != nil {
					t.Fatal(err)
				}
				return count
			}
			if stored, err := app.blobs.List(t.Context(), "source-jobs/"+out.Job.ID+"/"); jobs() != 1 || err != nil || len(stored) == 0 {
				t.Fatalf("the job was not stored: %v %v", stored, err)
			}
			restarted, leaveAgain := pgtest.Gateway(t, app.db)
			defer leaveAgain()
			off, err := NewWithBlobs(app.catalog, restarted, app.blobs, app.modules, t.TempDir(), app.origin, app.token)
			if err != nil {
				t.Fatal(err)
			}
			defer off.Close()
			if off.sourceJobs != nil || jobs() != 0 {
				t.Fatalf("with the module off, the job stays (%d rows) or source work runs", jobs())
			}
			objects, err := app.blobs.List(t.Context(), "source-jobs/"+out.Job.ID+"/")
			if err != nil || len(objects) != 0 {
				t.Fatalf("the job's files stay: %v %v", objects, err)
			}
		},
	},
	"data-connections": {
		configure: func(t *testing.T, app *Server) {
			input := json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)
			output := json.RawMessage(`{"type":"object","properties":{"value":{"type":"integer"}},"required":["value"],"additionalProperties":false}`)
			if err := app.ConfigureDataConnectionsConfig(connectors.Config{Version: 1, Connections: []connectors.Connection{{
				ID: "metrics", Kind: "http", Endpoint: "http://127.0.0.1:1",
				Operations: []connectors.Operation{{Name: "read", Method: "GET", Path: "/", ReadOnly: true, InputSchema: input, OutputSchema: output}},
			}}}); err != nil {
				t.Fatal(err)
			}
		},
		installed: func(app *Server) bool { return app.dataConnections != nil },
		cleanup: func(t *testing.T, app *Server) {
			// A grant made while the module ran is to a group that is then
			// deleted with the module off.
			if _, err := app.catalog.SetGroup(catalog.Bootstrap, catalog.Group{ID: "analysts", Name: "Analysts"}); err != nil {
				t.Fatal(err)
			}
			if _, err := app.catalog.SetConnectionAccess(catalog.Bootstrap, catalog.ConnectionGrant{Connection: "metrics", Groups: map[string][]string{"analysts": {"read"}}}); err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest("POST", app.origin+"/api/v1/actions/delete_group", strings.NewReader(`{"id":"analysts","revision":1}`))
			r.Header.Set("Authorization", "Bearer "+testToken)
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			app.ServeHTTP(w, r)
			if w.Code != 200 {
				t.Fatalf("delete_group: HTTP %d %s", w.Code, w.Body)
			}
			grant, err := app.catalog.ConnectionAccess(catalog.Bootstrap, "metrics")
			if err != nil || len(grant.Groups) != 0 {
				t.Fatalf("the deleted group keeps its grant: %+v %v", grant, err)
			}
		},
	},
	"judgments": {
		duties: []string{"jev-scheduler"},
		configure: func(t *testing.T, app *Server) {
			key := filepath.Join(t.TempDir(), "key")
			if err := os.WriteFile(key, []byte("synthetic-key"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := app.ConfigureAIConfig(nil, &decision.Config{Enabled: true, Model: "jev-1.13.0", Endpoint: "https://127.0.0.1:1/v1/systemone", SecretFile: key}); err != nil {
				t.Fatal(err)
			}
		},
		installed: func(app *Server) bool {
			judge, _ := app.judgmentsConfigured()
			return judge != nil
		},
		cleanup: func(t *testing.T, app *Server) {
			// A judgment record written while the module ran quotes a note
			// that is then purged with the module off.
			store, err := app.catalog.Access(catalog.Bootstrap, "default", "edit")
			if err != nil {
				t.Fatal(err)
			}
			secret := webPurgeSecret()
			savePurgeNote(t, store, "notes/keys.md", "# Keys\n\nThe staging key is "+secret+".\n", "")
			if err := app.judgments.Records().Write("jobs/"+strings.Repeat("e", 64)+".json", map[string]any{"version": 2, "project": "default", "message": "Found:\nThe staging key is " + secret + "."}); err != nil {
				t.Fatal(err)
			}
			plan := preparePurgeHTTP(t, app, workspace.PurgeScope{Kind: workspace.PurgeText, Text: secret})
			if plan.Derived.JudgmentRecords != 1 {
				t.Fatalf("the plan does not reach the judgment record: %+v", plan.Derived)
			}
			if code, receipt, body := executePurgeHTTP(app, plan, "Leaked staging key"); code != 200 || receipt.Derived.JudgmentRecords != 1 {
				t.Fatalf("execute: HTTP %d %s", code, body)
			}
			if _, judgments, _ := holdingPurgeSecret(t, app, secret); judgments != 0 {
				t.Fatalf("%d judgment records still hold the purged text", judgments)
			}
		},
	},
}

// Every module that can be turned off has its wiring and its checks here, and
// no module that cannot be turned off claims wiring.
func TestEveryModuleThatCanBeTurnedOffIsWired(t *testing.T) {
	wired := map[string]bool{}
	for _, module := range wiredModules {
		if wired[module.name] {
			t.Fatalf("%s is wired twice", module.name)
		}
		wired[module.name] = true
		if _, ok := offModuleChecks[module.name]; !ok {
			t.Errorf("%s has no absence checks", module.name)
		}
	}
	for _, module := range modules.All() {
		_, err := modules.Parse([]string{module.Name})
		if (err == nil) != wired[module.Name] {
			t.Errorf("%s: wired=%v, but turning it off alone gives %v", module.Name, wired[module.Name], err)
		}
	}
}

// A module that is off is absent wherever a person or an agent could reach
// it: none of its actions is in the registry that /api/v1, chat and
// automations read, or in MCP's tools/list, a call by name is refused as
// unknown, its routes answer 404 like any unknown path, its settings are not
// installed and its workers take no duty. What it stored stays, and cleanup
// still reaches it. With every module on, each of those is there, and turning
// one module off changes nothing else.
func TestAnOffModuleIsAbsentEverywhere(t *testing.T) {
	full := startedModuleServer(t)
	origin := full.origin
	fullListed := listedTools(t, actionClient(t, full, catalog.Bootstrap, moduleScopes))
	full.origin = origin
	for _, module := range wiredModules {
		t.Run(module.name, func(t *testing.T) {
			checks := offModuleChecks[module.name]
			actions, routes := declaredModule(t, full, module)
			if len(actions)+len(routes) == 0 {
				t.Fatal("the module declares neither tools nor routes")
			}
			for _, name := range actions {
				if _, ok := full.actions[name]; !ok || !fullListed[name] {
					t.Errorf("with every module on, %s is missing: registered=%v listed=%v", name, ok, fullListed[name])
				}
			}
			for _, route := range routes {
				if !route.registered(full) {
					t.Errorf("with every module on, %s is not routed", route.pattern)
				}
			}

			if checks.installed != nil && !checks.installed(full) {
				t.Error("with every module on, its settings are not installed")
			}
			if checks.running != nil && !checks.running(full) {
				t.Error("with every module on, its hub is not open")
			}

			app := startedModuleServer(t, module.name)
			if checks.installed != nil && checks.installed(app) {
				t.Error("the off module's settings were installed")
			}
			if checks.running != nil && checks.running(app) {
				t.Error("the off module's hub is open")
			}
			// The MCP client serves the instance at another origin; the
			// requests below use the instance's own.
			origin := app.origin
			listed := listedTools(t, actionClient(t, app, catalog.Bootstrap, moduleScopes))
			app.origin = origin
			for _, name := range actions {
				if _, ok := app.actions[name]; ok || listed[name] {
					t.Errorf("%s is still offered: registered=%v listed=%v", name, ok, listed[name])
				}
				r := httptest.NewRequest("POST", app.origin+"/api/v1/actions/"+name, strings.NewReader("{}"))
				r.Header.Set("Authorization", "Bearer "+testToken)
				r.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				app.ServeHTTP(w, r)
				if w.Code != 404 || !strings.Contains(w.Body.String(), "Action not found") {
					t.Errorf("calling %s by name: HTTP %d %s", name, w.Code, w.Body)
				}
			}
			for name := range full.actions {
				if _, ok := app.actions[name]; !ok && !slices.Contains(actions, name) {
					t.Errorf("turning %s off removed %s, which another module or the core adds", module.name, name)
				}
			}
			for _, route := range routes {
				w := httptest.NewRecorder()
				app.ServeHTTP(w, route.request(app))
				if w.Code != 404 || route.registered(app) {
					t.Errorf("%s: HTTP %d %s", route.pattern, w.Code, w.Body)
				}
			}
			if held := heldModuleDuties(t, app, checks.duties); len(held) > 0 {
				t.Errorf("workers of the off module took %v", held)
			}
			var me struct {
				ModulesOff []string `json:"modulesOff"`
			}
			w := httptest.NewRecorder()
			r := httptest.NewRequest("GET", app.origin+"/api/me", nil)
			r.Header.Set("Authorization", "Bearer "+testToken)
			app.ServeHTTP(w, r)
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &me) != nil || !slices.Equal(me.ModulesOff, []string{module.name}) {
				t.Errorf("/api/me: HTTP %d %s", w.Code, w.Body)
			}
			if checks.cleanup != nil {
				checks.cleanup(t, app)
			}
		})
	}
	// With every module on, the workers of each take their duties.
	var duties []string
	for _, module := range wiredModules {
		duties = append(duties, offModuleChecks[module.name].duties...)
	}
	if held := heldModuleDuties(t, full, duties); !slices.Equal(held, duties) {
		t.Errorf("with every module on, the instance holds %v of %v", held, duties)
	}
}

// With data connections off, a person's saved data watches stay as they are.
// Saving the rest of their assistant settings keeps them; changing them
// answers that data connections are off.
func TestSavedWatchesStayWithDataConnectionsOff(t *testing.T) {
	set, err := modules.Parse([]string{"data-connections"})
	if err != nil {
		t.Fatal(err)
	}
	app, _ := fixtureWithModules(t, set)
	admin := recoverySession(t, app)
	alice := addLocal(t, app, admin, "watch-alice")
	login := oidcRequest(app, "POST", "/auth/password/login", url.Values{"username": {"watch-alice"}, "password": {"test-password-long"}}.Encode())
	if login.Code != http.StatusSeeOther {
		t.Fatalf("login: %d %s", login.Code, login.Body)
	}
	cookie := login.Result().Cookies()[0]
	if w := localRequest(app, "/api/organization/action/set_assistant_policy", `{"revision":0,"enabled":true}`, admin); w.Code != 200 {
		t.Fatalf("assistant policy: %d %s", w.Code, w.Body)
	}
	save := func(settings catalog.AssistantSettings) *httptest.ResponseRecorder {
		body, _ := json.Marshal(settings)
		return localRequest(app, "/api/assistant/action/save_assistant_settings", string(body), cookie)
	}
	if w := save(catalog.AssistantSettings{Enabled: true, ExternalProcessing: true, TimeZone: "UTC", Features: []string{"operations"}}); w.Code != 200 {
		t.Fatalf("assistant setup: %d %s", w.Code, w.Body)
	}
	settings, err := app.catalog.Assistant(alice)
	if err != nil {
		t.Fatal(err)
	}
	// Saved while the module ran.
	settings.Watches = []catalog.AssistantWatch{{Connection: "metrics", Operation: "read", Arguments: json.RawMessage(`{}`)}}
	if settings, err = app.catalog.SaveAssistant(alice, settings); err != nil {
		t.Fatal(err)
	}
	changed := settings
	changed.Watches = []catalog.AssistantWatch{{Connection: "metrics", Operation: "other", Arguments: json.RawMessage(`{}`)}}
	if w := save(changed); w.Code != 400 || !strings.Contains(w.Body.String(), "data connections are turned off") {
		t.Fatalf("a watch changed with data connections off: %d %s", w.Code, w.Body)
	}
	settings.QuietStart, settings.QuietEnd = "22:00", "07:00"
	if w := save(settings); w.Code != 200 {
		t.Fatalf("a saved watch stops the person saving: %d %s", w.Code, w.Body)
	}
	stored, err := app.catalog.Assistant(alice)
	if err != nil || stored.QuietStart != "22:00" || len(stored.Watches) != 1 || stored.Watches[0].Operation != "read" {
		t.Fatalf("saved settings: %+v %v", stored, err)
	}
}
