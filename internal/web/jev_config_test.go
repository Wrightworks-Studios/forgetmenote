package web

import (
	"context"
	"encoding/json"
	"forgetmenote/internal/catalog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func writeAIConfig(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func assertAIAvailability(t *testing.T, app *Server, chatEnabled, jevAvailable bool) {
	t.Helper()
	for _, check := range []struct {
		path, field string
		want        bool
	}{{"/api/chat/connections", "enabled", chatEnabled}, {"/api/jev/settings", "available", jevAvailable}} {
		w := proxyCall(app, "alice", "GET", check.path, "")
		var result map[string]any
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result[check.field] != check.want {
			t.Fatalf("%s: %d %s", check.path, w.Code, w.Body)
		}
		for _, private := range []string{"secretFile", "synthetic-standalone-secret", "api.typesafe.ai"} {
			if strings.Contains(w.Body.String(), private) {
				t.Fatalf("provider configuration exposed by %s", check.path)
			}
		}
	}
}

func TestStandaloneJevReviewsWithoutChatAndRequiresConsent(t *testing.T) {
	app, store := fixture(t)
	if err := app.ConfigureProxy(proxyTestSecret, []string{"127.0.0.1/32"}); err != nil {
		t.Fatal(err)
	}
	if err := app.catalog.Member(catalog.Bootstrap, "default", "alice", "owner"); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/systemone" || r.Header.Get("Authorization") != "Bearer synthetic-standalone-secret" {
			t.Error("incorrect Jev endpoint or credential")
			http.Error(w, "fixture", 400)
			return
		}
		calls.Add(1)
		syntheticReviewAnswer(w, r)
	}))
	t.Cleanup(provider.Close)
	previous := http.DefaultTransport
	http.DefaultTransport = provider.Client().Transport
	t.Cleanup(func() {
		app.Close()
		http.DefaultTransport = previous
	})
	key := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(key, []byte("synthetic-standalone-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	path := writeAIConfig(t, map[string]any{"provider": "typesafe", "enabled": true, "model": "jev-1.13.0", "endpoint": provider.URL + "/v1/systemone", "secretFile": key})
	if err := app.ConfigureAI("", path); err != nil {
		t.Fatal(err)
	}
	assertAIAvailability(t, app, false, true)
	for name, body := range map[string]string{"a.md": "# Workshop\nThe workshop opens at 09:00 on weekdays.", "b.md": "# Workshop\nThe workshop opens at 11:00 on weekdays."} {
		if _, err := store.Save(context.Background(), "notes/"+name, []byte(body), "", "operator"); err != nil {
			t.Fatal(err)
		}
	}
	input := `{"kind":"consistency","operationId":"standalone"}`
	if w := proxyCall(app, "alice", "POST", "/p/default/api/jev/jobs", input); w.Code < 400 {
		t.Fatal("Jev review ran without personal consent", w.Body)
	}
	if calls.Load() != 0 {
		t.Fatal("Jev made a request before consent")
	}
	if w := proxyCall(app, "alice", "POST", "/api/jev/settings", `{"consent":true,"rerank":true,"citations":false}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	id := startReview(t, app, input)
	if job := waitReview(t, app, id); job["state"] != "completed" || calls.Load() == 0 {
		t.Fatal("standalone review did not complete", job)
	}
	findings, err := store.Findings(context.Background())
	if err != nil || len(findings) != 1 {
		t.Fatalf("standalone review did not publish its finding: %v %v", findings, err)
	}
	assertAIAvailability(t, app, false, true)
}

func TestStandaloneJevRejectsConflictsAndKeepsPriorConfiguration(t *testing.T) {
	app, _ := fixture(t)
	if err := app.ConfigureProxy(proxyTestSecret, []string{"127.0.0.1/32"}); err != nil {
		t.Fatal(err)
	}
	if err := app.catalog.Member(catalog.Bootstrap, "default", "alice", "read"); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(key, []byte("synthetic-standalone-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	jevPath := writeAIConfig(t, map[string]any{"enabled": true, "model": "jev-1.13.0", "secretFile": key})
	if err := app.ConfigureAI("", jevPath); err != nil {
		t.Fatal(err)
	}
	chatConfig := map[string]any{"connections": []any{map[string]any{"id": "local", "label": "Local", "format": "chat-completions", "baseURL": "http://localhost/v1", "models": []string{"synthetic"}, "local": true}}}
	chatPath := writeAIConfig(t, chatConfig)
	chatConfig["localOnly"] = true
	localPath := writeAIConfig(t, chatConfig)
	chatConfig["localOnly"] = false
	chatConfig["jev"] = map[string]any{"enabled": false}
	conflictPath := writeAIConfig(t, chatConfig)
	for _, test := range []struct{ name, chat, jev, errorText string }{
		{"local only", localPath, jevPath, "local-only"},
		{"two definitions", conflictPath, jevPath, "not both"},
		{"unsupported provider", "", writeAIConfig(t, map[string]any{"provider": "unconfigured", "enabled": true, "model": "small-decision-model", "secretFile": key}), "unsupported decision provider"},
		{"missing credential", "", writeAIConfig(t, map[string]any{"enabled": true, "model": "jev-1.13.0", "secretFile": filepath.Join(t.TempDir(), "missing")}), "credential"},
		{"unknown field", "", writeAIConfig(t, map[string]any{"enabled": true, "apiKey": "private-value"}), "unknown field"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := app.ConfigureAI(test.chat, test.jev)
			if err == nil || !strings.Contains(err.Error(), test.errorText) || strings.Contains(err.Error(), "private-value") {
				t.Fatalf("incorrect configuration rejection: %v", err)
			}
			assertAIAvailability(t, app, false, true)
		})
	}
	if err := app.ConfigureAI(chatPath, jevPath); err != nil {
		t.Fatal(err)
	}
	assertAIAvailability(t, app, true, true)
	disabled := writeAIConfig(t, map[string]any{"enabled": false, "secretFile": "/nonexistent"})
	if err := app.ConfigureAI(localPath, disabled); err != nil {
		t.Fatal(err)
	}
	assertAIAvailability(t, app, true, false)
}

func TestJevConfigurationLoadsMountedSecretWithoutPublicDisclosure(t *testing.T) {
	app := chatFixture(t)
	dir := t.TempDir()
	secret := filepath.Join(dir, "synthetic-key")
	if err := os.WriteFile(secret, []byte("private-synthetic-marker"), 0600); err != nil {
		t.Fatal(err)
	}
	config := map[string]any{"connections": []any{map[string]any{"id": "local", "label": "Local", "format": "chat-completions", "baseURL": "http://localhost/v1", "models": []string{"test"}, "local": true}}, "jev": map[string]any{"enabled": true, "model": "jev-1.13.0", "secretFile": secret}}
	path := filepath.Join(dir, "chat.json")
	raw, _ := json.Marshal(config)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := app.ConfigureChat(path); err != nil {
		t.Fatal(err)
	}
	// A missing credential must fail configuration before any feature becomes usable.
	if err := os.Remove(secret); err != nil {
		t.Fatal(err)
	}
	if err := app.ConfigureChat(path); err == nil {
		t.Fatal("configured Jev without loading its mounted credential")
	}
	w := proxyCall(app, "alice", "GET", "/api/chat/connections", "")
	for _, private := range []string{"private-synthetic-marker", "secretFile", "api.typesafe.ai", secret} {
		if strings.Contains(w.Body.String(), private) {
			t.Fatal("operator-only Jev configuration leaked")
		}
	}
	config["localOnly"] = true
	raw, _ = json.Marshal(config)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := app.ConfigureChat(path); err == nil || !strings.Contains(err.Error(), "local-only") {
		t.Fatalf("local-only must prohibit Jev before credential reads: %v", err)
	}
}
