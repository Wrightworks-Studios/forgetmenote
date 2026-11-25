package chat

import (
	"encoding/json"
	"forgetmenote/internal/pg/pgtest"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOperatorCanRaiseStorageAndExecutionCapacity(t *testing.T) {
	c := Config{Connections: []Connection{providerTestConnection("http://localhost/v1", "chat-completions")}, Limits: Limits{MaxConversations: 250, MaxEvents: 50000, MaxRecordBytes: 128 << 20, MaxRounds: 120, MaxTools: 2000, RunSeconds: 7200}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	s, err := OpenStore(pgtest.Org(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.SetLimits(c.Limits, c.Connections)
	for i := 0; i < 101; i++ {
		if _, err := s.Create("alice", "default", Options{}); err != nil {
			t.Fatalf("conversation %d: %v", i+1, err)
		}
	}
}

func TestProviderAcceptsRequestWithinConfiguredModelContext(t *testing.T) {
	seen := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct{ Messages []Message }
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		seen = len(request.Messages) == 1 && strings.HasSuffix(request.Messages[0].Content, "FINAL_VALUE")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Received complete input."},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()
	c := providerTestConnection(srv.URL, "chat-completions")
	c.Limits.ContextTokens = 4_000_000
	p, err := NewProvider(c)
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Generate(t.Context(), Request{Messages: []Message{{Role: "user", Content: strings.Repeat("x", 9<<20) + "FINAL_VALUE"}}}, nil)
	if err != nil || !seen {
		t.Fatalf("valid large input rejected: seen=%t err=%v", seen, err)
	}
}
