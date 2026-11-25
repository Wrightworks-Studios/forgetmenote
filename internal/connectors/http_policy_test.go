package connectors

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestHTTPClusterTransportConsent(t *testing.T) {
	for _, tc := range []struct {
		endpoint    string
		allow, want bool
	}{
		{"http://broker.team.svc.cluster.local:8080", true, true},
		{"http://broker.team.svc.cluster.local:8080", false, false},
		{"http://public.example", true, false},
		{"http://broker.team.svc.cluster.local.attacker.example", true, false},
		{"http://broker.svc.cluster.local", true, false},
	} {
		var cfg Config
		b, _ := json.Marshal(map[string]any{"version": 1, "connections": []any{map[string]any{"id": "test", "kind": "http", "endpoint": tc.endpoint, "allowClusterHTTP": tc.allow, "operations": []any{map[string]any{"name": "read", "path": "/read", "readOnly": true}}}}})
		json.Unmarshal(b, &cfg)
		m, err := New(cfg)
		if err == nil {
			m.Close()
		}
		if (err == nil) != tc.want {
			t.Fatalf("endpoint %s allow %v: %v", tc.endpoint, tc.allow, err)
		}
	}
}

func TestHTTPRepeatedJSONAndBoundedDates(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		events := r.URL.Query()["e"]
		var first, second map[string]string
		if len(events) == 2 {
			json.Unmarshal([]byte(events[0]), &first)
			json.Unmarshal([]byte(events[1]), &second)
		}
		if len(events) != 2 || first["event_type"] != "Login & success" || second["event_type"] != "Purchase" {
			t.Errorf("wrong repeated JSON query: %v", events)
		}
		w.Write([]byte(`{"answer":"ok"}`))
	}))
	defer server.Close()
	var cfg Config
	json.Unmarshal([]byte(`{"version":1,"connections":[{"id":"test","kind":"http","endpoint":`+strconvJSON(server.URL)+`,"operations":[{"name":"read","path":"/funnel","readOnly":true,"query":{"start":"{start}","end":"{end}"},"queryLists":{"e":"events"},"range":{"start":"start","end":"end","format":"date","maxSeconds":2592000},"inputSchema":{"type":"object","properties":{"start":{"type":"string"},"end":{"type":"string"},"events":{"type":"array","items":{"type":"object"}}},"required":["start","end","events"],"additionalProperties":false}}]}]}`), &cfg)
	m, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	for _, dates := range [][2]string{{"20260901", "20260902"}, {"20260901", "20261002"}, {"20260902", "20260901"}, {"bad", "20260901"}} {
		args, _ := json.Marshal(map[string]any{"start": dates[0], "end": dates[1], "events": []any{map[string]any{"event_type": "Login & success"}, map[string]any{"event_type": "Purchase"}}})
		_, err = m.Call(t.Context(), "test", "read", args, "")
		if dates[0] == "20260901" && dates[1] == "20260902" {
			if err != nil {
				t.Fatal(err)
			}
		} else if err == nil {
			t.Fatalf("accepted invalid range %v", dates)
		}
	}
	if calls.Load() != 1 {
		t.Fatal("invalid range reached upstream", calls.Load())
	}
}
