package backups

import (
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"forgetmenote/internal/githubapp"
	"forgetmenote/internal/outgoing"
)

// A copy restored beside the live instance holds its bindings and in-flight
// leases, whose tokens are the live instance's. A paused manager mints no
// token, creates or checks no repository, and never validates or revokes a
// copied token; a handoff there still fences its own leases.
func TestPausedManagerLeavesGitHubAlone(t *testing.T) {
	m, _, head := fixture(t)
	ctx := t.Context()
	setup := Setup{Binding: Binding{Project: "notes", Actor: "alice", Owner: "local", Installation: 7, Repository: githubapp.Repository{ID: 42}, Branch: "main"}, UserToken: "user-grant"}
	b, err := m.Configure(ctx, setup)
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("a", 40)
	*head = sha
	copied, err := m.Lease(ctx, "local", "notes", b.Generation, sha)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	m.app.HTTP.Transport = backupRoundTrip(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		t.Errorf("paused manager called GitHub: %s %s", r.Method, r.URL.Path)
		return nil, errors.New("outgoing actions are paused")
	})
	m.Outgoing = &outgoing.Mode{}
	m.Outgoing.Pause()
	if _, err = m.Lease(ctx, "local", "notes", b.Generation, sha); !errors.Is(err, outgoing.ErrPaused) {
		t.Errorf("lease: %v", err)
	}
	if err = m.CheckLease(ctx, "local", copied.ID); !errors.Is(err, outgoing.ErrPaused) {
		t.Errorf("check: %v", err)
	}
	if _, err = m.Complete(ctx, "local", Receipt{Lease: copied.ID, Project: "notes", Generation: b.Generation, Revision: sha}); !errors.Is(err, outgoing.ErrPaused) {
		t.Errorf("receipt: %v", err)
	}
	other := setup
	other.Project, other.Repository = "other", githubapp.Repository{}
	if _, err = m.Create(ctx, other, "example", "copy"); !errors.Is(err, outgoing.ErrPaused) {
		t.Errorf("create: %v", err)
	}
	if _, err = m.Configure(ctx, other); !errors.Is(err, outgoing.ErrPaused) {
		t.Errorf("configure: %v", err)
	}
	if _, err = m.Control(ctx, "notes", b.Generation, "now"); !errors.Is(err, outgoing.ErrPaused) {
		t.Errorf("control: %v", err)
	}
	if _, ok := m.leases[copied.ID]; !ok {
		t.Error("paused manager dropped the copied lease")
	}
	b, err = m.BeginAuthority(ctx, "notes", "0123456789abcdef0123456789abcdef", b.Generation, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.leases[copied.ID]; ok || b.Enabled {
		t.Error("handoff on a paused copy kept its lease")
	}
	if calls.Load() != 0 {
		t.Fatalf("GitHub called %d times", calls.Load())
	}
}
