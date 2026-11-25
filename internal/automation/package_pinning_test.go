package automation

import (
	"forgetmenote/internal/pg/pgtest"
	"strings"
	"testing"
)

// Definitions saved before the opt-in existed keep their revision and load
// pinned; an explicit opt-in survives a restart.
func TestPersistedScriptInstallationsDefaultToPinned(t *testing.T) {
	dir := t.TempDir()
	definitions, err := OpenDefinitions(pgtest.Dir(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := definitions.Save(scriptSpec(t), "")
	if err != nil {
		t.Fatal(err)
	}
	db := pgtest.Dir(t, dir)
	_, raw, err := db.ReadDocument(t.Context(), db.Pool, "automation-definitions", "jobs", -1)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "followPrimaryBranch") {
		t.Fatal("default installation changed the persisted definition shape", string(raw))
	}
	follow := scriptSpec(t)
	follow.ID = "following"
	follow.Script.FollowPrimaryBranch = true
	if _, err = definitions.Save(follow, ""); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenDefinitions(pgtest.Dir(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	loaded, ok := reopened.Get(pinned.Spec.ID)
	if !ok || loaded.Spec.Script.FollowPrimaryBranch || loaded.Revision != pinned.Revision {
		t.Fatal("existing installation did not load pinned with its revision", loaded)
	}
	loaded, ok = reopened.Get("following")
	if !ok || !loaded.Spec.Script.FollowPrimaryBranch {
		t.Fatal("explicit primary-branch opt-in was not persisted", loaded)
	}
}
