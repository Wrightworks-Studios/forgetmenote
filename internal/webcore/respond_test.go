package webcore

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestRespondWritesTheResultOrOnlyTheError(t *testing.T) {
	w := httptest.NewRecorder()
	Respond(w, map[string]int{"notes": 2}, nil)
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/json" || w.Body.String() != "{\"notes\":2}\n" {
		t.Fatalf("result: %d %q %s", w.Code, w.Header().Get("Content-Type"), w.Body)
	}
	w = httptest.NewRecorder()
	Respond(w, map[string]int{"notes": 2}, Invalid("choose a folder"))
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != 400 || body["code"] != "invalid_request" || body["error"] != "choose a folder" || len(body) != 3 {
		t.Fatalf("error: %d %s", w.Code, w.Body)
	}
}
