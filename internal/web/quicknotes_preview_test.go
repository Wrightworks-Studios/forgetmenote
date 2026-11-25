package web

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestQuicknotesListUsesPlainPreview(t *testing.T) {
	app := chatFixture(t)
	content := "# Capture heading\n\nRead [the schedule](https://example.invalid) and **confirm** dates."
	body, _ := json.Marshal(map[string]string{"operationId": "plain-preview", "content": content})
	response := proxyCall(app, "alice", "POST", "/api/quicknotes", string(body))
	if response.Code != 200 {
		t.Fatalf("capture: %d %s", response.Code, response.Body)
	}
	response = proxyCall(app, "alice", "GET", "/api/quicknotes", "")
	var result struct {
		Captures []struct{ Title, Excerpt string }
	}
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &result) != nil || len(result.Captures) != 1 {
		t.Fatalf("list: %d %s", response.Code, response.Body)
	}
	capture := result.Captures[0]
	if capture.Title != "Capture heading" || capture.Excerpt != "Read the schedule and confirm dates." || strings.Contains(capture.Excerpt, "#") {
		t.Fatalf("unreadable capture preview: %+v", capture)
	}
}
