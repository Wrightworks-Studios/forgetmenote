package catalog

import (
	"context"
	"errors"
	"forgetmenote/internal/pg/pgtest"
	"testing"

	"forgetmenote/internal/workspace"
)

func TestAuthorityPreviewBindsPermissionsAndPreventsConcurrentChanges(t *testing.T) {
	c, err := Open(t.TempDir(), nil, pgtest.Org(t))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	st, err := c.Access(Bootstrap, "default", "edit")
	if err != nil {
		t.Fatal(err)
	}
	head, err := st.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	in := AuthorityTransfer{To: "github", Revision: head.Revision, Source: "source", SourceRevision: head.Revision}
	p, err := c.PreviewAuthority(Bootstrap, "default", in)
	if err != nil {
		t.Fatal(err)
	}
	if p.ID == "" || p.From != "forgetmenote" || p.Generation != 1 {
		t.Fatalf("preview %+v", p)
	}
	if _, _, err = c.BeginAuthority(Bootstrap, "default", "stale"); !errors.Is(err, workspace.ErrConflict) {
		t.Fatal("stale preview", err)
	}
	if _, _, err = c.BeginAuthority(Bootstrap, "default", p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Access(Bootstrap, "default", "edit"); !errors.Is(err, workspace.ErrForbidden) {
		t.Fatal("write during handoff", err)
	}
	if _, err = c.PreviewAuthority(Bootstrap, "default", in); !errors.Is(err, workspace.ErrConflict) {
		t.Fatal("replaced active handoff", err)
	}
}
