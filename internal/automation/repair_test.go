package automation

import (
	"forgetmenote/internal/pg/pgtest"
	"testing"
	"time"
)

func TestRepairDefinitionProtectsCheckOwnership(t *testing.T) {
	base := Spec{ID: "repair", Kind: "checks", Actor: "robot", Project: "notes", Recipe: "recipe.md", ExternalProcessing: true, Repair: &RepairPolicy{CooldownSeconds: 60}, Checks: []Check{{Name: "count", Block: "count", Call: Call{Operation: "query"}, Reference: &Call{Operation: "query"}}}}
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*Spec){
		func(s *Spec) { s.ExternalProcessing = false },
		func(s *Spec) { s.Repair.CooldownSeconds = 0 },
		func(s *Spec) { s.Checks[0].DependsOn = []string{"unknown"} },
		func(s *Spec) { s.Checks[0].DependsOn = []string{"count"} },
		func(s *Spec) { s.Checks[0].SQLTemplate = "SELECT true" },
		func(s *Spec) { s.Checks[0].SQLTemplate = "{{sql}} {{sql}}" },
		func(s *Spec) { s.Output = "report.md" },
		func(s *Spec) { s.Checks = append(s.Checks, s.Checks[0]) },
		func(s *Spec) { s.Kind = "task" },
	} {
		s := cloneSpec(base)
		change(&s)
		if s.Validate() == nil {
			t.Fatal("invalid repair accepted", s)
		}
	}
}

func TestRepairMemoSurvivesRetentionAndRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	q, err := Open(pgtest.Dir(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	spec := Spec{ID: "repair"}
	memo := RepairMemo{AttemptFingerprint: "unchanged", AttemptedAt: time.Now()}
	if err = q.SaveRepairMemo(spec, memo); err != nil {
		t.Fatal(err)
	}
	q, err = Open(pgtest.Dir(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	got, err := q.RepairMemo(spec)
	if err != nil || got.AttemptFingerprint != memo.AttemptFingerprint {
		t.Fatal(got, err)
	}
	changed := spec
	changed.Generation = 2
	if got, err = q.RepairMemo(changed); err != nil || got.AttemptFingerprint != "" {
		t.Fatal("definition change retained prior suppression", got, err)
	}
	if err = q.records.replace(Hash([]string{spec.ID, "repair"})+".state", make([]byte, 1<<20+1)); err != nil {
		t.Fatal(err)
	}
	if _, err = q.RepairMemo(spec); err == nil {
		t.Fatal("oversized checkpoint accepted")
	}
}
