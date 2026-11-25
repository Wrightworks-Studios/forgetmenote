package web

import (
	"encoding/json"
	"fmt"
	"testing"
)

// The browser save shares MCP save_note's authority-generation fence: a save
// prepared under an earlier notebook authority must not commit after a handoff.
func TestBrowserSaveEnforcesAuthorityGeneration(t *testing.T) {
	app, store := fixture(t)
	current := app.catalog.Authority("default").Generation
	if current == 0 {
		t.Fatal("fixture notebook has no authority generation")
	}
	save := func(path string, generation uint64) (int, string) {
		w := call(app, "POST", "/api/note", fmt.Sprintf(`{"path":%q,"content":"# Fenced","revision":"","authorityGeneration":%d}`, path, generation), testToken)
		var out struct {
			Code string `json:"code"`
		}
		json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out.Code
	}
	if status, code := save("notes/stale.md", current+1); status != 409 || code != "conflict" {
		t.Fatalf("stale authority generation saved: %d %s", status, code)
	}
	if _, err := store.Read(t.Context(), "notes/stale.md", ""); err == nil {
		t.Fatal("fenced save committed the note")
	}
	if status, _ := save("notes/current.md", current); status != 200 {
		t.Fatalf("current authority generation refused: %d", status)
	}
	if status, _ := save("notes/unfenced.md", 0); status != 200 {
		t.Fatalf("omitted generation refused: %d", status)
	}
}
