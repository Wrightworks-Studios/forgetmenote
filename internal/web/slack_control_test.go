package web

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func signedSlackMessage(t *testing.T, app *Server, id, kind, channel, thread, message, text string) {
	t.Helper()
	if w := signedSlackEvent(app, id, kind, channel, thread, message, text); w.Code != 200 {
		t.Fatalf("Slack %s: HTTP %d %s", id, w.Code, w.Body)
	}
}

func signedSlackEvent(app *Server, id, kind, channel, thread, message, text string) *httptest.ResponseRecorder {
	payload := map[string]any{"type": "event_callback", "team_id": "T123", "event_id": id, "event": map[string]any{"type": kind, "channel": channel, "channel_type": "channel", "user": "U123", "text": text, "ts": message, "thread_ts": thread}}
	if strings.HasPrefix(channel, "D") {
		payload["event"].(map[string]any)["channel_type"] = "im"
	}
	raw, _ := json.Marshal(payload)
	stamp := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte("fixture-signing"))
	mac.Write([]byte("v0:" + stamp + ":" + string(raw)))
	r := httptest.NewRequest("POST", "http://localhost/integrations/slack/events", strings.NewReader(string(raw)))
	r.Header.Set("X-Slack-Request-Timestamp", stamp)
	r.Header.Set("X-Slack-Signature", "v0="+hex.EncodeToString(mac.Sum(nil)))
	w := httptest.NewRecorder()
	app.ServeHTTP(w, r)
	return w
}

func TestSlackChannelThreadRequiresExplicitMention(t *testing.T) {
	app, _ := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Ready."},"finish_reason":"stop"}]}`))
	})
	configureSlackFixture(t, app)
	app.messageClients["slack"] = &fixtureChatTransport{}
	signedSlackMessage(t, app, "start", "app_mention", "C123", "", "1.000001", "Please help.")
	if _, err := app.automations.Queue.RunNext(t.Context(), app.runAutomation); err != nil {
		t.Fatal(err)
	}
	signedSlackMessage(t, app, "human", "message", "C123", "1.000001", "2.000001", "<@U456> Is that gross or net?")
	runs, err := app.automations.Queue.List()
	if err != nil || len(runs) != 1 {
		t.Fatalf("answered a human-to-human reply: %d %v", len(runs), err)
	}
	signedSlackMessage(t, app, "mentioned", "app_mention", "C123", "1.000001", "3.000001", "Please continue.")
	runs, err = app.automations.Queue.List()
	if err != nil || len(runs) != 2 {
		t.Fatalf("explicit follow-up lost: %d %v", len(runs), err)
	}
}

func TestSlackStopCancelsActiveGenerationWithoutReply(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	app, _ := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-r.Context().Done():
		case <-release:
		}
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Obsolete answer."},"finish_reason":"stop"}]}`))
	})
	defer close(release)
	configureSlackFixture(t, app)
	transport := &fixtureChatTransport{}
	app.messageClients["slack"] = transport
	signedSlackMessage(t, app, "start", "app_mention", "C123", "", "1.000001", "Please help.")
	done := make(chan error, 1)
	go func() { _, err := app.automations.Queue.RunNext(context.Background(), app.runAutomation); done <- err }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("model did not start")
	}
	signedSlackMessage(t, app, "stop", "app_mention", "C123", "1.000001", "2.000001", "<@UBOT> you can stop responding to this thread")
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stop did not cancel active generation")
	}
	if len(transport.replies) != 0 {
		t.Fatal("cancelled request posted a reply or failure", transport.replies)
	}
	if ran, err := app.automations.Queue.RunNext(t.Context(), app.runAutomation); err != nil || ran {
		t.Fatal("stop request invoked the model", ran, err)
	}
}
