package web

import (
	"encoding/json"
	"testing"
)

func TestInboxMalformedRequestsReturnStableJSONErrors(t *testing.T) {
	app := chatFixture(t)
	for _, body := range []string{
		`{"operationId":"malformed","content":"Private draft","unknown":true}`,
		`{"operationId":"malformed","content":"Private draft"} {}`,
		`{"operationId":"malformed","content":"Private draft"`,
	} {
		response := proxyCall(app, "alice", "POST", "/api/quicknotes", body)
		var payload struct {
			Error   string `json:"error"`
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		if response.Code != 400 || response.Header().Get("Content-Type") != "application/json" || json.Unmarshal(response.Body.Bytes(), &payload) != nil || payload.Code != "invalid_request" || payload.Message == "" || payload.Error != payload.Message {
			t.Fatalf("malformed request did not return stable legacy-compatible JSON error: %d %s", response.Code, response.Body)
		}
	}
	summary := proxyCall(app, "alice", "GET", "/api/quicknotes?summary=1", "")
	var payload struct {
		InboxCount int `json:"inboxCount"`
	}
	if summary.Code != 200 || json.Unmarshal(summary.Body.Bytes(), &payload) != nil || payload.InboxCount != 0 {
		t.Fatal("invalid request changed Inbox", summary.Code, summary.Body)
	}
}

func TestAPIAuthenticationFailureUsesJSONErrorContract(t *testing.T) {
	app, _ := fixture(t)
	response := call(app, "GET", "/api/me", "", "")
	var detail struct {
		Error   string `json:"error"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if response.Code != 401 || response.Header().Get("Content-Type") != "application/json" || json.Unmarshal(response.Body.Bytes(), &detail) != nil || detail.Code != "unauthenticated" || detail.Message == "" || detail.Error != detail.Message {
		t.Fatalf("API authentication failure is not stable JSON: status=%d body=%s", response.Code, response.Body)
	}
}

func TestAPIErrorEnvelopeDoesNotChangeAssetErrors(t *testing.T) {
	app, _ := fixture(t)
	for _, path := range []string{"/p/default/assets/assets/manual.txt", "/p/default/assets/assets/api/manual.txt"} {
		response := call(app, "GET", path, "", "")
		if response.Code != 401 || response.Header().Get("Content-Type") != "text/plain; charset=utf-8" || response.Body.String() != "Authentication required\n" {
			t.Fatalf("asset request treated as API: path=%s status=%d body=%s", path, response.Code, response.Body)
		}
	}
}
