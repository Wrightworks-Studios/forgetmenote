package backups

import (
	"errors"
	"forgetmenote/internal/githubapp"
	"strings"
	"testing"
)

func TestHandoffStopsOutgoingLeasesBeforeWorkerAcknowledgement(t *testing.T) {
	m, _, head := fixture(t)
	ctx := t.Context()
	b, err := m.Configure(ctx, Setup{Binding: Binding{Project: "notes", Actor: "alice", Owner: "local", Installation: 7, Repository: githubapp.Repository{ID: 42}, Branch: "main"}, UserToken: "user-grant"})
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("a", 40)
	*head = sha
	lease, err := m.Lease(ctx, "local", "notes", b.Generation, sha)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.Complete(ctx, "local", Receipt{Lease: lease.ID, Project: "notes", Generation: b.Generation, Revision: sha}); err != nil {
		t.Fatal(err)
	}
	b, err = m.BeginAuthority(ctx, "notes", "0123456789abcdef0123456789abcdef", b.Generation, sha)
	if err != nil {
		t.Fatal(err)
	}
	if b.Enabled {
		t.Fatal("outgoing worker still enabled")
	}
	if _, err = m.Lease(ctx, "local", "notes", b.Generation, sha); !errors.Is(err, ErrDenied) {
		t.Fatal("handoff allowed new push", err)
	}
}
