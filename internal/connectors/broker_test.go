package connectors

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
)

func TestBrokerUsesCuratedContractsWithoutCallerCode(t *testing.T) {
	var calls, catalogs atomic.Int32
	var clipped atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Code    string `json:"code"`
			Reason  string `json:"reason"`
			Timeout int    `json:"timeoutSeconds"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		python, err := exec.LookPath("python3")
		if err == nil {
			cmd := exec.Command(python, "-c", "import ast,sys; ast.parse(sys.stdin.read())")
			cmd.Stdin = strings.NewReader(body.Code)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("invalid generated helper code: %v %s", err, out)
			}
		}
		if r.URL.Path != "/v1/shell-ro/run" || body.Reason == "" || body.Timeout < 1 {
			t.Error("invalid broker request")
		}
		if !strings.Contains(body.Code, "_result={'tool':") {
			catalogs.Add(1)
		}
		result := `{"tools":[{"name":"lookup","description":"Lookup a record","arguments":{"id":{"type":"integer","required":true,"minimum":1},"include_deleted":{"type":"boolean","default":false},"debit_types":{"type":"array","items":{"type":"string","enum":["charge","refund"]},"default":[]}}},{"name":"private_helper","arguments":{}}]}`
		if strings.Contains(body.Code, "_result={'tool':") {
			calls.Add(1)
			if strings.Contains(body.Code, "__import__") {
				t.Error("caller code was interpolated")
			}
			result = `{"tool":"lookup","result":{"id":1,"status":"ok"}}`
		}
		json.NewEncoder(w).Encode(map[string]any{"exitCode": 0, "stdout": "startup noise\n__FMN_HELPER_JSON__=" + result + "\n", "stdoutTruncated": clipped.Load()})
	}))
	defer server.Close()
	var cfg Config
	json.Unmarshal([]byte(`{"version":1,"connections":[{"id":"app","kind":"broker","endpoint":"`+server.URL+`","broker":{"module":"app.registry","catalogFunction":"list_tools","invokeFunction":"invoke_tool"},"operations":[{"name":"lookup","readOnly":true}]}]}`), &cfg)
	m, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	ops, err := m.Tools(t.Context(), "app")
	if err != nil || len(ops) != 1 || !strings.Contains(string(ops[0].InputSchema), "minimum") {
		t.Fatalf("live contracts: %+v %v", ops, err)
	}
	if _, err = m.Call(t.Context(), "app", "lookup", json.RawMessage(`{"id":1,"include_deleted":true,"debit_types":["charge"]}`), ops[0].SchemaHash); err != nil {
		t.Fatal(err)
	}
	for _, args := range []string{`{"id":"__import__('os')"}`, `{"id":0}`, `{"id":1,"code":"print('oops')"}`, `{"id":1,"include_deleted":"false"}`, `{"id":1,"debit_types":["unknown"]}`, `{"id":1,"debit_types":[1]}`} {
		if _, err = m.Call(t.Context(), "app", "lookup", json.RawMessage(args), ""); err == nil {
			t.Fatal("invalid helper call accepted")
		}
	}
	if calls.Load() != 1 {
		t.Fatal("unexpected invocation", calls.Load())
	}
	// Calls reuse the catalog discovered moments before; inspection asks again.
	if catalogs.Load() != 1 {
		t.Fatal("calls asked the broker for its catalog again", catalogs.Load())
	}
	expireDiscovery(m, "app")
	if _, err = m.Call(t.Context(), "app", "lookup", json.RawMessage(`{"id":1}`), ops[0].SchemaHash); err != nil || catalogs.Load() != 2 {
		t.Fatal("an expired catalog was not asked for again", err, catalogs.Load())
	}
	clipped.Store(true)
	if _, err = m.Tools(t.Context(), "app"); err == nil || catalogs.Load() != 3 {
		t.Fatal("truncated catalog accepted", err)
	}
}
