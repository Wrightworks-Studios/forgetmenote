package web

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"forgetmenote/internal/catalog"
)

// chatVision is a fake provider for the instance's chat connection. It
// counts the requests it receives and answers each with one transcription.
type chatVision struct {
	calls  atomic.Int32
	mu     sync.Mutex
	bodies []string
}

func (v *chatVision) body(i int) string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.bodies[i]
}

// chatTextFixture is an instance whose chat connection can read images when
// images is true, with alice a reader of the default notebook and the
// instance AI policy letting the chat read uploaded images.
func chatTextFixture(t *testing.T, images bool) (*Server, *chatVision) {
	t.Helper()
	return chatTextInstance(t, images, true)
}

// chatTextInstance adopts an instance AI policy only when adopt is true.
func chatTextInstance(t *testing.T, images, adopt bool) (*Server, *chatVision) {
	t.Helper()
	app, _ := fixture(t)
	if err := app.ConfigureProxy(proxyTestSecret, []string{"127.0.0.1/32"}); err != nil {
		t.Fatal(err)
	}
	if err := app.catalog.Member(catalog.Bootstrap, "default", "alice", "read"); err != nil {
		t.Fatal(err)
	}
	vision := &chatVision{}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		vision.mu.Lock()
		vision.bodies = append(vision.bodies, string(raw))
		vision.mu.Unlock()
		vision.calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"model":"fixture-vision","choices":[{"message":{"role":"assistant","content":"HARBOUR NOTICE\nNo whaling within the harbour."},"finish_reason":"stop"}],"usage":{"prompt_tokens":812,"completion_tokens":14}}`)
	}))
	t.Cleanup(provider.Close)
	connection := map[string]any{"id": "vision", "label": "Fixture vision", "format": "chat-completions", "baseURL": provider.URL + "/v1", "models": []string{"fixture-vision"}, "defaultModel": "fixture-vision", "local": true, "capabilities": map[string]any{"images": images}, "limits": map[string]int{"contextTokens": 65536, "outputTokens": 2048}}
	raw, _ := json.Marshal(map[string]any{"connections": []any{connection}, "defaultConnection": "vision"})
	file := filepath.Join(t.TempDir(), "chat.json")
	if err := os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := app.ConfigureChat(file); err != nil {
		t.Fatal(err)
	}
	if adopt {
		if _, err := app.catalog.SetAISettings(catalog.Bootstrap, catalog.AISettings{UploadReading: true}); err != nil {
			t.Fatal(err)
		}
	}
	return app, vision
}

// uploadSource archives an original as the browser does: a preview, then
// its import.
func uploadSource(t *testing.T, app *Server, project, filename string, original []byte) noteOutput {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"filename": filename, "data": encoded(original)})
	preview := notebookCall(app, "POST", "/p/"+project+"/api/sources/preview", string(body))
	if preview.Code != 200 {
		t.Fatalf("preview %s: %d %s", filename, preview.Code, preview.Body.String())
	}
	var prepared map[string]any
	json.Unmarshal(preview.Body.Bytes(), &prepared)
	body, _ = json.Marshal(map[string]any{"prepared": prepared, "title": "Notice " + filename})
	imported := notebookCall(app, "POST", "/p/"+project+"/api/sources/import", string(body))
	var note noteOutput
	if imported.Code != 200 || json.Unmarshal(imported.Body.Bytes(), &note) != nil || note.Metadata.Source == nil {
		t.Fatalf("import %s: %d %s", filename, imported.Code, imported.Body.String())
	}
	return note
}

type chatTextOutput struct {
	Note    *noteOutput `json:"note"`
	Waiting string      `json:"waiting"`
}

func chatText(t *testing.T, w *httptest.ResponseRecorder) chatTextOutput {
	t.Helper()
	var out chatTextOutput
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
		t.Fatalf("chat text: %d %s", w.Code, w.Body.String())
	}
	return out
}

func sourceFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile("../sources/testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// A browser upload of an image that the instance's chat model can read gets
// the chat's text. The server hands the original to the model once, records
// the call as a paid step, and refreshes the source note at its revision
// through the supplied-text path, with the model named as the extractor.
func TestChatReadsAnUploadedImage(t *testing.T) {
	app, vision := chatTextFixture(t, true)
	original := sourceFixture(t, "reference.png")
	note := uploadSource(t, app, "default", "notice.png", original)
	if note.Metadata.Source.Extraction != "original-only" {
		t.Fatalf("the upload was read before the chat: %+v", note.Metadata.Source)
	}
	out := chatText(t, notebookCall(app, "POST", "/api/sources/chat-text", `{"path":"`+note.Path+`","revision":"`+note.Revision+`"}`))
	if out.Waiting != "" || out.Note == nil {
		t.Fatalf("the chat did not read the image: %+v", out)
	}
	record := out.Note.Metadata.Source
	if record == nil || record.Extraction != "client-supplied" || !strings.Contains(record.Extractor, "fixture-vision") || record.SHA256 != note.Metadata.Source.SHA256 || !strings.Contains(record.Warning, "chat") {
		t.Fatalf("record: %+v", record)
	}
	if !strings.Contains(out.Note.Content, "No whaling within the harbour.") || out.Note.Path != note.Path || out.Note.Revision == note.Revision {
		t.Fatalf("note: %s %s\n%s", out.Note.Path, out.Note.Revision, out.Note.Content)
	}
	if n := vision.calls.Load(); n != 1 || !strings.Contains(vision.body(0), `data:image/png;base64,`) {
		t.Fatalf("provider requests: %d", n)
	}
	records, err := app.chat.Snapshot().Ledger.Records()
	if err != nil || len(records) != 1 || records[0].Scope.Feature != "extraction" || records[0].Status != "settled" || records[0].Usage == nil || records[0].Usage.InputTokens != 812 {
		t.Fatalf("paid steps: %+v %v", records, err)
	}
	if listed := notebookCall(app, "GET", "/api/sources/awaiting-text", ""); strings.Contains(listed.Body.String(), note.Path) {
		t.Fatalf("the source still waits for text: %s", listed.Body.String())
	}
	if verified := notebookCall(app, "GET", "/api/sources/verify?path="+note.Path, ""); !strings.Contains(verified.Body.String(), `"originalStatus":"match"`) || !strings.Contains(verified.Body.String(), `"textStatus":"match"`) {
		t.Fatalf("verify: %s", verified.Body.String())
	}
	// A source that has its text is not read again.
	if w := notebookCall(app, "POST", "/api/sources/chat-text", `{"path":"`+note.Path+`","revision":"`+out.Note.Revision+`"}`); w.Code != 400 || vision.calls.Load() != 1 {
		t.Fatalf("a source with text was read again: %d %s", w.Code, w.Body.String())
	}
}

// Whenever the chat may not or cannot read the original, the note stays
// waiting for a connected agent and the uploader is told why. No provider is
// contacted and no paid step is recorded.
func TestChatSourceTextRefusalsContactNoProvider(t *testing.T) {
	waiting := func(t *testing.T, app *Server, vision *chatVision, project string, note noteOutput, reason string) {
		t.Helper()
		out := chatText(t, notebookCall(app, "POST", "/p/"+project+"/api/sources/chat-text", `{"path":"`+note.Path+`","revision":"`+note.Revision+`"}`))
		if out.Note != nil || !strings.Contains(out.Waiting, reason) {
			t.Fatalf("waiting: %+v, want %q", out, reason)
		}
		nobodyPaid(t, app, vision)
		if listed := notebookCall(app, "GET", "/p/"+project+"/api/sources/awaiting-text", ""); !strings.Contains(listed.Body.String(), note.Path) {
			t.Fatalf("the note no longer waits for text: %s", listed.Body.String())
		}
	}
	png := sourceFixture(t, "reference.png")

	t.Run("AI writing is on and the chat's reading of uploads off", func(t *testing.T) {
		app, vision := chatTextFixture(t, true)
		note := uploadSource(t, app, "default", "notice.png", png)
		policy, _ := app.catalog.AISettings()
		policy.Writing, policy.UploadReading = true, false
		if _, err := app.catalog.SetAISettings(catalog.Bootstrap, policy); err != nil {
			t.Fatal(err)
		}
		waiting(t, app, vision, "default", note, "does not let the chat read uploaded images")
	})
	t.Run("formats the chat cannot read", func(t *testing.T) {
		app, vision := chatTextFixture(t, true)
		for name, original := range map[string][]byte{"scan.tiff": sourceFixture(t, "reference.tiff"), "announcement.wav": sourceFixture(t, "announcement.wav"), "memo.pages": sourceFixture(t, "iwork/modern.pages"), "scan.pdf": sourceFixture(t, "scanned.pdf")} {
			waiting(t, app, vision, "default", uploadSource(t, app, "default", name, original), "PNG and JPEG images only")
		}
	})
	t.Run("the chat connection cannot read images", func(t *testing.T) {
		app, vision := chatTextFixture(t, false)
		waiting(t, app, vision, "default", uploadSource(t, app, "default", "notice.png", png), "No chat connection on this instance reads images")
	})
	t.Run("a paused instance copy", func(t *testing.T) {
		app, vision := chatTextFixture(t, true)
		note := uploadSource(t, app, "default", "notice.png", png)
		if err := app.ConfigureOutgoing("paused"); err != nil {
			t.Fatal(err)
		}
		waiting(t, app, vision, "default", note, "paused")
	})
	t.Run("a private Inbox without its owner's cloud consent", func(t *testing.T) {
		app, vision := chatTextFixture(t, true)
		inbox, err := app.catalog.EnsureInbox(catalog.Bootstrap)
		if err != nil {
			t.Fatal(err)
		}
		note := uploadSource(t, app, inbox.ID, "notice.png", png)
		waiting(t, app, vision, inbox.ID, note, "consent")
		// With the owner's consent the chat reads it.
		settings, err := app.quicknotes.Settings(catalog.Bootstrap)
		if err != nil {
			t.Fatal(err)
		}
		settings.CloudConsent = true
		if _, err = app.quicknotes.UpdateSettings(catalog.Bootstrap, settings); err != nil {
			t.Fatal(err)
		}
		out := chatText(t, notebookCall(app, "POST", "/p/"+inbox.ID+"/api/sources/chat-text", `{"path":"`+note.Path+`","revision":"`+note.Revision+`"}`))
		if out.Note == nil || vision.calls.Load() != 1 {
			t.Fatalf("the consenting owner's upload was not read: %+v", out)
		}
	})
	t.Run("a reader or a stale revision", func(t *testing.T) {
		app, vision := chatTextFixture(t, true)
		note := uploadSource(t, app, "default", "notice.png", png)
		if w := proxyCall(app, "alice", "POST", "/p/default/api/sources/chat-text", `{"path":"`+note.Path+`","revision":"`+note.Revision+`"}`); w.Code != 403 {
			t.Fatalf("a reader: %d %s", w.Code, w.Body.String())
		}
		if w := notebookCall(app, "POST", "/api/sources/chat-text", `{"path":"`+note.Path+`","revision":"0000000000000000000000000000000000000000"}`); w.Code != 409 {
			t.Fatalf("a stale revision: %d %s", w.Code, w.Body.String())
		}
		nobodyPaid(t, app, vision)
	})
	t.Run("no adopted AI policy", func(t *testing.T) {
		app, vision := chatTextInstance(t, true, false)
		waiting(t, app, vision, "default", uploadSource(t, app, "default", "notice.png", png), "does not let the chat read uploaded images")
	})
}

// nobodyPaid fails the test if the chat's provider was contacted or a paid
// step recorded.
func nobodyPaid(t *testing.T, app *Server, vision *chatVision) {
	t.Helper()
	if n := vision.calls.Load(); n != 0 {
		t.Fatalf("the provider was contacted %d times", n)
	}
	if ledger := app.chat.Snapshot().Ledger; ledger != nil {
		if records, err := ledger.Records(); err != nil || len(records) != 0 {
			t.Fatalf("paid steps: %+v %v", records, err)
		}
	}
}

// The chat's reading of uploads is its own switch in the instance AI policy,
// off until an admin turns it on, and only when a chat connection reads
// images.
func TestUploadReadingIsItsOwnInstanceAISwitch(t *testing.T) {
	setting := func(app *Server, body string) *httptest.ResponseRecorder {
		return apiCall(app, "POST", "/api/organization/action/set_ai_settings", body, testToken)
	}
	app, vision := chatTextInstance(t, false, false)
	status := apiCall(app, "POST", "/api/organization/action/ai_settings", `{}`, testToken)
	if !strings.Contains(status.Body.String(), `"uploadReading":false`) || !strings.Contains(status.Body.String(), `"images":false`) {
		t.Fatalf("status: %s", status.Body.String())
	}
	if w := setting(app, `{"config":{"revision":0,"writing":true,"uploadReading":true}}`); w.Code != 400 || !strings.Contains(w.Body.String(), "reads images") {
		t.Fatalf("turned on without a connection that reads images: %d %s", w.Code, w.Body.String())
	}
	// AI writing alone leaves it off.
	if w := setting(app, `{"config":{"revision":0,"writing":true}}`); w.Code != 200 || !strings.Contains(w.Body.String(), `"uploadReading":false`) {
		t.Fatalf("writing: %d %s", w.Code, w.Body.String())
	}
	nobodyPaid(t, app, vision)

	app, vision = chatTextInstance(t, true, false)
	if status := apiCall(app, "POST", "/api/organization/action/ai_settings", `{}`, testToken); !strings.Contains(status.Body.String(), `"images":true`) {
		t.Fatalf("status: %s", status.Body.String())
	}
	if w := setting(app, `{"config":{"revision":0,"uploadReading":true}}`); w.Code != 200 || !strings.Contains(w.Body.String(), `"uploadReading":true`) {
		t.Fatalf("upload reading: %d %s", w.Code, w.Body.String())
	}
	note := uploadSource(t, app, "default", "notice.png", sourceFixture(t, "reference.png"))
	if out := chatText(t, notebookCall(app, "POST", "/api/sources/chat-text", `{"path":"`+note.Path+`","revision":"`+note.Revision+`"}`)); out.Note == nil || vision.calls.Load() != 1 {
		t.Fatalf("the chat did not read the image: %+v", out)
	}
}
