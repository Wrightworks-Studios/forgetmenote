package webcore

import (
	"errors"
	"forgetmenote/internal/workspace"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNotebookRoutePolicyCannotFallBackForClassifiedRoutes(t *testing.T) {
	routes := NewNotebookRoutes()
	routes.Register("GET /api/status", "read", func(http.ResponseWriter, *http.Request) {})
	// Preserve the corrupt-metadata checks previously performed inside web.
	// The concrete owner now makes its internal table inaccessible to handlers.
	for _, role := range []string{"", "administrator"} {
		routes.roles["GET /api/status"] = role
		if _, _, err := routes.Match(httptest.NewRequest("GET", "/api/status", nil)); !errors.Is(err, workspace.ErrForbidden) {
			t.Fatal("invalid authority admitted", role, err)
		}
	}
}
func TestNotebookRejectedRoutesKeepHistoricalPermissionChecks(t *testing.T) {
	routes := NewNotebookRoutes()
	routes.Register("POST /api/move", "owner", func(http.ResponseWriter, *http.Request) {})
	routes.RejectPath("/api/move", MethodRoles{Read: "owner", Write: "owner"})
	routes.RejectPrefix("/api/chat/", MethodRoles{Write: "read"})
	for _, test := range []struct{ method, path, role string }{
		{"GET", "/api/move", "owner"}, {"POST", "/api/chat/unknown", "read"}, {"DELETE", "/api/unknown", "edit"}, {"GET", "/api/unknown", "read"},
	} {
		pattern, role, err := routes.Match(httptest.NewRequest(test.method, test.path, nil))
		if pattern != "" || role != test.role || err != nil {
			t.Fatal("historical rejection authority changed", pattern, role, err)
		}
	}
}
