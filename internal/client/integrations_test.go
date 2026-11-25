package client

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestIntegrationCommandsUseSharedActions(t *testing.T) {
	t.Setenv("FORGETMENOTE_CONFIG_DIR", t.TempDir())
	t.Setenv("FORGETMENOTE_TOKEN", "fixture")
	for _, tc := range []struct {
		args          []string
		input, action string
		want          map[string]any
	}{
		{[]string{"data", "list"}, "", "list_data_connections", map[string]any{}},
		{[]string{"data", "operations", "warehouse"}, "", "list_data_operations", map[string]any{"connection": "warehouse"}},
		{[]string{"data", "call", "warehouse", "query", "--file", "-", "--notebook", "profiles", "--profile", "counts.md", "--revision", "abc", "--schema-hash", "contract"}, `{"sql":"select 1"}`, "call_data_operation", map[string]any{"connection": "warehouse", "operation": "query", "arguments": map[string]any{"sql": "select 1"}, "project": "profiles", "profilePath": "counts.md", "profileRevision": "abc", "schemaHash": "contract"}},
		{[]string{"data", "call", "warehouse", "query", "--file", "-"}, `{"sql":"select $1","params":[7]}`, "call_data_operation", map[string]any{"connection": "warehouse", "operation": "query", "arguments": map[string]any{"sql": "select $1", "params": []any{float64(7)}}}},
		{[]string{"automation", "status"}, "", "automation_status", map[string]any{}},
		{[]string{"automation", "discover", "packages", "--path", "automations/example", "--revision", "commit"}, "", "discover_automation_packages", map[string]any{"project": "packages", "path": "automations/example", "revision": "commit"}},
		{[]string{"automation", "prepare", "--file", "-"}, `{"id":"package"}`, "prepare_automation_package", map[string]any{"spec": map[string]any{"id": "package"}}},
		{[]string{"automation", "install", "--file", "-", "--revision", "current"}, `{"id":"package"}`, "install_automation_package", map[string]any{"spec": map[string]any{"id": "package"}, "revision": "current"}},
		{[]string{"automation", "run", "daily", "--id", "retry-safe"}, "", "run_automation", map[string]any{"id": "daily", "requestId": "retry-safe"}},
		{[]string{"automation", "cancel", "run-id"}, "", "cancel_automation", map[string]any{"id": "run-id"}},
		{[]string{"automation", "test", "learning", "--id", "preview-1", "--evidence", "retained-run"}, "", "test_automation", map[string]any{"id": "learning", "requestId": "preview-1", "evidenceRun": "retained-run"}},
		{[]string{"automation", "inspect", "run-id"}, "", "automation_run", map[string]any{"id": "run-id"}},
		{[]string{"recipe", "find", "profiles", "--query", "count"}, "", "find_recipes", map[string]any{"project": "profiles", "query": "count"}},
	} {
		t.Run(tc.action, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				var got map[string]any
				if r.URL.Path != "/api/v1/actions/"+tc.action || r.Header.Get("Authorization") != "Bearer fixture" || json.NewDecoder(r.Body).Decode(&got) != nil || !reflect.DeepEqual(got, tc.want) {
					t.Error("incorrect action", r.URL.Path, got)
				}
				w.Write([]byte(`{"ok":true}`))
			}))
			defer server.Close()
			t.Setenv("FORGETMENOTE_SERVER", server.URL)
			var out bytes.Buffer
			if err := Run(t.Context(), append(tc.args, "--json"), strings.NewReader(tc.input), &out, io.Discard); err != nil || calls != 1 || !strings.Contains(out.String(), `"ok":true`) {
				t.Fatal(err, calls, out.String())
			}
		})
	}
}
