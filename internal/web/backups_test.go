package web

import (
	"forgetmenote/internal/catalog"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestManagedBackupRoutesRequireOwnerAndWorkerCredentials(t *testing.T) {
	app, _ := fixture(t)
	w := call(app, "GET", "/api/managed-backup", "", testToken)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"configured":false`) {
		t.Fatalf("status: %d %s", w.Code, w.Body.String())
	}
	if err := app.catalog.CreateKind(catalog.Bootstrap, "code", "Code", "repository"); err != nil {
		t.Fatal(err)
	}
	w = call(app, "POST", "/p/code/api/managed-backup/control", `{"action":"now","generation":0}`, testToken)
	if w.Code != 403 {
		t.Fatalf("source backup control: %d %s", w.Code, w.Body.String())
	}
	for _, token := range []string{"", testToken} {
		r := httptest.NewRequest("GET", app.origin+"/internal/backups/v1/bindings", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		out := httptest.NewRecorder()
		app.ServeHTTP(out, r)
		if out.Code != 401 {
			t.Fatalf("browser/admin credential admitted as worker: %d", out.Code)
		}
	}
}
