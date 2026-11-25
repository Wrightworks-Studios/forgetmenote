package web

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"forgetmenote/internal/automation"
	"forgetmenote/internal/catalog"
)

func automaticPackageFixture(t *testing.T, follow bool) (*Server, func(map[string]string) string, automation.Definition) {
	t.Helper()
	app, _ := fixture(t)
	manifest := automation.Manifest{APIVersion: automation.PackageAPI, ID: "fixture", Name: "Fixture", Entrypoint: "main.py", Runtime: automation.PythonRuntime, Files: []string{"main.py"}, Capabilities: []string{"checkpoint"}}
	raw, _ := json.Marshal(manifest)
	advance := configureSearchSource(t, app, map[string]string{"package/automation.json": string(raw), "package/main.py": "def main(host): return {'version': 1}"})
	admin := recoverySession(t, app)
	owner := addLocal(t, app, admin, "package-owner")
	bot, err := app.catalog.CreateService(catalog.Bootstrap, "Package worker", owner)
	if err != nil {
		t.Fatal(err)
	}
	if err = app.catalog.Member(catalog.Bootstrap, "default", bot.ID, "edit"); err != nil {
		t.Fatal(err)
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
	info, _, err := app.automationPackage(t.Context(), bot.ID, packageQuery{Project: "default", Path: "package"}, true)
	if err != nil {
		t.Fatal(err)
	}
	spec := automation.Spec{ID: "automatic-package", Kind: "script", Actor: bot.ID, Project: "default", Disabled: true, TimeoutSeconds: 20, Script: &automation.Script{SourceProject: "default", SourceID: info.Source, PackagePath: "package", Revision: info.Revision, Digest: info.Digest, Runtime: manifest.Runtime, Entrypoint: manifest.Entrypoint, Capabilities: manifest.Capabilities, FollowPrimaryBranch: follow, Sandbox: new(false)}}
	request, _ := json.Marshal(map[string]any{"spec": spec, "revision": ""})
	w := localRequest(app, "/api/organization/action/install_automation_package", string(request), admin)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var installed automation.Definition
	if err = json.Unmarshal(w.Body.Bytes(), &installed); err != nil {
		t.Fatal(err)
	}
	return app, advance, installed
}

func TestInstalledPackageAutomaticallyFollowsPrimaryBranchWhenOptedIn(t *testing.T) {
	app, advance, installed := automaticPackageFixture(t, true)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// Only the periodic refresh, not a request, may load the new revision.
	app.automations.PackageRefresh = 500 * time.Millisecond
	app.startAutomations(ctx)
	revision := advance(map[string]string{"package/main.py": "def main(host): return {'version': 2}"})
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		current, _ := app.automations.Definitions.Get(installed.Spec.ID)
		if current.Spec.Script.Revision == revision {
			if !current.Spec.Disabled || current.Spec.Script.Environment == installed.Spec.Script.Environment || current.Spec.Script.Digest == installed.Spec.Script.Digest {
				t.Fatal("update did not preserve pause or prepare changed source", current)
			}
			// The fixture's explicit sandbox opt-out is part of the grant an update keeps.
			if current.Spec.Script.Sandboxed() {
				t.Fatal("automatic update dropped the administrator's sandbox opt-out")
			}
			if err := app.python.Ready(current.Spec.Script.Environment, current.Spec.Script.Digest); err != nil {
				t.Fatal(err)
			}
			w := localRequest(app, "/api/organization/action/automation_status", `{}`, recoverySession(t, app))
			if w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}
			request, _ := json.Marshal(map[string]string{"id": current.Spec.ID, "requestId": "automatic-version-preview"})
			w = localRequest(app, "/api/organization/action/test_automation", string(request), recoverySession(t, app))
			if w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}
			var queued automationRunStatus
			if err := json.Unmarshal(w.Body.Bytes(), &queued); err != nil {
				t.Fatal(err)
			}
			until := time.Now().Add(5 * time.Second)
			for time.Now().Before(until) {
				run, err := app.automations.Queue.Get(queued.ID)
				if err != nil {
					t.Fatal(err)
				}
				if run.State == "completed" {
					var result map[string]int
					if err = json.Unmarshal([]byte(run.Result), &result); err != nil || result["version"] != 2 || run.Script.Revision != revision || !run.DryRun {
						t.Fatal("preview did not execute refreshed package", run.Result, err)
					}
					return
				}
				if run.State == "failed" {
					t.Fatal(run.Error)
				}
				time.Sleep(50 * time.Millisecond)
			}
			t.Fatal("refreshed package preview did not finish")
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("installed package did not load the refreshed primary branch")
}

// A prepared environment is this process's copy of an approved package for its
// Python. Gateways keep it on ephemeral storage, and base-image updates change the
// interpreter, so an installed package must still enable and run where it was
// never prepared, without another administrator installation.
func TestInstalledPackageRunsWhereItWasNotPrepared(t *testing.T) {
	for _, change := range []string{"missing", "interpreter"} {
		t.Run(change, func(t *testing.T) {
			app, _, installed := automaticPackageFixture(t, false)
			switch change {
			case "missing":
				// Another gateway, or a restart that lost its ephemeral data directory.
				dir := filepath.Join(app.data, "automations", "python", installed.Spec.Script.Environment)
				if err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
					if err == nil && d.IsDir() {
						return os.Chmod(path, 0700)
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
				if err := os.RemoveAll(dir); err != nil {
					t.Fatal(err)
				}
			case "interpreter":
				python, err := exec.LookPath("python3")
				if err != nil {
					t.Fatal(err)
				}
				wrapper := filepath.Join(t.TempDir(), "python3")
				if err = os.WriteFile(wrapper, []byte("#!/bin/sh\nexec "+python+" \"$@\"\n"), 0700); err != nil {
					t.Fatal(err)
				}
				if err = app.ConfigureAutomationPython(wrapper); err != nil {
					t.Fatal(err)
				}
			}
			admin := recoverySession(t, app)
			spec := installed.Spec
			spec.Disabled = false
			request, _ := json.Marshal(map[string]any{"spec": spec, "revision": installed.Revision})
			w := localRequest(app, "/api/organization/action/save_automation", string(request), admin)
			if w.Code != 200 {
				t.Fatal("enable:", w.Code, w.Body.String())
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			app.startAutomations(ctx)
			request, _ = json.Marshal(map[string]string{"id": spec.ID, "requestId": "unprepared-preview"})
			w = localRequest(app, "/api/organization/action/test_automation", string(request), admin)
			if w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}
			var queued automationRunStatus
			if err := json.Unmarshal(w.Body.Bytes(), &queued); err != nil {
				t.Fatal(err)
			}
			until := time.Now().Add(10 * time.Second)
			for time.Now().Before(until) {
				run, err := app.automations.Queue.Get(queued.ID)
				if err != nil {
					t.Fatal(err)
				}
				if run.State == "failed" {
					t.Fatal(run.Error)
				}
				if run.State == "completed" {
					var result map[string]int
					if err = json.Unmarshal([]byte(run.Result), &result); err != nil || result["version"] != 1 || run.Script.Digest != installed.Spec.Script.Digest {
						t.Fatal("unprepared process did not run the approved package", run.Result, err)
					}
					return
				}
				time.Sleep(50 * time.Millisecond)
			}
			t.Fatal("unprepared package preview did not finish")
		})
	}
}

func TestAutomaticPackageUpdatePreservesApprovedInstallation(t *testing.T) {
	for _, kind := range []string{"capability", "entrypoint", "dependencies", "invalid-python", "revoked-service", "excluded-file", "unchanged-package", "mounted-definition", "concurrent-edit", "enabled"} {
		t.Run(kind, func(t *testing.T) {
			app, advance, installed := automaticPackageFixture(t, true)
			files := map[string]string{"package/main.py": "def main(host): return {'version': 2}"}
			info, _, err := app.scriptPackage(t.Context(), installed.Spec, false)
			if err != nil {
				t.Fatal(err)
			}
			manifest := info.Manifest
			switch kind {
			case "capability":
				manifest.Capabilities = append(manifest.Capabilities, "web.fetch")
			case "entrypoint":
				manifest.Entrypoint = "other.py"
				manifest.Files = append(manifest.Files, "other.py")
				files["package/other.py"] = "def main(host): return {}"
			case "dependencies":
				manifest.Requirements = "requirements.lock"
				manifest.Files = append(manifest.Files, manifest.Requirements)
				files["package/requirements.lock"] = "example==1.0 --hash=sha256:" + strings.Repeat("a", 64)
			case "invalid-python":
				files["package/main.py"] = "def broken("
			case "revoked-service":
				if err = app.catalog.Member(catalog.Bootstrap, "default", installed.Spec.Actor, ""); err != nil {
					t.Fatal(err)
				}
			case "excluded-file":
				state, _ := app.repositories.Status("default")
				cfg := state.IndexConfig
				cfg.Exclude = []string{"package/main.py"}
				if _, err = app.repositories.ConfigureIndex("default", catalog.Bootstrap, state.Generation, cfg); err != nil {
					t.Fatal(err)
				}
			case "unchanged-package":
				files = map[string]string{"readme.md": "An unrelated repository change"}
			case "mounted-definition":
				if err = app.automations.Definitions.Delete(installed.Spec.ID, installed.Revision); err != nil {
					t.Fatal(err)
				}
				if err = app.automations.Definitions.SetFiles([]automation.Spec{installed.Spec}); err != nil {
					t.Fatal(err)
				}
			case "enabled":
				spec := installed.Spec
				spec.Disabled = false
				installed, err = app.automations.Definitions.Save(spec, installed.Revision)
				if err != nil {
					t.Fatal(err)
				}
			}
			if kind == "capability" || kind == "entrypoint" || kind == "dependencies" {
				raw, _ := json.Marshal(manifest)
				files["package/automation.json"] = string(raw)
			}
			revision := advance(files)
			if kind == "concurrent-edit" {
				spec := installed.Spec
				spec.Name = "New administrator name"
				changed, err := app.automations.Definitions.Save(spec, installed.Revision)
				if err != nil {
					t.Fatal(err)
				}
				if err = app.refreshAutomationPackage(t.Context(), installed); err == nil {
					t.Fatal("stale automatic update replaced an administrator edit")
				}
				current, _ := app.automations.Definitions.Get(spec.ID)
				if current.Revision != changed.Revision {
					t.Fatal("concurrent edit lost")
				}
				return
			}
			app.refreshAutomationPackages(t.Context())
			current, _ := app.automations.Definitions.Get(installed.Spec.ID)
			if kind == "enabled" {
				if current.Spec.Script.Revision != revision || current.Spec.Disabled {
					t.Fatal("enabled installation did not follow source", current)
				}
				return
			}
			if current.Revision != installed.Revision {
				t.Fatal("unsafe or unrelated update changed installation", current)
			}
		})
	}
}

// Repository write access is not administrator review: by default a changed
// package digest is only reported as available, and runs keep the approved code.
func TestInstalledPackageStaysPinnedAndReportsAvailableUpdate(t *testing.T) {
	app, advance, installed := automaticPackageFixture(t, false)
	revision := advance(map[string]string{"package/main.py": "def main(host): return {'version': 2}"})
	app.refreshAutomationPackages(t.Context())
	current, _ := app.automations.Definitions.Get(installed.Spec.ID)
	if current.Revision != installed.Revision || current.Spec.Script.Revision != installed.Spec.Script.Revision || current.Spec.Script.Environment != installed.Spec.Script.Environment {
		t.Fatal("pinned installation adopted unreviewed package code", current)
	}
	admin := recoverySession(t, app)
	w := localRequest(app, "/api/organization/action/discover_automation_packages", `{"project":"default","path":"package"}`, admin)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var available []packageInfo
	if err := json.Unmarshal(w.Body.Bytes(), &available); err != nil || len(available) != 1 {
		t.Fatal(w.Body.String(), err)
	}
	if available[0].Revision != revision || available[0].Digest == installed.Spec.Script.Digest {
		t.Fatal("available update not reported for explicit review", available[0])
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	app.startAutomations(ctx)
	request, _ := json.Marshal(map[string]string{"id": installed.Spec.ID, "requestId": "pinned-version-preview"})
	w = localRequest(app, "/api/organization/action/test_automation", string(request), admin)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var queued automationRunStatus
	if err := json.Unmarshal(w.Body.Bytes(), &queued); err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(10 * time.Second)
	for time.Now().Before(until) {
		run, err := app.automations.Queue.Get(queued.ID)
		if err != nil {
			t.Fatal(err)
		}
		if run.State == "failed" {
			t.Fatal(run.Error)
		}
		if run.State == "completed" {
			var result map[string]int
			if err = json.Unmarshal([]byte(run.Result), &result); err != nil || result["version"] != 1 || run.Script.Revision != installed.Spec.Script.Revision {
				t.Fatal("pinned installation did not run its approved package", run.Result, err)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("pinned package preview did not finish")
}
