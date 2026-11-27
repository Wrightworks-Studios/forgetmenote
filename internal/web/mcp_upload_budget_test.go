package web

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"forgetmenote/internal/workspace"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func uploadBudgetClient(t *testing.T, app *Server, legacy bool) *mcp.ClientSession {
	t.Helper()
	ts := httptest.NewServer(app)
	t.Cleanup(ts.Close)
	app.origin = ts.URL
	client := mcp.NewClient(&mcp.Implementation{Name: "upload-budget", Version: "1"}, nil)
	if legacy {
		client.AddSendingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
			return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
				if method == "server/discover" {
					return nil, &jsonrpc.Error{Code: -32601, Message: "legacy client"}
				}
				return next(ctx, method, req)
			}
		})
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL + "/mcp", HTTPClient: &http.Client{Transport: tokenTransport{}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

func TestMCPSourceUploadsUseConfiguredBudget(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		name := "stateless"
		if legacy {
			name = "legacy"
		}
		t.Run(name, func(t *testing.T) {
			app, _ := fixture(t)
			client := uploadBudgetClient(t, app, legacy)
			comment := append([]byte("<!--"), bytes.Repeat([]byte("x"), 1<<20)...)
			comment = append(comment, []byte("-->")...)
			data := append([]byte("<p>Large original acceptance evidence.</p>"), bytes.Repeat(comment, 6)...)
			raw := actionCall(t, client, "archive_source", map[string]any{"project": "default", "filename": "large.html", "data": base64.StdEncoding.EncodeToString(data)}, false)
			var note noteOutput
			if err := json.Unmarshal(raw, &note); err != nil || note.Metadata.Source == nil {
				t.Fatal("missing source record", err)
			}
			if note.Metadata.Source.SHA256 != workspace.ContentHash(data) {
				t.Fatal("original hash changed")
			}
			download, err := (&http.Client{Transport: tokenTransport{}}).Get(app.origin + "/p/default/assets/" + note.Metadata.Source.Original + "?revision=" + note.Revision)
			if err != nil {
				t.Fatal(err)
			}
			defer download.Body.Close()
			original, err := io.ReadAll(io.LimitReader(download.Body, int64(len(data)+1)))
			if err != nil || download.StatusCode != 200 || !bytes.Equal(original, data) {
				t.Fatalf("original download returned %d or changed bytes: %v", download.StatusCode, err)
			}
		})
	}
}

func TestMCPOperatorUploadLimitsRemainBounded(t *testing.T) {
	for _, key := range []string{"FMN_LIMIT_FILE_BYTES", "FMN_LIMIT_SOURCE_BYTES", "FMN_LIMIT_SOURCE_TEXT_BYTES", "FMN_LIMIT_BUNDLE_BYTES"} {
		t.Setenv(key, "4096")
	}
	for _, legacy := range []bool{false, true} {
		name := "stateless"
		if legacy {
			name = "legacy"
		}
		t.Run(name, func(t *testing.T) {
			app, _ := fixture(t)
			client := uploadBudgetClient(t, app, legacy)
			// Small envelopes still reach the independent decoded-source limit.
			actionCall(t, client, "archive_source", map[string]any{"project": "default", "filename": "large.txt", "data": base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("x"), 5000))}, true)
			result, err := client.CallTool(t.Context(), &mcp.CallToolParams{Name: "archive_source", Arguments: map[string]any{"project": "default", "filename": "huge.txt", "data": base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("x"), 2<<20))}})
			if err == nil || !strings.Contains(err.Error(), "Request Entity Too Large") {
				t.Fatalf("oversized request reached action execution: %+v", result)
			}
			// The SDK closes its client after a transport error; the server remains healthy.
			if response := call(app, "GET", "/healthz", "", testToken); response.Code != 200 {
				t.Fatalf("server unhealthy after rejected upload: %d", response.Code)
			}
		})
	}
}
