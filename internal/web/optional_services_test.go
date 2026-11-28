package web

import (
	"encoding/json"
	"testing"
)

// An optional service the instance does not configure answers every request
// that needs it as unavailable (503), before reading the request, whichever
// transport asks.
func TestUnconfiguredOptionalServicesAreUnavailable(t *testing.T) {
	app, _ := fixture(t)
	if app.repositories != nil || app.repositoryConnections != nil || app.backups != nil {
		t.Fatal("fixture configures optional services")
	}
	browser := []struct{ method, path, body string }{
		{"POST", "/api/repository/connect", `not json`},
		{"GET", "/api/repository/choices", ""},
		{"POST", "/api/repository/select", `not json`},
		{"POST", "/api/repository/control", `not json`},
		{"POST", "/repository/github/webhook", `not json`},
		{"POST", "/api/projects/repository/connect", `{"id":"new-source","name":"New source"}`},
		{"GET", "/api/projects/repository/choices", ""},
		{"POST", "/api/projects/repository/finish", `not json`},
		{"GET", "/api/document/asset?path=a.png", ""},
		{"POST", "/api/managed-backup/connect", `not json`},
		{"POST", "/api/managed-backup/control", `not json`},
		{"POST", "/api/managed-backup/drive/connect", `not json`},
		{"GET", "/api/managed-backup/drive/pending?kind=gdrive", ""},
		{"POST", "/api/managed-backup/drive/enable", `not json`},
		{"POST", "/api/managed-backup/drive/cancel", `not json`},
	}
	for _, request := range browser {
		w := call(app, request.method, request.path, request.body, testToken)
		var body struct{ Code string }
		if w.Code != 503 || json.Unmarshal(w.Body.Bytes(), &body) != nil || body.Code != "unavailable" {
			t.Errorf("%s %s: %d %s", request.method, request.path, w.Code, w.Body)
		}
	}
	// The package tools reported a missing repository service as invalid input.
	for _, request := range []string{
		`{"project":"default"}`,
		`{"project":"default","path":"tools/report"}`,
	} {
		w := apiCall(app, "POST", "/api/v1/actions/discover_automation_packages", request, testToken)
		var body struct{ Error struct{ Code string } }
		if w.Code != 503 || json.Unmarshal(w.Body.Bytes(), &body) != nil || body.Error.Code != "unavailable" {
			t.Errorf("discover_automation_packages %s: %d %s", request, w.Code, w.Body)
		}
	}
	w := apiCall(app, "POST", "/api/v1/actions/control_managed_backup", `{"project":"default","generation":1,"action":"pause"}`, testToken)
	if w.Code != 503 {
		t.Errorf("control_managed_backup: %d %s", w.Code, w.Body)
	}
}
