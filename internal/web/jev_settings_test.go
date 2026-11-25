package web

import (
	"encoding/json"
	"testing"
)

func TestJevPersonalSettingsArePrivateAndOffUntilConsent(t *testing.T) {
	app := chatFixture(t)
	w := proxyCall(app, "alice", "GET", "/api/jev/settings", "")
	if w.Code != 200 {
		t.Fatalf("missing Jev settings: %d %s", w.Code, w.Body.String())
	}
	var result struct {
		Available bool `json:"available"`
		Settings  struct {
			Consent   bool `json:"consent"`
			Rerank    bool `json:"rerank"`
			Citations bool `json:"citations"`
		} `json:"settings"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || result.Available || result.Settings.Consent {
		t.Fatalf("remote processing enabled without configuration/consent: %s", w.Body.String())
	}
	w = proxyCall(app, "alice", "POST", "/api/jev/settings", `{"consent":true,"rerank":true,"citations":true}`)
	if w.Code != 200 {
		t.Fatalf("save personal settings: %d %s", w.Code, w.Body.String())
	}
	w = proxyCall(app, "bob", "GET", "/api/jev/settings", "")
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || result.Settings.Consent {
		t.Fatal("shared private feature consent across users")
	}
	w = proxyCall(app, "alice", "GET", "/api/jev/settings", "")
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || !result.Settings.Consent {
		t.Fatal("lost saved consent")
	}
}
