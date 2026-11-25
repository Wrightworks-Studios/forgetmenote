package web

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/workspace"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestHostedSourceRequiresInstancePolicy(t *testing.T) {
	app, _ := fixture(t)
	raw, err := os.ReadFile("../sources/testdata/reference.png")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"filename": "image.png", "data": base64.StdEncoding.EncodeToString(raw), "describeImages": true})
	response := call(app, "POST", "/api/sources/jobs", string(body), testToken)
	if response.Code != 403 {
		t.Fatalf("hosted source option must be denied before admission without instance policy: %d %s", response.Code, response.Body.String())
	}
}

func hostedSourceFixture(t *testing.T, handler http.HandlerFunc) (*Server, *workspace.Store) {
	t.Helper()
	app, store := fixture(t)
	// A provider reads the whole request before it answers. Answering first closes
	// the connection with unread bytes, which Linux turns into a reset the client
	// may see while still writing, as a request of unknown outcome.
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(request))
		handler(w, r)
	}))
	t.Cleanup(provider.Close)
	raw, _ := json.Marshal(map[string]any{"sources": map[string]any{"connection": "source", "model": "synthetic-vision", "transcriptionModel": "synthetic-transcribe"}, "connections": []any{map[string]any{"id": "source", "label": "Synthetic source processor", "format": "responses", "baseURL": provider.URL, "models": []string{"synthetic-vision"}, "local": true, "capabilities": map[string]bool{"images": true}, "limits": map[string]int{"contextTokens": 32768, "outputTokens": 2048}}}})
	file := filepath.Join(t.TempDir(), "ai.json")
	if err := os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := app.ConfigureAI(file, ""); err != nil {
		t.Fatal(err)
	}
	settings, _ := app.catalog.AISettings()
	settings.SourceOCR = true
	settings.SourceDescriptions = true
	settings.SourceTranscription = true
	if _, err := app.catalog.SetAISettings(catalog.Bootstrap, settings); err != nil {
		t.Fatal(err)
	}
	return app, store
}
func syntheticVision(w http.ResponseWriter, text string) {
	result, _ := json.Marshal(map[string]any{"partial": false, "sections": []any{map[string]any{"number": 1, "blocks": []any{map[string]any{"kind": "text", "text": text, "rows": [][]string{}}}}}})
	json.NewEncoder(w).Encode(map[string]any{"status": "completed", "model": "synthetic-vision", "usage": map[string]any{"input_tokens": 37, "output_tokens": 11, "input_tokens_details": map[string]int{"cached_tokens": 0}}, "output": []any{map[string]any{"type": "message", "content": []any{map[string]string{"type": "output_text", "text": string(result)}}}}})
}
func sourceJobUntilFinished(t *testing.T, app *Server, id string) sourceJobOutput {
	t.Helper()
	until := time.Now().Add(5 * time.Second)
	for time.Now().Before(until) {
		response := call(app, "GET", "/api/sources/jobs/"+id, "", testToken)
		if response.Code != 200 {
			t.Fatal(response.Code, response.Body.String())
		}
		var out sourceJobOutput
		if err := json.Unmarshal(response.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if out.Job.State != "running" && out.Job.State != "queued" {
			return out
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("source job did not finish")
	return sourceJobOutput{}
}
func submitHostedImage(t *testing.T, app *Server) string {
	t.Helper()
	raw, err := os.ReadFile("../sources/testdata/reference.png")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"filename": "image.png", "data": base64.StdEncoding.EncodeToString(raw), "externalOCR": true})
	response := call(app, "POST", "/api/sources/jobs", string(body), testToken)
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	var out sourceJobOutput
	json.Unmarshal(response.Body.Bytes(), &out)
	return out.Job.ID
}
func TestHostedSourceExplicitOCRReusesPaidResultAtSave(t *testing.T) {
	requireLocalOCR(t) // its source is an image
	var calls atomic.Int32
	app, _ := hostedSourceFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/responses" {
			t.Error("wrong hosted endpoint", r.URL.Path)
		}
		syntheticVision(w, "Cobalt remote table [link](javascript:evil)")
	})
	id := submitHostedImage(t, app)
	out := sourceJobUntilFinished(t, app, id)
	if out.Prepared == nil || !strings.Contains(out.Prepared.Preview, "Cobalt remote table") || strings.Contains(out.Prepared.Preview, "[link](") || !out.Job.ResumeBlocked {
		t.Fatalf("hosted OCR result: %+v", out)
	}
	body, _ := json.Marshal(map[string]any{"job": id})
	saved := call(app, "POST", "/api/sources/import", string(body), testToken)
	if saved.Code != 200 {
		t.Fatal(saved.Code, saved.Body.String())
	}
	if calls.Load() != 1 {
		t.Fatal("saving repeated paid extraction", calls.Load())
	}
	records, err := app.chat.JevLedger.Records()
	if err != nil || len(records) != 1 || records[0].Usage == nil || records[0].Usage.InputTokens != 37 || records[0].Status != "settled" {
		t.Fatalf("hosted spending receipt: %+v %v", records, err)
	}

}
func TestHostedSourceRevokedPolicyRejectsAcceptance(t *testing.T) {
	requireLocalOCR(t) // its source is an image
	var app *Server
	var calls atomic.Int32
	app, _ = hostedSourceFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		settings, _ := app.catalog.AISettings()
		settings.SourceOCR = false
		if _, err := app.catalog.SetAISettings(catalog.Bootstrap, settings); err != nil {
			t.Error(err)
		}
		syntheticVision(w, "MUST_NOT_BE_ACCEPTED")
	})
	id := submitHostedImage(t, app)
	out := sourceJobUntilFinished(t, app, id)
	if out.Prepared != nil || out.Job.State != "failed" || calls.Load() != 1 {
		t.Fatalf("revoked remote response accepted: %+v", out)
	}
}
func TestHostedSourceUnknownRequestCannotResume(t *testing.T) {
	requireLocalOCR(t) // its source is an image
	var calls atomic.Int32
	app, _ := hostedSourceFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		connection, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		connection.Close()
	})
	id := submitHostedImage(t, app)
	out := sourceJobUntilFinished(t, app, id)
	if out.Prepared != nil || out.Job.State != "failed" || !out.Job.ResumeBlocked || calls.Load() != 1 {
		t.Fatalf("unknown paid state: %+v", out)
	}
	app.sourceJobs.Close()
	if err := app.initSourceJobs(); err != nil {
		t.Fatal(err)
	}
	persisted := sourceJobUntilFinished(t, app, id)
	if !persisted.Job.ResumeBlocked {
		t.Fatal("restart lost paid attempt fence")
	}
	resumed := call(app, "POST", "/api/sources/jobs/"+id+"/resume", "{}", testToken)
	records, err := app.chat.JevLedger.Records()
	if err != nil || len(records) != 1 || records[0].Status != "unknown" {
		t.Fatalf("uncertain spending receipt: %+v %v", records, err)
	}
	if resumed.Code == 200 || calls.Load() != 1 {
		t.Fatal("uncertain upload replayed", resumed.Code, calls.Load())
	}
}

func TestHostedSourceLocalOnlyMakesNoCalls(t *testing.T) {
	var calls atomic.Int32
	app, _ := hostedSourceFixture(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); syntheticVision(w, "unreachable") })
	setChatFixtureLocalOnly(t, app, true)
	raw, _ := os.ReadFile("../sources/testdata/reference.png")
	body, _ := json.Marshal(map[string]any{"filename": "image.png", "data": base64.StdEncoding.EncodeToString(raw), "externalOCR": true})
	response := call(app, "POST", "/api/sources/jobs", string(body), testToken)
	if response.Code != 403 || calls.Load() != 0 {
		t.Fatal("local-only source processing uploaded", response.Code, calls.Load())
	}
}
func TestHostedSourceInboxConsentGatesUploadAndAcceptance(t *testing.T) {
	requireLocalOCR(t) // its source is an image
	var app *Server
	var calls atomic.Int32
	app, _ = hostedSourceFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		settings, err := app.quicknotes.Settings(catalog.Bootstrap)
		if err != nil {
			t.Error(err)
			return
		}
		settings.CloudConsent = false
		if _, err := app.quicknotes.UpdateSettings(catalog.Bootstrap, settings); err != nil {
			t.Error(err)
		}
		syntheticVision(w, "private response must be discarded")
	})
	inbox, err := app.catalog.EnsureInbox(catalog.Bootstrap)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile("../sources/testdata/reference.png")
	body, _ := json.Marshal(map[string]any{"filename": "image.png", "data": base64.StdEncoding.EncodeToString(raw), "externalOCR": true})
	endpoint := "/p/" + inbox.ID + "/api/sources/jobs"
	denied := call(app, "POST", endpoint, string(body), testToken)
	if denied.Code != 403 || calls.Load() != 0 {
		t.Fatal("private upload lacked consent", denied.Code, calls.Load())
	}
	settings, err := app.quicknotes.Settings(catalog.Bootstrap)
	if err != nil {
		t.Fatal(err)
	}
	settings.CloudConsent = true
	if _, err := app.quicknotes.UpdateSettings(catalog.Bootstrap, settings); err != nil {
		t.Fatal(err)
	}
	accepted := call(app, "POST", endpoint, string(body), testToken)
	if accepted.Code != 200 {
		t.Fatal(accepted.Code, accepted.Body.String())
	}
	var created sourceJobOutput
	json.Unmarshal(accepted.Body.Bytes(), &created)
	until := time.Now().Add(5 * time.Second)
	var out sourceJobOutput
	for time.Now().Before(until) {
		status := call(app, "GET", endpoint+"/"+created.Job.ID, "", testToken)
		if status.Code != 200 {
			t.Fatal(status.Code, status.Body.String())
		}
		json.Unmarshal(status.Body.Bytes(), &out)
		if out.Job.State == "failed" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if out.Prepared != nil || out.Job.State != "failed" || calls.Load() != 1 {
		t.Fatalf("revoked private result accepted: %+v calls=%d", out, calls.Load())
	}
}
