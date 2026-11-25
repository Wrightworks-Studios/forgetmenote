package web

import (
	"net/http"
	"testing"
)

func TestRepositoryHasNoDocumentationAPI(t *testing.T) {
	app, store := fixture(t)
	before, err := store.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range []struct{ method, path string }{
		{"GET", "providers"}, {"POST", "policy"}, {"POST", "examine"},
		{"GET", "runs"}, {"POST", "undo"}, {"POST", "usage"},
	} {
		t.Run(route.path, func(t *testing.T) {
			response := call(app, route.method, "/api/repository/"+route.path, `{}`, testToken)
			if response.Code != http.StatusNotFound {
				t.Fatalf("removed endpoint returned %d: %s", response.Code, response.Body.String())
			}
		})
	}
	after, err := store.Status(t.Context())
	if err != nil || after.Revision != before.Revision {
		t.Fatal("unsupported endpoints changed notebook history", err)
	}
}
