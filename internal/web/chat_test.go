package web

import (
	"encoding/json"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func chatFixture(t *testing.T) *Server {
	t.Helper()
	app, _ := fixture(t)
	if err := app.ConfigureProxy(proxyTestSecret, []string{"127.0.0.1/32"}); err != nil {
		t.Fatal(err)
	}
	for _, user := range []string{"alice", "bob"} {
		if err := app.catalog.Member(catalog.Bootstrap, "default", user, "read"); err != nil {
			t.Fatal(err)
		}
	}
	cfg := filepath.Join(t.TempDir(), "chat.json")
	if err := os.WriteFile(cfg, []byte(`{"connections":[{"id":"local","label":"Synthetic local","format":"chat-completions","baseURL":"http://127.0.0.1:1/v1","models":["synthetic"],"defaultModel":"synthetic","local":true}],"defaultConnection":"local"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := app.ConfigureChat(cfg); err != nil {
		t.Fatal(err)
	}
	return app
}

// Publish fixture privacy changes without mutating a configuration retained by
// the running index worker. Synthetic providers remain installed unchanged.
func setChatFixtureLocalOnly(t *testing.T, app *Server, localOnly bool) {
	t.Helper()
	app.chat.Mu.Lock()
	defer app.chat.Mu.Unlock()
	config := *app.chat.Config
	config.LocalOnly = localOnly
	app.chat.Config = &config
}

// setChatConnection installs connection, and a new provider for it, in place
// of the configured connection with its ID, and returns the provider. Like
// setChatFixtureLocalOnly it publishes a new configuration under the runtime's
// lock, because the running search index worker reads the runtime through
// Runtime.Snapshot.
func setChatConnection(t *testing.T, app *Server, connection chat.Connection) *chat.Provider {
	t.Helper()
	provider, err := chat.NewProvider(connection)
	if err != nil {
		t.Fatalf("connection %s: %v", connection.ID, err)
	}
	app.chat.Mu.Lock()
	defer app.chat.Mu.Unlock()
	config := *app.chat.Config
	config.Connections = slices.Clone(config.Connections)
	i := slices.IndexFunc(config.Connections, func(c chat.Connection) bool { return c.ID == connection.ID })
	if i < 0 {
		t.Fatalf("connection %s is not configured", connection.ID)
	}
	config.Connections[i] = connection
	app.chat.Config = &config
	app.chat.Providers[connection.ID] = provider
	return provider
}

// The search index worker reads the chat runtime through chatEmbeddingPolicy
// while a test installs a connection. A provider written without the lock can
// crash the reader ("concurrent map clone and map write"), and -race reports a
// change to a configuration that a reader already holds.
func TestSetChatConnectionWhileIndexWorkerReadsPolicy(t *testing.T) {
	app := chatFixture(t)
	before := app.chat.Snapshot()
	original := before.Config.Connections[0]
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			app.chatEmbeddingPolicy(nil)
			_ = app.chat.Snapshot().Config.Connections[0].Limits.ContextTokens
		}
	}()
	connection := original
	for i := range 200 {
		connection.Limits.ContextTokens = 8000 + i
		setChatConnection(t, app, connection)
	}
	close(stop)
	<-done
	after := app.chat.Snapshot()
	if before.Config.Connections[0].Limits != original.Limits || before.Providers[original.ID] == after.Providers[original.ID] {
		t.Fatal("installation changed a configuration a reader held, or kept its provider")
	}
	if after.Config.Connections[0].Limits.ContextTokens != 8199 {
		t.Fatal("last connection not installed", after.Config.Connections[0].Limits)
	}
}

func TestChatHTTPPrivateScopeAndRevocation(t *testing.T) {
	app := chatFixture(t)
	w := proxyCall(app, "alice", "POST", "/p/default/api/chat", `{"connection":"local","model":"synthetic","mode":"ask"}`)
	if w.Code != 200 {
		t.Fatalf("reader cannot create private chat: %d %s", w.Code, w.Body)
	}
	var c chat.Conversation
	if err := json.Unmarshal(w.Body.Bytes(), &c); err != nil || c.ID == "" {
		t.Fatalf("missing conversation %s %v", w.Body, err)
	}
	path := "/p/default/api/chat/" + c.ID
	for _, user := range []string{"alice", "bob"} {
		w = proxyCall(app, user, "GET", path, "")
		if user == "alice" && w.Code != 200 || user == "bob" && w.Code != 404 {
			t.Errorf("%s read %d %s", user, w.Code, w.Body)
		}
	}
	if w = call(app, "GET", path, "", testToken); w.Code != 403 {
		t.Errorf("recovery token can access personal chat %d", w.Code)
	}
	w = proxyCall(app, "bob", "GET", "/p/default/api/chat", "")
	if w.Code != 200 || strings.Contains(w.Body.String(), c.ID) {
		t.Fatal("list leaks another user's conversation")
	}
	if err := app.catalog.Member(catalog.Bootstrap, "default", "alice", ""); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "/events?after=0"} {
		w = proxyCall(app, "alice", "GET", path+suffix, "")
		if w.Code != 403 {
			t.Errorf("retained access after revoke %d", w.Code)
		}
	}
}
func TestChatRejectsUnconfiguredModelsAndUnknownFields(t *testing.T) {
	app := chatFixture(t)
	for _, body := range []string{`{"connection":"cloud","model":"synthetic","mode":"ask"}`, `{"connection":"local","model":"invented","mode":"ask"}`, `{"connection":"local","model":"synthetic","mode":"ask","baseURL":"https://evil.example"}`} {
		w := proxyCall(app, "alice", "POST", "/p/default/api/chat", body)
		if w.Code != 400 {
			t.Errorf("unsafe selection %d %s", w.Code, w.Body)
		}
	}
	w := proxyCall(app, "alice", "GET", "/api/chat/connections", "")
	if w.Code != 200 || strings.Contains(w.Body.String(), "127.0.0.1") || strings.Contains(w.Body.String(), "secretFile") {
		t.Fatalf("unsafe connection metadata: %d %s", w.Code, w.Body)
	}
}

func TestChatCreateAndSubmitApplySameOptionValidation(t *testing.T) {
	app := chatFixture(t)
	w := proxyCall(app, "alice", "POST", "/p/default/api/chat", `{"mode":"ask"}`)
	var c chat.Conversation
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &c) != nil {
		t.Fatal(w.Code, w.Body)
	}
	for _, options := range []string{`{"mode":"unknown"}`, `{"mode":"ask","actions":["unknown"]}`, `{"mode":"ask","actions":["notes","live","comments","review","notes"]}`} {
		for path, body := range map[string]string{
			"/p/default/api/chat":                    options,
			"/p/default/api/chat/" + c.ID + "/turns": `{"submissionId":"invalid-options","text":"hello","options":` + options + `}`,
		} {
			w := proxyCall(app, "alice", "POST", path, body)
			if w.Code != 400 {
				t.Errorf("invalid options accepted at %s: %d %s", path, w.Code, w.Body)
			}
		}
	}
}
