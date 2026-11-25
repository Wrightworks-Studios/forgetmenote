package automation

import (
	"bytes"
	"encoding/json"
	"forgetmenote/internal/pg/pgtest"
	"testing"
)

func TestUnsupportedAutomationDefinitionsAreRejected(t *testing.T) {
	for _, kind := range []string{"shadow-review", "unknown-task"} {
		t.Run(kind, func(t *testing.T) {
			raw, err := json.Marshal(map[string]any{"id": "unsupported", "kind": kind, "actor": "worker", "project": "notes", "generation": 1, "externalProcessing": true, "shadow": map[string]any{"connection": "api"}})
			if err != nil {
				t.Fatal(err)
			}
			var spec Spec
			if err := json.Unmarshal(raw, &spec); err != nil {
				t.Fatal(err)
			}
			if err := spec.ValidateConfigured(); err == nil {
				t.Error("unsupported definition accepted")
			}
			d, err := OpenDefinitions(pgtest.Org(t))
			if err != nil {
				t.Fatal(err)
			}
			if err := d.SetFiles([]Spec{spec}); err == nil {
				t.Error("unsupported file definition accepted")
			}
			if _, err := d.Save(spec, ""); err == nil {
				t.Error("unsupported administrator definition accepted")
			}
			db := pgtest.Org(t)
			before := []byte(`{"version":1,"generation":1,"jobs":{"unsupported":` + string(raw) + `}}`)
			if _, err := db.WriteDocument(t.Context(), db.Pool, "automation-definitions", "jobs", 0, before); err != nil {
				t.Fatal(err)
			}
			if _, err := OpenDefinitions(db); err == nil {
				t.Error("unsupported persisted definition accepted")
			}
			if version, _, err := db.ReadDocument(t.Context(), db.Pool, "automation-definitions", "jobs", -1); err != nil || version != 1 {
				t.Fatal("rejecting an unsupported definition rewrote it", version, err)
			}
			id := Hash(kind)
			run := []byte(`{"version":5,"id":"` + id + `","spec":` + string(raw) + `,"state":"running"}`)
			if _, err := (records{db: db}).write(id+".json", run, 0); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(db); err == nil {
				t.Error("unsupported persisted run accepted")
			}
			after, _, err := (records{db: db}).read(id+".json", 1<<20)
			if err != nil || !bytes.Equal(run, after) {
				t.Fatal("rejecting an unsupported run changed its saved bytes", err)
			}
		})
	}
}
