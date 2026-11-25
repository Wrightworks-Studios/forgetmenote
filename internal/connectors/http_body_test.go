package connectors

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHTTPNestedBodyKeepsRoutingArgumentsOutOfSubmission(t *testing.T) {
	var received map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&received)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()
	var op Operation
	json.Unmarshal([]byte(`{"name":"submit","method":"POST","path":"/cases/{id}","bodyArgument":"submission","inputSchema":{"type":"object","additionalProperties":true}}`), &op)
	m, err := New(Config{Version: 1, Connections: []Connection{{ID: "cases", Kind: "http", Endpoint: upstream.URL, Operations: []Operation{op}}}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.Call(t.Context(), "cases", "submit", json.RawMessage(`{"id":"case1","submission":{"revision":"v1"}}`), "")
	if err != nil || received["revision"] != "v1" || len(received) != 1 {
		t.Fatal(err, received)
	}
}
