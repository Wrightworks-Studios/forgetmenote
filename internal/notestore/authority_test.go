package notestore_test

import (
	"bytes"
	"errors"
	"io/fs"
	"testing"

	"forgetmenote/internal/notestore"
)

// A notebook's authority record is replaced only over the version read, and
// keeps its stage only until the next write; every gateway reads the same.
func TestAuthorityRecordIsFencedByVersion(t *testing.T) {
	e := newEnv(t, notestore.Options{})
	n := e.create("notes")
	if a, err := n.Authority(e.ctx); err != nil || a.Version != 0 || a.State != nil || a.Staged {
		t.Fatalf("a new notebook's record: %+v %v", a, err)
	}
	v, err := n.PutAuthority(e.ctx, notestore.AuthorityWrite{State: []byte(`{"phase":"prepared"}`)})
	if err != nil || v != 1 {
		t.Fatal(v, err)
	}
	if _, err = n.PutAuthority(e.ctx, notestore.AuthorityWrite{State: []byte(`{"phase":"other"}`)}); !errors.Is(err, notestore.ErrConflict) {
		t.Fatal("a second first record:", err)
	}
	if v, err = n.PutAuthority(e.ctx, notestore.AuthorityWrite{Version: 1, State: []byte(`{"phase":"staged"}`), Stage: []byte("snapshot")}); err != nil || v != 2 {
		t.Fatal(v, err)
	}
	other := e.open(e.db, notestore.Options{}).Notebook("notes")
	a, err := other.Authority(e.ctx)
	if err != nil || a.Version != 2 || string(a.State) != `{"phase": "staged"}` || !a.Staged {
		t.Fatalf("another gateway's read: %+v %s %v", a, a.State, err)
	}
	if stage, err := other.AuthorityStage(e.ctx, 2); err != nil || !bytes.Equal(stage, []byte("snapshot")) {
		t.Fatalf("stage: %q %v", stage, err)
	}
	if _, err = n.PutAuthority(e.ctx, notestore.AuthorityWrite{Version: 1, State: []byte(`{"phase":"stale"}`)}); !errors.Is(err, notestore.ErrConflict) {
		t.Fatal("a write over an older version:", err)
	}
	if v, err = n.PutAuthority(e.ctx, notestore.AuthorityWrite{Version: 2, State: []byte(`{"phase":"complete"}`)}); err != nil || v != 3 {
		t.Fatal(v, err)
	}
	if _, err = other.AuthorityStage(e.ctx, 2); !errors.Is(err, notestore.ErrConflict) {
		t.Fatal("a stage read past its record:", err)
	}
	if a, err = n.Authority(e.ctx); err != nil || a.Version != 3 || a.Staged {
		t.Fatalf("a write without a stage keeps none: %+v %v", a, err)
	}
	if stage, err := n.AuthorityStage(e.ctx, 3); err != nil || stage != nil {
		t.Fatalf("no stage: %q %v", stage, err)
	}
}

// A record written for a head is written only while the notebook is there.
func TestAuthorityRecordChecksTheHead(t *testing.T) {
	e := newEnv(t, notestore.Options{})
	n := e.create("notes")
	first, err := n.Head(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = n.PutAuthority(e.ctx, notestore.AuthorityWrite{State: []byte(`{}`), ExpectHead: &first.Commit}); err != nil {
		t.Fatal(err)
	}
	apply(t, n, notestore.ChangeSet{Files: []notestore.FileChange{write("a.md", "a")}})
	_, err = n.PutAuthority(e.ctx, notestore.AuthorityWrite{Version: 1, State: []byte(`{"stale":true}`), ExpectHead: &first.Commit})
	var conflict *notestore.ConflictError
	if !errors.As(err, &conflict) || conflict.Expected != first.Commit {
		t.Fatal("a record for a head the notebook left:", err)
	}
	if a, err := n.Authority(e.ctx); err != nil || a.Version != 1 {
		t.Fatalf("a refused write changed the record: %+v %v", a, err)
	}
}

// Deleting a notebook deletes its record; a notebook created again under its
// ID starts without one, and a missing notebook takes none.
func TestAuthorityRecordGoesWithItsNotebook(t *testing.T) {
	e := newEnv(t, notestore.Options{})
	n := e.create("notes")
	if _, err := n.PutAuthority(e.ctx, notestore.AuthorityWrite{State: []byte(`{}`), Stage: []byte("stage")}); err != nil {
		t.Fatal(err)
	}
	if err := e.store.Delete(e.ctx, "notes"); err != nil {
		t.Fatal(err)
	}
	if _, err := n.PutAuthority(e.ctx, notestore.AuthorityWrite{Version: 1, State: []byte(`{}`)}); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("a record for a deleted notebook:", err)
	}
	n = e.create("notes")
	if a, err := n.Authority(e.ctx); err != nil || a.Version != 0 {
		t.Fatalf("a new notebook inherited a record: %+v %v", a, err)
	}
}
