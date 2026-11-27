package app

import (
	"context"
	"errors"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/instanceconfig"
	"forgetmenote/internal/modules"
	"forgetmenote/internal/pg"
	"forgetmenote/internal/pg/pgtest"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

// The runtime closes its database, so it gets a pool of its own.
func options(t *testing.T) Options {
	return Options{Data: t.TempDir(), Origin: "http://localhost", Token: strings.Repeat("x", 32), Database: pgtest.Database(t)}
}
func TestFailedOpenReleasesStoreOwnership(t *testing.T) {
	opts := options(t)
	opts.Token = "invalid"
	if runtime, err := Open(opts); err == nil {
		runtime.Close()
		t.Fatal("invalid server opened")
	}
	projects, err := catalog.Open(opts.Data, nil, pgtest.Dir(t, opts.Data))
	if err != nil {
		t.Fatal(err)
	}
	projects.Close()
}
func TestRuntimeAdmissionAndShutdown(t *testing.T) {
	opts := options(t)
	runtime, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	recorder := httptest.NewRecorder()
	runtime.ServeHTTP(recorder, httptest.NewRequest("GET", "/healthz", nil))
	if recorder.Code != 503 {
		t.Fatalf("admitted before recovery: %d", recorder.Code)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runtime.Serve(ctx, &http.Server{Addr: address}, time.Second) }()
	deadline := time.Now().Add(3 * time.Second)
	admitted := false
	for time.Now().Before(deadline) {
		response, err := http.Get("http://" + address + "/healthz")
		if err == nil {
			response.Body.Close()
			if response.StatusCode == 200 {
				admitted = true
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !admitted {
		t.Fatal("runtime never admitted after recovery")
	}
	if err := runtime.Server.ConfigureProxy(strings.Repeat("s", 32), []string{"127.0.0.1/32"}); err == nil {
		t.Fatal("startup proxy configuration changed after request admission")
	}
	runtime.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("runtime did not drain")
	}
	recorder = httptest.NewRecorder()
	runtime.ServeHTTP(recorder, httptest.NewRequest("GET", "/healthz", nil))
	if recorder.Code != 503 {
		t.Fatalf("admitted after close: %d", recorder.Code)
	}
	if err := runtime.Serve(t.Context(), &http.Server{}, time.Second); err == nil {
		t.Fatal("restarted closed runtime")
	}
	projects, err := catalog.Open(opts.Data, nil, pgtest.Dir(t, opts.Data))
	if err != nil {
		t.Fatal(err)
	}
	projects.Close()
}

// databaseOf returns a pool of its own on the database of a fresh test
// instance, for the runtime to close, and another for the test.
func databaseOf(t *testing.T) (runtime, test *pg.DB) {
	t.Helper()
	url := pgtest.URL(t, true)
	var err error
	if runtime, err = pg.Open(t.Context(), url, ""); err != nil {
		t.Fatal(err)
	}
	if test, err = pg.Open(t.Context(), url, ""); err != nil {
		runtime.Close()
		t.Fatal(err)
	}
	t.Cleanup(test.Close)
	return runtime, test
}

func testGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=Operator", "GIT_AUTHOR_EMAIL=operator@example.test", "GIT_COMMITTER_NAME=Operator", "GIT_COMMITTER_EMAIL=operator@example.test")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// The runtime keeps notebooks in the notes store and exports each into a bare
// Git repository under DATA/exports after every commit, which backups bundle;
// closing it releases the export for the next process.
func TestRuntimeExportsNotebooksForBackups(t *testing.T) {
	opts := options(t)
	runtime, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	store, err := runtime.Catalog.Access(catalog.Bootstrap, "default", "edit")
	if err != nil {
		t.Fatal(err)
	}
	saved, err := store.Save(t.Context(), "exported.md", []byte("# Exported\n"), "", "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Lstat(filepath.Join(opts.Data, "projects", "default", "workspace")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the notebook has a working tree: %v", err)
	}
	export := filepath.Join(opts.Data, "exports", "default.git")
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if raw, err := os.ReadFile(filepath.Join(export, "refs", "heads", "main")); err == nil && strings.TrimSpace(string(raw)) == saved.Revision {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the commit never reached the notebook's export")
		}
	}
	if content := testGit(t, export, "show", "main:exported.md"); content != "# Exported" {
		t.Fatalf("the export holds %q", content)
	}
	bundle := filepath.Join(t.TempDir(), "default.bundle")
	if err = store.Backup(t.Context(), bundle); err != nil {
		t.Fatal(err)
	}
	if heads := testGit(t, t.TempDir(), "bundle", "list-heads", bundle); !strings.Contains(heads, saved.Revision) {
		t.Fatalf("the backup does not hold the commit: %s", heads)
	}
	runtime.Close()
	projects, err := catalog.Open(opts.Data, nil, pgtest.Dir(t, opts.Data))
	if err != nil {
		t.Fatal("the closed runtime kept the data directory or its export:", err)
	}
	projects.Close()
}

// Startup refuses configuration files inside a notebook directory, which a
// notebook kept in the notes store does not have: configuration beside such
// notebooks is accepted, and a file inside a notebook's workspace directory
// is still refused.
func TestRuntimeConfigurationNeedsNoNotebookDirectory(t *testing.T) {
	for _, inside := range []bool{false, true} {
		opts := options(t)
		runtime, err := Open(opts)
		if err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		if inside {
			dir = filepath.Join(opts.Data, "projects", "default", "workspace")
			if err = os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
		}
		file := filepath.Join(dir, "instance.json")
		if err = os.WriteFile(file, []byte(`{"version":1}`), 0600); err != nil {
			t.Fatal(err)
		}
		resolved, err := instanceconfig.Resolve(func(name string) string {
			if name == "INSTANCE_CONFIG_FILE" {
				return file
			}
			return ""
		})
		if err != nil {
			t.Fatal(err)
		}
		err = runtime.Server.ConfigureInstance(resolved)
		runtime.Close()
		if inside && (err == nil || !strings.Contains(err.Error(), "outside notebook repositories")) {
			t.Fatalf("configuration inside a notebook directory: %v", err)
		}
		if !inside && err != nil {
			t.Fatal("configuration beside notebooks without directories:", err)
		}
	}
}

// The notebooks' Git exports share DATA/exports with the bundles of MCP
// exports when blobs are kept in the data directory; a restart, which sweeps
// bundles no export record names, leaves the Git exports whole.
func TestRestartKeepsTheNotebooksGitExports(t *testing.T) {
	opts := options(t)
	opts.Database.Close()
	url := pgtest.URL(t, true)
	open := func() *Runtime {
		t.Helper()
		o := opts
		var err error
		if o.Database, err = pg.Open(t.Context(), url, ""); err != nil {
			t.Fatal(err)
		}
		runtime, err := Open(o)
		if err != nil {
			o.Database.Close()
			t.Fatal(err)
		}
		return runtime
	}
	runtime := open()
	store, err := runtime.Catalog.Access(catalog.Bootstrap, "default", "edit")
	if err != nil {
		t.Fatal(err)
	}
	saved, err := store.Save(t.Context(), "kept.md", []byte("# Kept\n"), "", "test")
	if err != nil {
		t.Fatal(err)
	}
	export := filepath.Join(opts.Data, "exports", "default.git")
	if _, err = runtime.Notes.Exporter.Sync(t.Context(), "default"); err != nil {
		t.Fatal(err)
	}
	runtime.Close()
	runtime = open()
	defer runtime.Close()
	if head := testGit(t, export, "rev-parse", "main"); head != saved.Revision {
		t.Fatalf("the export names %s after a restart, not %s", head, saved.Revision)
	}
	testGit(t, export, "fsck", "--strict")
}

// An instance that turns off a module whose wiring has not landed refuses to
// open; with an empty or absent modules section it opens and serves as
// before, runs every module, and keeps that set until it restarts.
func TestRuntimeRefusesModulesOffAndOpensWithoutThem(t *testing.T) {
	runtime, err := Open(options(t))
	if err != nil {
		t.Fatal(err)
	}
	err = runtime.Server.ConfigureInstance(instanceconfig.Resolved{Config: instanceconfig.Config{Version: 1, Modules: instanceconfig.Modules{Off: []string{"personal", "messaging", "tasks"}}}})
	runtime.Close()
	if err == nil || !strings.Contains(err.Error(), "instance modules") || !strings.Contains(err.Error(), "not supported yet") {
		t.Fatalf("an instance with modules off opened: %v", err)
	}
	for _, off := range [][]string{nil, {}} {
		runtime, err := Open(options(t))
		if err != nil {
			t.Fatal(err)
		}
		defer runtime.Close()
		if err = runtime.Server.ConfigureInstance(instanceconfig.Resolved{Config: instanceconfig.Config{Version: 1, Modules: instanceconfig.Modules{Off: off}}}); err != nil {
			t.Fatal(err)
		}
		for _, module := range runtime.Server.Modules().States() {
			if !module.Enabled {
				t.Fatalf("%s is off", module.Name)
			}
		}
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		address := listener.Addr().String()
		listener.Close()
		go runtime.Serve(t.Context(), &http.Server{Addr: address}, time.Second)
		admitted := false
		for deadline := time.Now().Add(3 * time.Second); !admitted && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			if response, err := http.Get("http://" + address + "/healthz"); err == nil {
				response.Body.Close()
				admitted = response.StatusCode == 200
			}
		}
		if !admitted {
			t.Fatal("runtime never admitted")
		}
		if err := runtime.Server.ConfigureModules(nil); err == nil {
			t.Fatal("the module set changed after request admission")
		}
	}
}

// The module set is decided before the server is built, from the same
// instance.json that configures it: the runtime runs the modules it was
// opened with, and refuses a configuration that names another set rather
// than run modules its operator turned off, or leave out ones they did not.
func TestRuntimeRunsTheModulesItOpensWith(t *testing.T) {
	off := []string{"sources", "judgments"}
	set, err := modules.Parse(off)
	if err != nil {
		t.Fatal(err)
	}
	opts := options(t)
	opts.Modules = set
	runtime, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	for _, other := range [][]string{nil, {"sources"}, {"judgments", "sources", "data-connections"}} {
		err := runtime.Server.ConfigureInstance(instanceconfig.Resolved{Config: instanceconfig.Config{Version: 1, Modules: instanceconfig.Modules{Off: other}}})
		if err == nil || !strings.Contains(err.Error(), "instance modules") || !strings.Contains(err.Error(), "restart") {
			t.Fatalf("%v: %v", other, err)
		}
	}
	if err := runtime.Server.ConfigureInstance(instanceconfig.Resolved{Config: instanceconfig.Config{Version: 1, Modules: instanceconfig.Modules{Off: []string{"judgments", "sources"}}}}); err != nil {
		t.Fatal(err)
	}
	for _, module := range runtime.Server.Modules().States() {
		if module.Enabled == slices.Contains(off, module.Name) {
			t.Fatalf("%s enabled=%v", module.Name, module.Enabled)
		}
	}
}
