package automation

import (
	"errors"
	"fmt"
	"forgetmenote/internal/pg/pgtest"
	"forgetmenote/internal/workspace"
	"testing"
)

func TestDefinitionsBeyondLegacyJobCount(t *testing.T) {
	dir := t.TempDir()
	d, err := OpenDefinitions(pgtest.Dir(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 129; i++ {
		spec := Spec{ID: fmt.Sprintf("job-%d", i), Kind: "digest", Actor: "robot", Project: "notes", Output: "report.md", Calls: []Call{{Connection: "metrics", Operation: "read"}}}
		if _, err := d.Save(spec, ""); err != nil {
			t.Fatalf("job %d: %v", i, err)
		}
	}
	d, err = OpenDefinitions(pgtest.Dir(t, dir))
	if err != nil || len(d.List()) != 129 {
		t.Fatalf("definitions did not survive reopen: %v", err)
	}
}

func TestDefinitionPersistenceOwnershipAndIsolation(t *testing.T) {
	dir := t.TempDir()
	d, err := OpenDefinitions(pgtest.Dir(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	spec := Spec{ID: "daily", Kind: "digest", Actor: "robot", Project: "notes", Output: "report.md", Calls: []Call{{Connection: "metrics", Operation: "read", Arguments: map[string]any{"days": 1}}}}
	saved, err := d.Save(spec, "")
	if err != nil {
		t.Fatal(err)
	}
	spec.Calls[0].Arguments["days"] = 99
	copy, _ := d.Get("daily")
	if copy.Spec.Calls[0].Arguments["days"] != float64(1) {
		t.Fatal("mutated stored definition")
	}
	copy.Spec.Calls[0].Arguments["days"] = 98
	d, err = OpenDefinitions(pgtest.Dir(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	copy, _ = d.Get("daily")
	if copy.Revision != saved.Revision || copy.ManagedBy != "instance" || copy.Spec.Calls[0].Arguments["days"] != float64(1) {
		t.Fatal("lost persisted definition", copy)
	}
	if _, err = d.Save(spec, ""); !errors.Is(err, workspace.ErrConflict) {
		t.Fatal("stale update accepted", err)
	}
	if err = d.SetFiles([]Spec{spec}); err == nil {
		t.Fatal("duplicate ownership accepted")
	}
	saved.Spec.Disabled = true
	paused, err := d.Save(saved.Spec, saved.Revision)
	if err != nil || paused.Revision == saved.Revision {
		t.Fatal("pause did not advance revision", err)
	}
	if err = d.Delete(spec.ID, saved.Revision); !errors.Is(err, workspace.ErrConflict) {
		t.Fatal("stale delete accepted", err)
	}
	if err = d.Delete(spec.ID, paused.Revision); err != nil {
		t.Fatal(err)
	}
	if err = d.SetFiles([]Spec{spec}); err != nil {
		t.Fatal(err)
	}
	copy, _ = d.Get(spec.ID)
	if _, err = d.Save(spec, copy.Revision); err == nil {
		t.Fatal("file definition was overwritten")
	}
}

func TestDefinitionsRefuseDamagedState(t *testing.T) {
	db := pgtest.Org(t)
	if _, err := db.WriteDocument(t.Context(), db.Pool, "automation-definitions", "jobs", 0, []byte(`{"version":1}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenDefinitions(db); err == nil {
		t.Fatal("opened definitions without jobs")
	}
}
