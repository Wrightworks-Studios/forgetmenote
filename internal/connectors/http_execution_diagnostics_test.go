package connectors

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestHTTPExecutionFailureReportsOnlyStructuralDiagnostics(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "default-private", true: "opt-in-structural"}[enabled], func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(422)
				json.NewEncoder(w).Encode(map[string]any{"exitCode": 1, "stderr": "Traceback (most recent call last):\n  File \"<shell-ro>\", line 11, in <module>\nTypeError: PRIVATE_COMPANY_VALUE_AND_PROVIDER_CREDENTIAL\n", "stdout": "PRIVATE_OUTPUT", "error": "PRIVATE_ERROR", "podName": "PRIVATE_TOPOLOGY", "timedOut": false})
			}))
			defer upstream.Close()
			raw, _ := json.Marshal(map[string]any{"version": 1, "connections": []any{map[string]any{"id": "execution", "kind": "http", "endpoint": upstream.URL, "operations": []any{map[string]any{"name": "run", "readOnly": true, "method": "POST", "path": "/", "body": true, "executionDiagnostics": enabled}}}}})
			var cfg Config
			if err := json.Unmarshal(raw, &cfg); err != nil {
				t.Fatal(err)
			}
			manager, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer manager.Close()
			_, err = manager.Call(t.Context(), "execution", "run", json.RawMessage(`{}`), "")
			if err == nil {
				t.Fatal("failed execution reported success")
			}
			if strings.Contains(err.Error(), "PRIVATE_") {
				t.Fatal("upstream private text escaped")
			}
			if got := strings.Contains(err.Error(), "TypeError at submitted line 11"); got != enabled {
				t.Fatalf("structural execution feedback = %q, enabled=%v", err, enabled)
			}
		})
	}
}

func TestHTTPExecutionDiagnosticsRefuseUntrustedOrOversizedDetails(t *testing.T) {
	status := 422
	body := ""
	var responseMu sync.Mutex
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		responseMu.Lock()
		status, body := status, body
		responseMu.Unlock()
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	defer upstream.Close()
	manager, err := New(Config{Version: 1, Connections: []Connection{{ID: "executor", Kind: "http", Endpoint: upstream.URL, Operations: []Operation{{Name: "run", ReadOnly: true, Method: "POST", Path: "/", Body: true, ExecutionDiagnostics: true}}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	for _, row := range []struct {
		name    string
		status  int
		payload map[string]any
	}{
		{"authentication", 403, map[string]any{"exitCode": 1, "stderr": "File \"<shell-ro>\", line 11, in <module>\nTypeError: PRIVATE_SECRET"}},
		{"server", 500, map[string]any{"exitCode": 1, "stderr": "File \"<shell-ro>\", line 11, in <module>\nTypeError: PRIVATE_SECRET"}},
		{"unknown-class", 422, map[string]any{"exitCode": 1, "stderr": "File \"<shell-ro>\", line 11, in <module>\nPRIVATE_CREDENTIAL_Error: secret"}},
		{"unsubmitted-frame", 422, map[string]any{"exitCode": 1, "stderr": "File \"/PRIVATE_PROVIDER_PATH\", line 11, in <module>\nTypeError: PRIVATE_SECRET"}},
		{"oversized", 422, map[string]any{"exitCode": 1, "stderr": "File \"<shell-ro>\", line 11, in <module>\nTypeError: " + strings.Repeat("PRIVATE_SECRET", 2000)}},
		{"not-an-execution", 422, map[string]any{"stderr": "File \"<shell-ro>\", line 11, in <module>\nTypeError: PRIVATE_SECRET"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			responseMu.Lock()
			status = row.status
			raw, _ := json.Marshal(row.payload)
			body = string(raw)
			responseMu.Unlock()
			_, err := manager.Call(t.Context(), "executor", "run", json.RawMessage(`{}`), "")
			if err == nil || strings.Contains(err.Error(), "PRIVATE_") || strings.Contains(err.Error(), "TypeError") || strings.Contains(err.Error(), "submitted") {
				t.Fatalf("unsafe diagnostic: %v", err)
			}
			if row.status == 500 && !errors.Is(err, ErrUnavailable) {
				t.Fatal("lost retryable server failure", err)
			}
		})
	}
}
