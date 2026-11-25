package web

import (
	"context"
	"encoding/json"
	"fmt"
	"forgetmenote/internal/pg/pgtest"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"forgetmenote/internal/automation"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/connectors"
)

func TestRepositoryAutomationTargetDefinitions(t *testing.T) {
	root := os.Getenv("FMN_TEST_AUTOMATIONS_ROOT")
	if root == "" {
		t.Skip("set FMN_TEST_AUTOMATIONS_ROOT to the local automation repository")
	}
	raw, err := os.ReadFile(filepath.Join(root, "installation", "jobs.json"))
	if err != nil {
		t.Fatal(err)
	}
	var target automation.Config
	if err = json.Unmarshal(raw, &target); err != nil {
		t.Fatal(err)
	}
	if len(target.Jobs) != 19 {
		t.Fatal("expected 19 replacement definitions")
	}
	definitions, err := automation.OpenDefinitions(pgtest.Org(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, spec := range target.Jobs {
		if spec.Script != nil {
			if spec.Script.SourceID != "${PACKAGE_SOURCE_ID}" || spec.Script.Revision != "${PACKAGE_REVISION}" {
				t.Fatal("installation must discover its source identity and reviewed commit")
			}
			// Syntax validation only: these identities belong to no live installation.
			spec.Script.SourceID = "fixture-source"
			spec.Script.Revision = strings.Repeat("a", 40)
		}
		if _, err := definitions.Save(spec, ""); err != nil {
			t.Fatalf("%s: %v", spec.ID, err)
		}
	}
}

// The private package checkout is an explicit input; all runtime data and HTTP
// services below are synthetic. This never calls the production instance.
func TestRepositoryAutomationPackagesThroughHost(t *testing.T) {
	root := os.Getenv("FMN_TEST_AUTOMATIONS_ROOT")
	if root == "" {
		t.Skip("set FMN_TEST_AUTOMATIONS_ROOT to the local automation repository")
	}
	raw, err := os.ReadFile(filepath.Join(root, "installation", "jobs.json"))
	if err != nil {
		t.Fatal(err)
	}
	var target automation.Config
	if err = json.Unmarshal(raw, &target); err != nil {
		t.Fatal(err)
	}
	for _, targetSpec := range target.Jobs {
		if targetSpec.Kind != "script" {
			continue
		}
		t.Run(targetSpec.ID, func(t *testing.T) {
			dir := filepath.Join(root, targetSpec.Script.PackagePath)
			raw, err := os.ReadFile(filepath.Join(dir, "automation.json"))
			if err != nil {
				t.Fatal(err)
			}
			manifest, err := automation.ParseManifest(raw)
			if err != nil {
				t.Fatal(err)
			}
			files := map[string]string{"automation.json": string(raw)}
			for _, name := range manifest.Files {
				raw, err = os.ReadFile(filepath.Join(dir, name))
				if err != nil {
					t.Fatal(err)
				}
				files[name] = string(raw)
			}
			if automation.PackageDigest(files) != targetSpec.Script.Digest {
				t.Fatal("target package digest is stale")
			}
			app, e := packageFixture(t, manifest, files)
			e.run.Spec.ID = targetSpec.ID
			e.run.Spec.Script.Parameters = targetSpec.Script.Parameters
			e.run.Spec.Alert = targetSpec.Alert
			p := e.run.Spec.Script.Parameters
			if _, ok := p["entryPath"]; ok {
				p["entryPath"] = "health.go"
			}
			e.run.Spec.TimeoutSeconds = 60
			var models, reads atomic.Int32
			var conflict atomic.Bool
			store, err := app.catalog.Access(e.run.Spec.Actor, "default", "edit")
			if err != nil {
				t.Fatal(err)
			}
			if targetSpec.ID == "curated-maintenance" {
				for _, name := range []string{"source-contract-map.md", "source-change-summary.md"} {
					if _, err := store.Save(t.Context(), name, []byte("# Secondary\nSynthetic source explanation. [Unverified](https://example.org/unverified)"), "", "fixture"); err != nil {
						t.Fatal(err)
					}
				}
			}
			now := time.Now().UTC()
			e.run.Created = now
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				op := strings.TrimPrefix(r.URL.Path, "/")
				var result any
				switch op {
				case "probe":
					result = map[string]any{"httpStatus": 200, "body": map[string]string{"database": "OK"}}
				case "lag-current", "lag-baseline":
					result = map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": []any{map[string]any{"value": []any{now.Unix(), "100"}}}}}
				case "lag-samples":
					result = map[string]any{"status": "success", "data": map[string]any{"resultType": "streams", "result": []any{map[string]any{"values": []any{[]any{fmt.Sprint(now.UnixNano()), "behind by 70 seconds"}}}}}}
				case "account-completion":
					result = map[string]any{"status": "success", "data": map[string]any{"resultType": "streams", "result": []any{}}}
				case "deployments":
					result = map[string]any{"items": []any{map[string]any{"metadata": map[string]any{"name": "web", "namespace": "fixture", "generation": 2, "creationTimestamp": now.Add(-20 * time.Minute).Format(time.RFC3339)}, "spec": map[string]any{"replicas": 1}, "status": map[string]any{"observedGeneration": 2, "readyReplicas": 1, "updatedReplicas": 1, "availableReplicas": 1}}}}
				case "events":
					result = map[string]any{"items": []any{}}
				case "rollout-logs":
					result = map[string]any{"status": "success", "data": map[string]any{"resultType": "matrix", "result": []any{}}}
				default:
					models.Add(1)
					var request map[string]any
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Error(err)
					}
					if _, ok := request["tools"]; ok {
						t.Error("synthesis received tools")
					}
					if conflict.Load() {
						doc, err := store.Read(context.Background(), p["output"].(string), "")
						if err == nil {
							_, err = store.Save(context.Background(), doc.Path, []byte("Human edit wins"), doc.Revision, "human")
						}
						if err != nil {
							t.Error(err)
						}
					}
					result = map[string]any{"choices": []any{map[string]any{"message": map[string]string{"role": "assistant", "content": `{"sections":[{"heading":"Understanding","text":"Synthetic observation.","references":["e1"]}]}`}, "finish_reason": "stop"}}, "usage": map[string]int{"prompt_tokens": 20, "completion_tokens": 10}}
				}
				if op != "chat/completions" {
					reads.Add(1)
				}
				json.NewEncoder(w).Encode(result)
			}))
			defer upstream.Close()
			var connections []connectors.Connection
			e.run.Spec.Operations = map[string][]string{}
			for alias, ops := range manifest.Connections {
				id := "fixture-" + alias
				c := connectors.Connection{ID: id, Kind: "http", Endpoint: upstream.URL, ExternalProcessing: true}
				for _, op := range ops {
					c.Operations = append(c.Operations, connectors.Operation{Name: op, Path: "/" + op, ReadOnly: true, InputSchema: json.RawMessage(`{"type":"object","additionalProperties":true}`)})
				}
				connections = append(connections, c)
				e.run.Spec.Script.Connections[alias] = id
				e.run.Spec.Connections = append(e.run.Spec.Connections, id)
				e.run.Spec.Operations[id] = ops
				grant, _ := app.catalog.ConnectionAccess(catalog.Bootstrap, id)
				grant.Users = map[string][]string{e.run.Spec.Actor: ops}
				if _, err := app.catalog.SetConnectionAccess(catalog.Bootstrap, grant); err != nil {
					t.Fatal(err)
				}
			}
			if len(connections) > 0 {
				app.dataConnections, err = connectors.New(connectors.Config{Version: 1, Connections: connections})
				if err != nil {
					t.Fatal(err)
				}
			}
			cfg := map[string]any{"defaultConnection": "local", "connections": []any{map[string]any{"id": "local", "label": "Fixture", "format": "chat-completions", "baseURL": upstream.URL, "models": []string{"synthetic"}, "defaultModel": "synthetic", "local": true}}}
			raw, _ = json.Marshal(cfg)
			config := filepath.Join(t.TempDir(), "chat.json")
			os.WriteFile(config, raw, 0600)
			if err = app.ConfigureChat(config); err != nil {
				t.Fatal(err)
			}
			e.run.Script = nil
			if err = app.runAutomation(t.Context(), e.run, e.save); err != nil {
				t.Fatal(err)
			}
			wantModels := int32(0)
			if e.run.Spec.UsesAI() {
				wantModels = 1
			}
			if models.Load() != wantModels || !e.run.Script.Complete {
				t.Fatal("wrong model count or incomplete execution", models.Load(), e.run.Result)
			}
			if !e.run.Spec.UsesAI() && e.run.Alert == nil {
				t.Fatal("monitor did not report through incident owner")
			}
			count := reads.Load()
			snapshot, _ := json.Marshal(e.run)
			var restored automation.Run
			json.Unmarshal(snapshot, &restored)
			if err = app.runAutomation(t.Context(), &restored, func() error { return nil }); err != nil {
				t.Fatal(err)
			}
			if models.Load() != wantModels || reads.Load() != count {
				t.Fatal("completed checkpoint replay repeated calls")
			}
			if wantModels > 0 {
				doc, err := store.Read(t.Context(), p["output"].(string), "")
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(doc.Content), "e1") {
					t.Fatal("exact reference missing", string(doc.Content))
				}
				preview := automation.Run{ID: automation.Hash(targetSpec.ID + "preview"), Spec: e.run.Spec, Created: now, DryRun: true}
				if err = app.runAutomation(t.Context(), &preview, func() error { return nil }); err != nil {
					t.Fatal(err)
				}
				after, _ := store.Read(t.Context(), doc.Path, "")
				if after.Revision != doc.Revision {
					t.Fatal("preview published")
				}
				if targetSpec.ID == "source-change-summary" {
					conflict.Store(true)
					collided := automation.Run{ID: automation.Hash("conflict"), Spec: e.run.Spec, Created: now}
					if err = app.runAutomation(t.Context(), &collided, func() error { return nil }); err == nil {
						t.Fatal("concurrent write overwritten")
					}
					after, _ = store.Read(t.Context(), doc.Path, "")
					if string(after.Content) != "Human edit wins" {
						t.Fatal("lost human edit")
					}
				}
			}
		})
	}
}
