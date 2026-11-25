package catalog

import (
	"encoding/json"
	"errors"
	"forgetmenote/internal/pg/pgtest"
	"forgetmenote/internal/workspace"
	"testing"
)

func TestRepositoryKindSurvivesRestartReadOnly(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(dir, nil, testDB(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	if err = c.CreateKind(Bootstrap, "code", "Code", "repository"); err != nil {
		t.Fatal(err)
	}
	c.Close()
	reopened, err := Open(dir, nil, testDB(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.Kind("code") != "repository" || reopened.Kind("default") != "notes" {
		t.Fatal("notebook kinds lost")
	}
	if _, err := reopened.Access(Bootstrap, "code", "edit"); err == nil {
		t.Fatal("repository became editable")
	}
}

func TestNotebookListExposesAuthority(t *testing.T) {
	c, err := Open(t.TempDir(), nil, pgtest.Org(t))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err = c.CreateKind(Bootstrap, "source", "Source", "repository"); err != nil {
		t.Fatal(err)
	}
	for _, p := range c.List(Bootstrap) {
		raw, _ := json.Marshal(p)
		var out struct {
			Authority struct {
				Type       string `json:"type"`
				Generation uint64 `json:"generation"`
			} `json:"authority"`
		}
		json.Unmarshal(raw, &out)
		want := "forgetmenote"
		if p.ID == "source" {
			want = "github"
		}
		if out.Authority.Type != want || out.Authority.Generation == 0 {
			t.Fatalf("missing authority: %s", raw)
		}
	}
}

func TestOpenAsWikiPersistsForMembersAndOnlyOwnersChangeIt(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(dir, nil, testDB(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	for user, role := range map[string]string{"owner": "owner", "editor": "edit", "reader": "read"} {
		if err = c.Member(Bootstrap, "default", user, role); err != nil {
			t.Fatal(err)
		}
	}
	if err = c.SetOpenAsWiki("editor", "default", true); !errors.Is(err, workspace.ErrForbidden) {
		t.Fatalf("editor: %v", err)
	}
	if err = c.SetOpenAsWiki("owner", "default", true); err != nil {
		t.Fatal(err)
	}
	c.Close()
	c, err = Open(dir, nil, testDB(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	listed := func(user string) bool {
		for _, p := range c.List(user) {
			if p.ID == "default" {
				return p.OpenAsWiki
			}
		}
		t.Fatalf("%s cannot list the notebook", user)
		return false
	}
	if !listed("reader") || !listed("owner") {
		t.Fatal("reopened catalog lost the wiki setting")
	}
	if entry := c.activity[len(c.activity)-1]; entry.Action != "notebook.wiki" || entry.Actor != "owner" || entry.Details["openAsWiki"] != "true" {
		t.Fatalf("activity: %+v", entry)
	}
	if err = c.SetOpenAsWiki(Bootstrap, "default", false); err != nil || listed("reader") {
		t.Fatalf("admin could not turn the wiki off: %v", err)
	}
}
