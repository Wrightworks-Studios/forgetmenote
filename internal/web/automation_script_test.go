package web

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"forgetmenote/internal/automation"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/connectors"
	"forgetmenote/internal/sandbox"
)

func scriptFixture(t *testing.T, code string, caps []string, connections map[string][]string, notebooks map[string]string) (*Server, *scriptExecution) {
	t.Helper()
	m := automation.Manifest{APIVersion: automation.PackageAPI, ID: "fixture", Name: "Fixture", Entrypoint: "main.py", Runtime: automation.PythonRuntime, Files: []string{"main.py"}, Capabilities: caps, Connections: connections, Notebooks: notebooks}
	raw, _ := json.Marshal(m)
	return packageFixture(t, m, map[string]string{"automation.json": string(raw), "main.py": code})
}
func packageFixture(t *testing.T, m automation.Manifest, files map[string]string, otherPackages ...map[string]string) (*Server, *scriptExecution) {
	t.Helper()
	return packageFixtureWith(t, m, files, nil, otherPackages...)
}

// packageFixtureWith lets a test change the job definition before it is saved
// and the run's contract is pinned.
func packageFixtureWith(t *testing.T, m automation.Manifest, files map[string]string, adjust func(*automation.Spec), otherPackages ...map[string]string) (*Server, *scriptExecution) {
	t.Helper()
	app, _ := fixture(t)
	sourceFiles := map[string]string{}
	for name, content := range files {
		sourceFiles["package/"+name] = content
	}
	configureSearchSource(t, app, append([]map[string]string{sourceFiles}, otherPackages...)...)
	caps, notebooks := m.Capabilities, m.Notebooks
	admin := recoverySession(t, app)
	steward := addLocal(t, app, admin, "package-owner")
	bot, err := app.catalog.CreateService(catalog.Bootstrap, "Package worker", steward)
	if err != nil {
		t.Fatal(err)
	}
	if err = app.catalog.Member(catalog.Bootstrap, "default", bot.ID, "edit"); err != nil {
		t.Fatal(err)
	}
	info, _, err := app.automationPackage(t.Context(), bot.ID, packageQuery{Project: "default", Path: "package"}, true)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &automation.Script{SourceProject: "default", SourceID: info.Source, PackagePath: "package", Revision: info.Revision, Digest: info.Digest, Runtime: automation.PythonRuntime, Entrypoint: m.Entrypoint, Capabilities: caps, Connections: map[string]string{}, Notebooks: map[string]string{}}
	for k := range notebooks {
		cfg.Notebooks[k] = "default"
	}
	// Script tests run the production default, the sandbox, wherever the kernel
	// enforces it, and the explicit opt-out elsewhere, such as macOS development.
	if sandbox.Available() != nil {
		cfg.Sandbox = new(false)
	}
	spec := automation.Spec{ID: "script-fixture", Kind: "script", Actor: bot.ID, Project: "default", Script: cfg, TimeoutSeconds: 20, ExternalProcessing: true}
	if adjust != nil {
		adjust(&spec)
	}
	if err = app.ConfigureAutomationPython("python3"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = filepath.WalkDir(filepath.Join(app.data, "automations", "python"), func(path string, d fs.DirEntry, err error) error {
			if err == nil && d.IsDir() {
				return os.Chmod(path, 0700)
			}
			return err
		})
	})
	cfg.Environment, err = app.python.Prepare(t.Context(), files, info.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = app.automations.Definitions.Save(spec, ""); err != nil {
		t.Fatal(err)
	}
	ctx, err := app.automationContext(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	contract, err := app.scriptContract(spec)
	if err != nil {
		t.Fatal(err)
	}
	run := &automation.Run{ID: automation.Hash(t.Name()), Spec: spec, Created: time.Now().UTC(), Script: &automation.ScriptCheckpoint{Digest: info.Digest, Revision: info.Revision, Environment: cfg.Environment, Contract: contract, Steps: map[string]*automation.ScriptStep{}}}
	file := filepath.Join(t.TempDir(), "checkpoint.json")
	e := &scriptExecution{ctx: ctx, run: run, info: info, save: func() error {
		b, err := json.Marshal(run)
		if err != nil {
			return err
		}
		return os.WriteFile(file, b, 0600)
	}}
	return app, e
}
func hostStep(t *testing.T, app *Server, e *scriptExecution, id, op string, args any) (any, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return app.scriptStep(e, scriptRequest{Step: id, Operation: op, Arguments: raw})
}
func TestScriptHostPermissionAndCheckpointBoundary(t *testing.T) {
	app, e := scriptFixture(t, "def main(host): return {}", []string{"checkpoint", "notebooks.read"}, nil, map[string]string{"notes": "read"})
	if _, err := hostStep(t, app, e, "bad", "web.fetch", map[string]any{"url": "https://example.org"}); err == nil {
		t.Fatal("ungranted capability accepted")
	}
	if _, err := hostStep(t, app, e, "bad", "notebooks.read", map[string]string{"notebook": "other", "path": "note.md"}); err == nil {
		t.Fatal("unbound notebook accepted")
	}
	if _, err := hostStep(t, app, e, "one", "checkpoint.set", map[string]any{"state": map[string]string{"completed": "case"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := hostStep(t, app, e, "one", "checkpoint.set", map[string]any{"state": map[string]string{"completed": "changed"}}); err == nil {
		t.Fatal("step input changed")
	}
	if err := app.catalog.Member(catalog.Bootstrap, "default", e.run.Spec.Actor, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := hostStep(t, app, e, "one", "checkpoint.set", map[string]any{"state": map[string]string{"completed": "case"}}); err == nil {
		t.Fatal("revoked authority received retained result")
	}
}
func TestScriptPackageRejectsChangedRevisionAndExclusions(t *testing.T) {
	app, e := scriptFixture(t, "def main(host): return {}", []string{"checkpoint"}, nil, nil)
	altered := e.run.Spec
	copy := *altered.Script
	copy.Revision = strings.Repeat("b", 40)
	altered.Script = &copy
	if _, _, err := app.scriptPackage(t.Context(), altered, false); err == nil {
		t.Fatal("unavailable commit accepted")
	}
	for _, p := range []string{"../package", "/package", "package/../package", "package\\file"} {
		if _, _, err := app.automationPackage(t.Context(), e.run.Spec.Actor, packageQuery{Project: "default", Path: p}, false); err == nil {
			t.Fatal("unsafe path", p)
		}
	}
	state, _ := app.repositories.Status("default")
	cfg := state.IndexConfig
	cfg.Exclude = []string{"package/main.py"}
	if _, err := app.repositories.ConfigureIndex("default", catalog.Bootstrap, state.Generation, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := hostStep(t, app, e, "state", "checkpoint.get", map[string]any{}); err == nil {
		t.Fatal("excluded source still executing")
	}
}

func TestScriptPackageAdministrationRequiresExplicitPinnedInstall(t *testing.T) {
	app, e := scriptFixture(t, "def main(host): return {}", []string{"checkpoint"}, nil, nil)
	admin := recoverySession(t, app)
	owner := addLocal(t, app, admin, "notebook-owner")
	if err := app.catalog.Member(catalog.Bootstrap, "default", owner, "owner"); err != nil {
		t.Fatal(err)
	}
	login := oidcRequest(app, "POST", "/auth/password/login", url.Values{"username": {"notebook-owner"}, "password": {"test-password-long"}}.Encode())
	if login.Code != 303 {
		t.Fatal(login.Code, login.Body.String())
	}
	// Discovery names the notebook it searches, which this owner may read.
	for action, body := range map[string]string{"discover_automation_packages": `{"project":"default"}`, "prepare_automation_package": `{}`, "install_automation_package": `{}`, "save_automation": `{}`} {
		w := localRequest(app, "/api/organization/action/"+action, body, login.Result().Cookies()[0])
		if w.Code != 403 {
			t.Fatalf("notebook owner could %s: HTTP %d %s", action, w.Code, w.Body.String())
		}
	}
	w := localRequest(app, "/api/organization/action/discover_automation_packages", `{"project":"default"}`, admin)
	if w.Code != 200 || !strings.Contains(w.Body.String(), e.run.Spec.Script.Revision) {
		t.Fatal(w.Code, w.Body.String())
	}
	spec := e.run.Spec
	spec.ID = "installed-package"
	spec.Disabled = false
	request := func(action, revision string) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(map[string]any{"spec": spec, "revision": revision})
		return localRequest(app, "/api/organization/action/"+action, string(raw), admin)
	}
	w = request("install_automation_package", "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var saved automation.Definition
	if err := json.Unmarshal(w.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	if !saved.Spec.Disabled {
		t.Fatal("install implicitly activated executable code")
	}
	if w = request("install_automation_package", ""); w.Code != 409 {
		t.Fatal("stale installation update", w.Code, w.Body.String())
	}
	if w = request("save_automation", saved.Revision); w.Code != 200 {
		t.Fatal("explicit activation failed", w.Code, w.Body.String())
	}
	active, _ := app.automations.Definitions.Get(spec.ID)
	if active.Spec.Disabled {
		t.Fatal("explicit activation did not enable the pinned definition")
	}
	copy := *spec.Script
	copy.Digest = strings.Repeat("d", 64)
	spec.Script = &copy
	if w = request("validate_automation", active.Revision); w.Code == 200 {
		t.Fatal("validation accepted a changed package digest")
	}
	if w = request("install_automation_package", active.Revision); w.Code == 200 {
		t.Fatal("installation accepted a changed package digest")
	}
	unchanged, _ := app.automations.Definitions.Get(spec.ID)
	if unchanged.Revision != active.Revision {
		t.Fatal("rejected update changed the active definition")
	}
}

func TestScriptFailedPreparationKeepsActiveInstallation(t *testing.T) {
	m := automation.Manifest{APIVersion: automation.PackageAPI, ID: "fixture", Name: "Fixture", Entrypoint: "main.py", Runtime: automation.PythonRuntime, Files: []string{"main.py"}}
	raw, _ := json.Marshal(m)
	files := map[string]string{"automation.json": string(raw), "main.py": "def main(host): return 'active'"}
	broken := map[string]string{"broken/automation.json": string(raw), "broken/main.py": "def main(host): return ("}
	app, e := packageFixture(t, m, files, broken)
	admin := recoverySession(t, app)
	active, _ := app.automations.Definitions.Get(e.run.Spec.ID)
	info, _, err := app.automationPackage(t.Context(), e.run.Spec.Actor, packageQuery{Project: "default", Path: "broken"}, false)
	if err != nil {
		t.Fatal(err)
	}
	spec := active.Spec
	cfg := *spec.Script
	spec.Script = &cfg
	cfg.PackagePath, cfg.Digest = info.Path, info.Digest
	request, _ := json.Marshal(map[string]any{"spec": spec, "revision": active.Revision})
	w := localRequest(app, "/api/organization/action/install_automation_package", string(request), admin)
	if w.Code == 200 || !strings.Contains(w.Body.String(), "SyntaxError") {
		t.Fatalf("preparation failure not reported: HTTP %d %s", w.Code, w.Body.String())
	}
	current, _ := app.automations.Definitions.Get(spec.ID)
	if current.Revision != active.Revision || current.Spec.Disabled {
		t.Fatal("failed preparation replaced or paused active installation")
	}
	if err = app.runAutomation(t.Context(), e.run, e.save); err != nil {
		t.Fatalf("original installation stopped working: %v", err)
	}
}

func TestScriptSubmissionRechecksPermissionAfterFreshRead(t *testing.T) {
	app, e := scriptFixture(t, "def main(host): return {}", []string{"connectors.read", "connectors.submit"}, map[string][]string{"api": {"read", "submit"}}, nil)
	writes := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "GET" {
			grant, err := app.catalog.ConnectionAccess(catalog.Bootstrap, "fixture-api")
			if err != nil {
				t.Error(err)
			}
			grant.Users[e.run.Spec.Actor] = []string{"read"}
			if _, err = app.catalog.SetConnectionAccess(catalog.Bootstrap, grant); err != nil {
				t.Error(err)
			}
			fmt.Fprint(w, `{"revision":"one"}`)
		} else {
			writes++
			fmt.Fprint(w, `{"id":"stable"}`)
		}
	}))
	defer upstream.Close()
	bindScriptConnector(t, app, e, upstream.URL)
	input := map[string]any{"connection": "api", "operation": "submit", "arguments": map[string]any{"body": map[string]string{"submission_id": "stable"}}, "guard": map[string]any{"read": map[string]any{"connection": "api", "operation": "read", "arguments": map[string]any{}}, "equals": map[string]string{"/revision": "one"}, "expiresAt": time.Now().Add(time.Minute)}, "acknowledgment": map[string]string{"/id": "stable"}}
	if _, err := hostStep(t, app, e, "submit", "connectors.submit", input); err == nil {
		t.Fatal("permission revocation was ignored")
	}
	if writes != 0 {
		t.Fatal("submitted after write permission was revoked")
	}
}
func TestScriptUnknownOutcomeIsNotRepeated(t *testing.T) {
	app, e := scriptFixture(t, "def main(host): return {}", []string{"checkpoint", "notebooks.publish"}, nil, map[string]string{"notes": "edit"})
	e.run.Script.Steps["paid"] = &automation.ScriptStep{State: "started", Effect: true, Operation: "ai.structured", Input: json.RawMessage(`{"prompt":"original"}`)}
	if err := app.runScript(e.ctx, e.run, e.save); err == nil || !strings.Contains(err.Error(), "unknown outcome") {
		t.Fatal(err)
	}
	if _, err := hostStep(t, app, e, "different-id", "notebooks.publish", map[string]any{"notebook": "notes", "path": "unsafe-continuation.md", "content": "must not publish", "revision": ""}); err == nil || !strings.Contains(err.Error(), "unknown outcome") {
		t.Fatal("different step bypassed uncertain effect", err)
	}
	if _, err := hostStep(t, app, e, "finish", "run.complete", map[string]any{"result": "ignored error"}); err == nil {
		t.Fatal("completion bypassed pending paid call")
	}
}

func TestScriptRunsInServerWithoutExecutor(t *testing.T) {
	app, e := scriptFixture(t, "def main(host):\n    host.call('state', 'checkpoint.set', state={'direct': True})\n    return {'direct': True}\n", []string{"checkpoint"}, nil, nil)
	e.run.Script = nil
	if err := app.runScript(e.ctx, e.run, e.save); err != nil {
		t.Fatal(err)
	}
	if !e.run.Script.Complete || !strings.Contains(e.run.Result, "direct") {
		t.Fatal("missing direct-process result", e.run.Result)
	}
}

var scriptReadOperation = connectors.Operation{Name: "read", ReadOnly: true, Method: "GET", Path: "/case"}
var scriptSubmitOperation = connectors.Operation{Name: "submit", ReadOnly: false, Method: "POST", Path: "/submit", BodyArgument: "body", InputSchema: json.RawMessage(`{"type":"object","properties":{"body":{"type":"object"}},"required":["body"],"additionalProperties":false}`)}

func bindScriptConnector(t *testing.T, app *Server, e *scriptExecution, endpoint string) {
	t.Helper()
	bindScriptOperations(t, app, e, endpoint, scriptReadOperation, scriptSubmitOperation)
}

// bindScriptOperations binds the package's api alias to a connection with
// operations, each granted to the job and to its actor.
func bindScriptOperations(t *testing.T, app *Server, e *scriptExecution, endpoint string, operations ...connectors.Operation) {
	t.Helper()
	names := []string{}
	for _, op := range operations {
		names = append(names, op.Name)
	}
	c := connectors.Connection{ID: "fixture-api", Label: "Fixture API", Kind: "http", Endpoint: endpoint, ExternalProcessing: true, Operations: operations}
	raw, _ := json.Marshal(connectors.Config{Version: 1, Connections: []connectors.Connection{c}})
	file := filepath.Join(t.TempDir(), "connections.json")
	os.WriteFile(file, raw, 0600)
	if err := app.ConfigureDataConnections(file); err != nil {
		t.Fatal(err)
	}
	e.run.Spec.Connections = []string{c.ID}
	e.run.Spec.Operations = map[string][]string{c.ID: names}
	e.run.Spec.Script.Connections["api"] = c.ID
	grant, err := app.catalog.ConnectionAccess(catalog.Bootstrap, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	grant.Users = map[string][]string{e.run.Spec.Actor: names}
	if _, err = app.catalog.SetConnectionAccess(catalog.Bootstrap, grant); err != nil {
		t.Fatal(err)
	}

	e.run.Script.Contract, err = app.scriptContract(e.run.Spec)
	if err != nil {
		t.Fatal(err)
	}
	e.ctx, err = app.automationContext(t.Context(), e.run.Spec)
	if err != nil {
		t.Fatal(err)
	}
}
func TestScriptGuardedSubmissionAndRetainedAcknowledgment(t *testing.T) {
	app, e := scriptFixture(t, "def main(host): return {}", []string{"connectors.read", "connectors.submit"}, map[string][]string{"api": {"read", "submit"}}, nil)
	writes := 0
	revision := "one"
	badAck := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "GET" {
			fmt.Fprintf(w, `{"revision":%q}`, revision)
			return
		}
		writes++
		if badAck {
			fmt.Fprint(w, `{"id":"wrong"}`)
		} else {
			fmt.Fprint(w, `{"id":"stable"}`)
		}
	}))
	defer upstream.Close()
	bindScriptConnector(t, app, e, upstream.URL)
	input := map[string]any{"connection": "api", "operation": "submit", "arguments": map[string]any{"body": map[string]string{"submission_id": "stable"}}, "guard": map[string]any{"read": map[string]any{"connection": "api", "operation": "read", "arguments": map[string]any{}}, "equals": map[string]string{"/revision": "one"}, "expiresAt": time.Now().Add(time.Minute)}, "acknowledgment": map[string]string{"/id": "stable"}}
	e.run.DryRun = true
	if _, err := hostStep(t, app, e, "preview", "connectors.submit", input); err != nil {
		t.Fatal(err)
	}
	if writes != 0 {
		t.Fatal("preview wrote")
	}
	e.run.DryRun = false
	if _, err := hostStep(t, app, e, "write", "connectors.submit", input); err != nil {
		t.Fatal(err)
	}
	// Simulate a restarted script after durable acknowledgment but before its own completed-case checkpoint.
	if _, err := hostStep(t, app, e, "write", "connectors.submit", input); err != nil {
		t.Fatal(err)
	}
	if writes != 1 {
		t.Fatal("duplicated submission", writes)
	}
	accepted, _ := json.Marshal(e.run.Script)
	revision = "two"
	if _, err := hostStep(t, app, e, "stale", "connectors.submit", input); err == nil {
		t.Fatal("changed case accepted")
	}
	if writes != 1 {
		t.Fatal("stale write sent")
	}
	// The missing-ack scenario starts independently at the accepted checkpoint,
	// not from the preceding run's intentionally unresolved guard failure.
	e.run.Script = &automation.ScriptCheckpoint{}
	if err := json.Unmarshal(accepted, e.run.Script); err != nil {
		t.Fatal(err)
	}
	revision = "one"
	badAck = true
	if _, err := hostStep(t, app, e, "bad-ack", "connectors.submit", input); err == nil || !strings.Contains(err.Error(), "acknowledgment mismatch") {
		t.Fatal("invalid acknowledgment not detected", err)
	}
	if writes != 2 || !e.run.Script.Uncertain() {
		t.Fatal("unknown write not fenced")
	}
	if _, err := hostStep(t, app, e, "another-id", "connectors.submit", input); err == nil || writes != 2 {
		t.Fatal("new step repeated unknown submission", writes, err)
	}
}

const catalogProbe = `from pathlib import Path
def main(host):
    host.call('state', 'checkpoint.set', state={'ran': True})
    try:
        Path('../../../instance-probe.json').read_bytes()  # DATA_DIR from the run's scratch
        return {'catalog': 'read'}
    except OSError:
        return {'catalog': 'denied'}
`

// writeInstanceProbe puts synthetic instance data beside the run's scratch
// directory, as admin.token is, for catalogProbe to try to read.
func writeInstanceProbe(t *testing.T, app *Server) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(app.data, "instance-probe.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
}

// An installation without a sandbox field, as every definition saved before the
// sandbox existed, keeps the whole pipe host contract, but its package cannot
// read instance data such as the catalog beside its scratch directory. Where
// Landlock is missing the run is refused, and the refusal names the opt-out.
func TestScriptInstallationIsSandboxedByDefault(t *testing.T) {
	app, e := scriptFixture(t, catalogProbe, []string{"checkpoint"}, nil, nil)
	writeInstanceProbe(t, app)
	e.run.Spec.Script.Sandbox = nil
	e.run.Script = nil
	err := app.runScript(e.ctx, e.run, e.save)
	if sandbox.Available() != nil {
		if err == nil || !strings.Contains(err.Error(), "Landlock") || !strings.Contains(err.Error(), `"sandbox": false`) || len(e.run.Script.Steps) != 0 {
			t.Fatalf("default installation was not refused without Landlock, naming the opt-out: %v %+v", err, e.run.Script)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if !e.run.Script.Complete || !strings.Contains(string(e.run.Script.State), "ran") || !strings.Contains(e.run.Result, "denied") {
		t.Fatalf("sandboxed run did not complete its host calls without catalog access: %s %s", e.run.Result, e.run.Script.State)
	}
}

// An administrator's explicit opt-out runs the package as server code, with the
// server's access to instance data, on every platform.
func TestScriptSandboxOptOutRunsAsServerCode(t *testing.T) {
	app, e := scriptFixture(t, catalogProbe, []string{"checkpoint"}, nil, nil)
	writeInstanceProbe(t, app)
	e.run.Spec.Script.Sandbox = new(false)
	e.run.Script = nil
	if err := app.runScript(e.ctx, e.run, e.save); err != nil {
		t.Fatal(err)
	}
	if !e.run.Script.Complete || !strings.Contains(e.run.Result, "read") {
		t.Fatalf("opted-out run lost server access: %s", e.run.Result)
	}
}

func TestScriptDirectRuntimeSanitizesEnvironment(t *testing.T) {
	t.Setenv("FMN_PARENT_SECRET", "do-not-inherit-this")
	app, e := scriptFixture(t, `import os
from pathlib import Path
def main(host):
    assert 'FMN_PARENT_SECRET' not in os.environ
    assert 'OPENAI_KEY' not in os.environ
    assert 'TYPESAFEAI_KEY' not in os.environ
    assert 'DOCKER_HOST' not in os.environ
    Path('temporary').write_text('scratch works')
    host.call('state','checkpoint.set',state={'direct': True})
    return {'direct': True}
`, []string{"checkpoint"}, nil, nil)
	e.run.Script = nil
	if err := app.runScript(e.ctx, e.run, e.save); err != nil {
		t.Fatal(err)
	}
	if !e.run.Script.Complete {
		t.Fatal("missing completion")
	}
}

// An installation grants exactly the capabilities its package requests:
// validation refuses one that withholds a capability the package needs, or
// grants one it never asked for.
func TestScriptInstallationGrantsMatchTheCapabilitiesItsPackageRequests(t *testing.T) {
	app, e := scriptFixture(t, "def main(host): return {}", []string{"checkpoint"}, nil, nil)
	admin := recoverySession(t, app)
	validate := func(grants ...string) (int, string) {
		spec := e.run.Spec
		script := *spec.Script
		script.Capabilities = grants
		spec.Script = &script
		raw, _ := json.Marshal(map[string]any{"spec": spec})
		w := localRequest(app, "/api/organization/action/validate_automation", string(raw), admin)
		return w.Code, w.Body.String()
	}
	if code, body := validate("checkpoint"); code != 200 {
		t.Fatalf("requested grant: HTTP %d %s", code, body)
	}
	if code, body := validate(); code == 200 || !strings.Contains(body, "package capability not granted: checkpoint") {
		t.Errorf("withheld capability: HTTP %d %s", code, body)
	}
	if code, body := validate("checkpoint", "web.fetch"); code == 200 || !strings.Contains(body, "grant is not requested by package: web.fetch") {
		t.Errorf("unrequested grant: HTTP %d %s", code, body)
	}
}

// A package's connector call stays within its installation: a call through
// an alias it never bound, of an operation the package did not request, or of
// a write through connectors.read is refused before anything reaches the
// connection.
func TestScriptConnectorCallsStayWithinThePackagesRequest(t *testing.T) {
	app, e := scriptFixture(t, "def main(host): return {}", []string{"connectors.read", "connectors.submit"}, map[string][]string{"api": {"read", "submit"}}, nil)
	requests := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"revision":"one"}`)
	}))
	defer upstream.Close()
	// The job and its actor may also list, which the package never asked for.
	bindScriptOperations(t, app, e, upstream.URL, scriptReadOperation, connectors.Operation{Name: "list", ReadOnly: true, Method: "GET", Path: "/list"}, scriptSubmitOperation)
	submission := map[string]any{"body": map[string]string{"submission_id": "stable"}}
	for _, c := range []struct {
		step, operation string
		call            map[string]any
	}{
		{"unbound-alias", "connectors.read", map[string]any{"connection": "other", "operation": "read", "arguments": map[string]any{}}},
		{"unrequested-operation", "connectors.read", map[string]any{"connection": "api", "operation": "list", "arguments": map[string]any{}}},
		{"write-through-read", "connectors.read", map[string]any{"connection": "api", "operation": "submit", "arguments": submission}},
	} {
		if _, err := hostStep(t, app, e, c.step, c.operation, c.call); err == nil {
			t.Errorf("%s accepted", c.step)
		}
	}
	if requests != 0 {
		t.Fatalf("refused calls reached the connection %d times", requests)
	}
	if _, err := hostStep(t, app, e, "bound-read", "connectors.read", map[string]any{"connection": "api", "operation": "read", "arguments": map[string]any{}}); err != nil || requests != 1 {
		t.Fatalf("bound read: %v after %d requests", err, requests)
	}
}
