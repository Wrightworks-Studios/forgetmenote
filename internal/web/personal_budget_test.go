package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"forgetmenote/internal/automation"
	"forgetmenote/internal/catalog"
)

func TestPersonalBatchBudgetAppliesToItemsWithinProviderPages(t *testing.T) {
	var calls, questions atomic.Int32
	app, _ := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"{\"title\":\"Prepare for meeting\",\"summary\":\"Review the agenda\"}"},"finish_reason":"stop"}]}`))
	})
	configureSyntheticJev(t, app, func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Questions map[string]any `json:"questions"`
		}
		json.NewDecoder(r.Body).Decode(&request)
		questions.Add(int32(len(request.Questions)))
		answers := map[string]any{}
		for key := range request.Questions {
			answers[key] = map[string]any{"type": "noul", "noul": .95}
		}
		json.NewEncoder(w).Encode(map[string]any{"model": "jev-1.13.0", "answers": answers, "usage": map[string]int{"input_tokens": 40, "output_tokens": 4}})
	})
	app.catalog.SetAssistantPolicy(catalog.Bootstrap, catalog.AssistantPolicy{Enabled: true, AIConnection: "local", Model: "synthetic", BatchItems: 1})
	settings, err := app.catalog.SaveAssistant("alice", catalog.AssistantSettings{Enabled: true, ExternalProcessing: true, TimeZone: "UTC", Features: []string{"calendar"}})
	if err != nil {
		t.Fatal(err)
	}
	events := []map[string]any{}
	for i := range 3 {
		events = append(events, map[string]any{"id": fmt.Sprint(i), "etag": fmt.Sprint(i), "summary": "Team meeting", "start": map[string]string{"dateTime": time.Now().Add(time.Duration(i+1) * time.Hour).UTC().Format(time.RFC3339)}})
	}
	connectPersonalFixture(t, app, "alice", func(r *http.Request) string {
		switch r.URL.Path {
		case "/token":
			return `{"access_token":"synthetic-access","refresh_token":"synthetic-refresh","expires_in":3600,"scope":"openid email https://www.googleapis.com/auth/calendar.events"}`
		case "/v1/userinfo":
			return `{"sub":"google-alice","email":"alice@example.test","email_verified":true}`
		case "/calendar/v3/calendars/primary/events":
			raw, _ := json.Marshal(map[string]any{"items": events})
			return string(raw)
		default:
			for _, event := range events {
				if strings.HasSuffix(r.URL.Path, "/events/"+event["id"].(string)) {
					raw, _ := json.Marshal(event)
					return string(raw)
				}
			}
			t.Errorf("unexpected provider path %s", r.URL.Path)
			return `{}`
		}
	})
	spec, err := app.personalSpec("alice", settings)
	if err != nil {
		t.Fatal(err)
	}
	if err = app.runAutomation(t.Context(), &automation.Run{Spec: spec}, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	_, state, err := app.personal.ReadState(spec)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || questions.Load() != 1 || len(state.Pending) != 2 {
		t.Fatalf("batch limit exceeded or remaining work lost: Luna=%d Jev=%d pending=%d", calls.Load(), questions.Load(), len(state.Pending))
	}
	for completed := 2; completed <= 3; completed++ {
		if err = app.runAutomation(t.Context(), &automation.Run{Spec: spec}, func() error { return nil }); err != nil {
			t.Fatal(err)
		}
		_, state, err = app.personal.ReadState(spec)
		if err != nil {
			t.Fatal(err)
		}
		if calls.Load() != int32(completed) || questions.Load() != int32(completed) || len(state.Pending) != 3-completed {
			t.Fatalf("queued work did not drain within budget: Luna=%d Jev=%d pending=%d", calls.Load(), questions.Load(), len(state.Pending))
		}
	}
}
