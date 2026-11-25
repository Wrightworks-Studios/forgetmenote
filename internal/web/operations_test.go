package web

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestReadinessDetectsReadOnlyStorage(t *testing.T) {
	app, _ := fixture(t)
	r := httptest.NewRequest("GET", app.origin+"/readyz", nil)
	w := httptest.NewRecorder()
	app.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("ready=%d", w.Code)
	}
	if err := os.Chmod(app.data, 0500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(app.data, 0700)
	app.readinessAt = time.Time{}
	w = httptest.NewRecorder()
	app.ServeHTTP(w, r)
	if w.Code != 503 {
		t.Fatalf("read-only ready=%d", w.Code)
	}
	files, _ := filepath.Glob(filepath.Join(app.data, ".readiness-*"))
	if len(files) > 0 {
		t.Fatal(files)
	}
}

func TestMCPLogsToolFailuresWithoutContents(t *testing.T) {
	var logs lockedLogBuffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	app, _ := fixture(t)
	client := actionClient(t, app, "@local-admin", "notes:read")
	actionCall(t, client, "read_note", map[string]any{"project": "default", "path": "private-missing-note.md"}, true)
	actionCall(t, client, "list_projects", map[string]any{}, false)
	_, err := client.CallTool(context.Background(), &mcp.CallToolParams{Name: "private-unknown-tool", Arguments: map[string]any{}})
	if err == nil {
		t.Fatal("unknown tool accepted")
	}
	client.Close()
	var outcomes []string
	for _, line := range strings.Split(logs.String(), "\n") {
		var entry map[string]any
		if json.Unmarshal([]byte(line), &entry) == nil && entry["msg"] == "mcp_tool" {
			outcomes = append(outcomes, entry["tool"].(string)+":"+entry["outcome"].(string))
		}
	}
	if strings.Join(outcomes, ",") != "read_note:error,list_projects:success,unknown:error" {
		t.Fatalf("tool outcomes=%v", outcomes)
	}
	if strings.Contains(logs.String(), "private-") {
		t.Fatal("logged private request data")
	}
}

// A tool error classified as a server failure carries its cause on the tool's
// log line, as the request log does for the browser. Invalid input, including
// arguments the SDK rejects against the tool's schema with their values quoted,
// a refusal or a missing note logs no error text.
func TestMCPLogsTheCauseOfServerFailuresOnly(t *testing.T) {
	var logs lockedLogBuffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	app, _ := fixture(t)
	client := actionClient(t, app, "@local-admin", "notes:read notes:write notes:manage")
	actionCall(t, client, "control_managed_backup", map[string]any{"project": "default", "generation": 1, "action": "pause"}, true)
	actionCall(t, client, "read_note", map[string]any{"project": "default", "path": "private-image.png"}, true)
	actionCall(t, client, "read_note", map[string]any{"project": "default", "path": "private-missing.md"}, true)
	actionCall(t, client, "save_note", map[string]any{"project": "default", "path": "diary.md", "content": []any{"private-diary: nobody knows"}}, true)
	actionCall(t, client, "read_note", map[string]any{"project": "default", "path": "diary.md", "revision": map[string]any{"private-key": "private-value"}}, true)
	client.Close()
	var outcomes []string
	for _, line := range strings.Split(logs.String(), "\n") {
		var entry map[string]any
		if json.Unmarshal([]byte(line), &entry) == nil && entry["msg"] == "mcp_tool" {
			cause, _ := entry["error"].(string)
			outcomes = append(outcomes, entry["tool"].(string)+":"+entry["outcome"].(string)+":"+cause)
		}
	}
	if want := "control_managed_backup:error:managed backups unavailable,read_note:error:,read_note:error:,save_note:error:,read_note:error:"; strings.Join(outcomes, ",") != want {
		t.Fatalf("tool outcomes=%v", outcomes)
	}
	if strings.Contains(logs.String(), "private-") {
		t.Fatal("logged private request data")
	}
}

type lockedLogBuffer struct {
	mu sync.Mutex
	bytes.Buffer
}

func (b *lockedLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.Write(p)
}
func (b *lockedLogBuffer) String() string { b.mu.Lock(); defer b.mu.Unlock(); return b.Buffer.String() }
