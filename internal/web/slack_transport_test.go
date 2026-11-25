package web

import (
	"encoding/json"
	"forgetmenote/internal/automation"
	"forgetmenote/internal/messaging"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSlackTransportExtractsOnlyFilesSharedWithTheDestination(t *testing.T) {
	app := chatFixture(t)
	configureSlackFixture(t, app)
	shared := true
	downloads := 0
	fileURL := ""
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-bot" {
			t.Error("missing file credential")
		}
		if r.URL.Path == "/files.info" {
			channels := []string{"C123"}
			if !shared {
				channels = []string{"Cprivate"}
			}
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "file": map[string]any{"id": "F123", "name": "evidence.txt", "size": 14, "url_private_download": fileURL, "permalink": "https://example.slack.com/files/U123/F123", "channels": channels}})
			return
		}
		downloads++
		w.Write([]byte("Known evidence"))
	}))
	defer upstream.Close()
	fileURL = upstream.URL + "/file"
	app.slack.endpoint = upstream.URL + "/chat.postEphemeral"
	d := messaging.Address{Channel: "C123", User: "U123", Files: []messaging.File{{ID: "F123"}}}
	input, err := app.slack.Context(t.Context(), d, messaging.Binding{Attachments: true})
	if err != nil || len(input.Documents) != 1 || !strings.Contains(input.Documents[0].Text, "Known evidence") || input.Documents[0].Hash == "" {
		t.Fatal(input, err)
	}
	shared = false
	if _, err = app.slack.Context(t.Context(), d, messaging.Binding{Attachments: true}); err == nil || downloads != 1 {
		t.Fatal("private file crossed channel boundary", err, downloads)
	}
	shared = true
	fileURL = "http://169.254.169.254/latest/meta-data/"
	if _, err = app.slack.Context(t.Context(), d, messaging.Binding{Attachments: true}); err == nil || downloads != 1 {
		t.Fatal("untrusted file URL fetched", err)
	}
}

func TestSlackTransportUploadsFilesAndNeverRepeatsAmbiguousDelivery(t *testing.T) {
	app := chatFixture(t)
	configureSlackFixture(t, app)
	calls := []string{}
	var upload string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.URL.Path)
		switch r.URL.Path {
		case "/files.getUploadURLExternal":
			if r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" || r.ParseForm() != nil || r.Method != "POST" || r.PostForm.Get("filename") != "result.csv" || r.PostForm.Get("length") != "4" {
				t.Error("upload URL must use Slack's form POST contract", r.Method, r.Header.Get("Content-Type"), r.PostForm)
				w.Write([]byte(`{"ok":false,"error":"invalid_arguments"}`))
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "file_id": "Fupload", "upload_url": upload})
		case "/upload":
			if r.Header.Get("Authorization") != "" {
				t.Error("credential sent to upload URL")
			}
			raw, _ := io.ReadAll(r.Body)
			if string(raw) != "x\n7\n" {
				t.Error(string(raw))
			}
			w.Write([]byte("OK"))
		case "/files.completeUploadExternal":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if body["channel_id"] != "C123" || body["thread_ts"] != "1.2" || body["initial_comment"] != "Result" {
				t.Error(body)
			}
			w.Write([]byte(`{"ok":true}`))
		default:
			t.Error("unexpected API", r.URL.Path)
		}
	}))
	defer upstream.Close()
	upload = upstream.URL + "/upload"
	app.slack.endpoint = upstream.URL + "/chat.postEphemeral"
	r := automation.Run{}
	d := messaging.Address{Channel: "C123", Thread: "1.2", Shared: true}
	reply := messaging.Reply{Text: "Result", Artifacts: []messaging.Artifact{{Name: "result.csv", MIME: "text/csv", Data: []byte("x\n7\n")}}}
	if err := app.sendMessage(t.Context(), app.slack, d, reply, &r, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 3 || r.DeliveryState != "sent" {
		t.Fatal(calls, r.DeliveryState)
	}
	r.DeliveryState = "sending"
	if err := app.sendMessage(t.Context(), app.slack, d, reply, &r, func() error { return nil }); err == nil || len(calls) != 3 {
		t.Fatal("ambiguous file delivery was resent", err)
	}
}

func TestSlackTransportReportsRateLimitWithoutRetryOrResponseSecrets(t *testing.T) {
	app := chatFixture(t)
	configureSlackFixture(t, app)
	posts := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts++
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(429)
		w.Write([]byte(`{"ok":false,"error":"ratelimited","private":"do not expose"}`))
	}))
	defer upstream.Close()
	app.slack.endpoint = upstream.URL + "/chat.postEphemeral"
	err := app.slack.Send(t.Context(), messaging.Address{Channel: "D123", Direct: true}, messaging.Reply{Text: "Answer"})
	if err == nil || posts != 1 || !strings.Contains(err.Error(), "HTTP 429") || !strings.Contains(err.Error(), "30 seconds") || strings.Contains(err.Error(), "do not expose") {
		t.Fatal(err, posts)
	}
}

func TestSlackTransportDoesNotTurnAnswerTextIntoMentions(t *testing.T) {
	app := chatFixture(t)
	configureSlackFixture(t, app)
	var delivered string
	var blocks []map[string]string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Text   string
			Blocks []map[string]string
		}
		json.NewDecoder(r.Body).Decode(&body)
		delivered = body.Text
		blocks = body.Blocks
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()
	app.slack.endpoint = upstream.URL + "/chat.postEphemeral"
	if err := app.slack.Send(t.Context(), messaging.Address{Channel: "C123", Shared: true}, messaging.Reply{Text: "**Source** [evidence](https://example.com/evidence) says <!channel> and <@U999>"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(delivered, "<!") || strings.Contains(delivered, "<@") {
		t.Fatal("generated content can notify additional people", delivered)
	}
	if len(blocks) != 1 || blocks[0]["type"] != "markdown" || blocks[0]["text"] != delivered {
		t.Fatal("answer links and emphasis must render as standard Markdown, with mentions still escaped", blocks)
	}
}
