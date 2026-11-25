package connectors

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestHTTPStatusModeReportsFailureAsBoundedEvidence(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		w.Write([]byte(`{"database":"down"}`))
	}))
	defer upstream.Close()
	raw := `{"version":1,"connections":[{"id":"health","kind":"http","endpoint":` + string(mustJSONStatus(t, upstream.URL)) + `,"operations":[{"name":"probe","readOnly":true,"path":"/health","response":"status"}]}]}`
	file := filepath.Join(t.TempDir(), "connections.json")
	if err := os.WriteFile(file, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	manager, err := Load(file)
	if err != nil {
		t.Fatal(err)
	}
	result, err := manager.Call(t.Context(), "health", "probe", json.RawMessage(`{}`), "")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(result.Data)
	if string(b) != `{"body":{"database":"down"},"httpStatus":503}` {
		t.Fatal(string(b))
	}
}
func mustJSONStatus(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
