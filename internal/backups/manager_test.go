package backups

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"forgetmenote/internal/githubapp"
	"forgetmenote/internal/pg"
	"forgetmenote/internal/pg/pgtest"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

type backupRoundTrip func(*http.Request) (*http.Response, error)

func (f backupRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestBackupLeaseRejectsLostInstallationAccess(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"suspended", 200, `{"id":7,"suspended_at":"2026-09-23T14:00:00Z","permissions":{"contents":"write"}}`},
		{"read only", 200, `{"id":7,"permissions":{"contents":"read"}}`},
		{"replaced", 200, `{"id":8,"permissions":{"contents":"write"}}`},
		{"removed", 404, `{"message":"Not Found"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _, head := fixture(t)
			b, err := m.Configure(t.Context(), Setup{Binding: Binding{Project: "notes", Actor: "alice", Owner: "local", Installation: 7, Repository: githubapp.Repository{ID: 42}, Branch: "main"}, UserToken: "user-grant"})
			if err != nil {
				t.Fatal(err)
			}
			sha := strings.Repeat("a", 40)
			*head = sha
			l, err := m.Lease(t.Context(), "local", "notes", b.Generation, sha)
			if err != nil {
				t.Fatal(err)
			}
			if err := m.CheckLease(t.Context(), "local", l.ID); err != nil {
				t.Fatal("active installation rejected", err)
			}
			transport := m.app.HTTP.Transport
			var revoked bool
			m.app.HTTP.Transport = backupRoundTrip(func(r *http.Request) (*http.Response, error) {
				if r.Method == "GET" && r.URL.Path == "/repos/example/backup/installation" {
					w := httptest.NewRecorder()
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(tc.status)
					w.WriteString(tc.body)
					return w.Result(), nil
				}
				if r.Method == "DELETE" && r.URL.Path == "/installation/token" {
					revoked = true
				}
				return transport.RoundTrip(r)
			})
			// GitHub can still serve repository metadata after suspending the App.
			if _, err := m.app.Repository(t.Context(), l.Token, b.Repository.ID); err != nil {
				t.Fatal("fixture did not preserve readable metadata", err)
			}
			if err := m.CheckLease(t.Context(), "local", l.ID); err == nil {
				t.Fatal("lease remained valid after installation access was lost")
			}
			if _, ok := m.leases[l.ID]; ok || !revoked {
				t.Fatal("rejected lease or GitHub token was retained")
			}
			if _, err := m.Complete(t.Context(), "local", Receipt{Lease: l.ID, Project: "notes", Generation: b.Generation, Revision: sha}); err == nil {
				t.Fatal("receipt accepted after installation access was lost")
			}
			if _, err := m.Lease(t.Context(), "local", "notes", b.Generation, sha); err == nil {
				t.Fatal("new lease issued after installation access was lost")
			}
			if status, err := m.Status("notes"); err != nil || status.LastRevision != "" || status.Error == "" {
				t.Fatal("access loss was not reflected in backup status", err)
			}
		})
	}
}

func TestBackupLeaseRechecksAuthorityAfterGitHubResponse(t *testing.T) {
	m, _, _ := fixture(t)
	var revoked atomic.Bool
	m.check = func(Binding) error {
		if revoked.Load() {
			return ErrDenied
		}
		return nil
	}
	b, err := m.Configure(t.Context(), Setup{Binding: Binding{Project: "notes", Actor: "alice", Owner: "local", Installation: 7, Repository: githubapp.Repository{ID: 42}, Branch: "main"}, UserToken: "user-grant"})
	if err != nil {
		t.Fatal(err)
	}
	l, err := m.Lease(t.Context(), "local", "notes", b.Generation, strings.Repeat("a", 40))
	if err != nil {
		t.Fatal(err)
	}
	transport := m.app.HTTP.Transport
	m.app.HTTP.Transport = backupRoundTrip(func(r *http.Request) (*http.Response, error) {
		response, err := transport.RoundTrip(r)
		if r.Method == "GET" && r.URL.Path == "/repositories/42" {
			revoked.Store(true)
		}
		return response, err
	})
	if err := m.CheckLease(t.Context(), "local", l.ID); err == nil {
		t.Fatal("lease remained valid after notebook authority was revoked during GitHub validation")
	}
	if _, ok := m.leases[l.ID]; ok {
		t.Fatal("revoked lease was retained")
	}
}

func TestBackupStateReopensAfterNotebookReplacement(t *testing.T) {
	db := pgtest.Org(t)
	m, _, _ := fixtureOn(t, db)
	// Deleted/disconnected notebooks retain their receipts. The catalog limit
	// counts current notebooks, not every notebook an instance has ever held.
	state := sharedState{Version: 1, Bindings: map[string]Binding{}}
	for i := range 256 {
		project := fmt.Sprintf("previous-%d", i)
		state.Bindings[project] = Binding{Project: project, Actor: "alice", Owner: "local"}
	}
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.WriteDocument(t.Context(), db.Pool, "backups", "bindings", 0, raw); err != nil {
		t.Fatal(err)
	}
	m, err = Open(db, m.app, m.check)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.Configure(t.Context(), Setup{Binding: Binding{Project: "replacement", Actor: "alice", Owner: "local", Installation: 7, Repository: githubapp.Repository{ID: 42}, Branch: "main"}, UserToken: "user-grant"}); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(db, m.app, m.check)
	if err != nil {
		t.Fatal("successful configuration could not be reopened", err)
	}
	if b, err := reopened.Status("replacement"); err != nil || !b.Connected {
		t.Fatal("latest backup configuration was lost", err)
	}
}

func TestBackupConfigurationRefusesStateItCannotReopen(t *testing.T) {
	db := pgtest.Org(t)
	m, _, _ := fixtureOn(t, db)
	_, err := m.Configure(t.Context(), Setup{Binding: Binding{Project: "notes", Actor: "alice", Owner: "local", Installation: 7, Repository: githubapp.Repository{ID: 42}, Branch: "main", Error: strings.Repeat("x", 4<<20)}, UserToken: "user-grant"})
	if err == nil {
		t.Fatal("configuration persisted more state than it can read on restart")
	}
	reopened, err := Open(db, m.app, m.check)
	if err != nil {
		t.Fatal(err)
	}
	if b, err := reopened.Status("notes"); err != nil || b.Connected {
		t.Fatal("rejected configuration did not preserve the prior state", err)
	}
}

// A worker reaches whichever gateway serves its request, so a lease granted by
// one gateway is checked, completed and released by another, and no gateway
// grants a second lease for the same notebook while it is active.
func TestGatewaysShareBackupLeasesAndReceipts(t *testing.T) {
	db := pgtest.Org(t)
	a, _, head := fixtureOn(t, db)
	b, err := Open(db, a.app, a.check)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := a.Configure(t.Context(), Setup{Binding: Binding{Project: "notes", Actor: "alice", Owner: "local", Installation: 7, Repository: githubapp.Repository{ID: 42}, Branch: "main"}, UserToken: "user-grant"})
	if err != nil {
		t.Fatal(err)
	}
	if listed, err := b.List("local"); err != nil || len(listed) != 1 {
		t.Fatal("another gateway does not list the new binding", listed, err)
	}
	sha := strings.Repeat("a", 40)
	*head = sha
	l, err := a.Lease(t.Context(), "local", "notes", binding.Generation, sha)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = b.Lease(t.Context(), "local", "notes", binding.Generation, sha); !errors.Is(err, ErrConflict) {
		t.Fatal("another gateway granted an overlapping lease:", err)
	}
	if err = b.CheckLease(t.Context(), "local", l.ID); err != nil {
		t.Fatal("another gateway refused an active lease:", err)
	}
	if _, err = b.Complete(t.Context(), "local", Receipt{Lease: l.ID, Project: "notes", Generation: binding.Generation, Revision: sha}); err != nil {
		t.Fatal(err)
	}
	if status, err := a.Status("notes"); err != nil || status.LastRevision != sha {
		t.Fatal("the granting gateway does not see the receipt", status.LastRevision, err)
	}
	if err = a.CheckLease(t.Context(), "local", l.ID); !errors.Is(err, ErrDenied) {
		t.Fatal("a completed lease stayed valid on the granting gateway:", err)
	}
	if _, err = a.Lease(t.Context(), "local", "notes", binding.Generation, sha); err != nil {
		t.Fatal("a completed lease still blocks the notebook:", err)
	}
}

func TestNoBackupLeaseWhileTheDatabaseIsUnreachable(t *testing.T) {
	db := pgtest.Database(t)
	m, _, _ := fixtureOn(t, db)
	b, err := m.Configure(t.Context(), Setup{Binding: Binding{Project: "notes", Actor: "alice", Owner: "local", Installation: 7, Repository: githubapp.Repository{ID: 42}, Branch: "main"}, UserToken: "user-grant"})
	if err != nil {
		t.Fatal(err)
	}
	var minted atomic.Int32
	transport := m.app.HTTP.Transport
	m.app.HTTP.Transport = backupRoundTrip(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/access_tokens") {
			minted.Add(1)
		}
		return transport.RoundTrip(r)
	})
	db.Close()
	if _, err = m.Lease(t.Context(), "local", "notes", b.Generation, strings.Repeat("a", 40)); !errors.Is(err, ErrUnavailable) {
		t.Fatal("lease without shared state:", err)
	}
	if listed, err := m.List("local"); !errors.Is(err, ErrUnavailable) || len(listed) != 0 {
		t.Fatal("bindings listed without shared state:", listed, err)
	}
	if minted.Load() != 0 {
		t.Fatal("a GitHub token was minted without shared state")
	}
}

func fixture(t *testing.T) (*Manager, *bool, *string) {
	t.Helper()
	return fixtureOn(t, pgtest.Org(t))
}

func fixtureOn(t *testing.T, db *pg.DB) (*Manager, *bool, *string) {
	t.Helper()
	private := true
	head := ""
	repo := func() githubapp.Repository {
		r := githubapp.Repository{ID: 42, FullName: "example/backup", DefaultBranch: "main", Private: private}
		r.Permissions.Push = true
		return r
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/user/installations":
			json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "installations": []map[string]int{{"id": 7}}})
		case "/user/installations/7/repositories":
			json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "repositories": []githubapp.Repository{repo()}})
		case "/repositories/42":
			json.NewEncoder(w).Encode(repo())
		case "/repos/example/backup/installation":
			json.NewEncoder(w).Encode(map[string]any{"id": 7, "permissions": map[string]string{"contents": "write"}})
		case "/app/installations/7/access_tokens":
			var in struct {
				IDs         []int64           `json:"repository_ids"`
				Permissions map[string]string `json:"permissions"`
			}
			json.NewDecoder(r.Body).Decode(&in)
			if len(in.IDs) != 1 || in.IDs[0] != 42 || in.Permissions["contents"] != "write" {
				t.Error("overbroad backup token")
			}
			json.NewEncoder(w).Encode(map[string]any{"token": "synthetic-secret", "expires_at": time.Now().Add(time.Hour)})
		case "/repos/example/backup/branches":
			json.NewEncoder(w).Encode([]any{})
		case "/repos/example/backup/git/ref/heads/main":
			json.NewEncoder(w).Encode(map[string]any{"object": map[string]string{"sha": head}})
		case "/installation/token":
			w.WriteHeader(204)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	dir := t.TempDir()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"key": pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), "client": []byte("synthetic"), "hook": []byte(strings.Repeat("s", 32))} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	app, err := githubapp.New(githubapp.Config{AppID: "1", Slug: "test", ClientID: "test", PrivateKeyFile: filepath.Join(dir, "key"), ClientSecretFile: filepath.Join(dir, "client"), WebhookSecretFile: filepath.Join(dir, "hook")}, "write")
	if err != nil {
		t.Fatal(err)
	}
	app.API = server.URL
	app.HTTP = server.Client()
	m, err := Open(db, app, func(b Binding) error {
		if b.Actor != "alice" || b.Owner != "local" {
			return ErrDenied
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return m, &private, &head
}
func TestBackupBindingUsesPrivateDestinationAndPersists(t *testing.T) {
	db := pgtest.Org(t)
	m, private, _ := fixtureOn(t, db)
	in := Setup{Binding: Binding{Project: "notes", Actor: "alice", Owner: "local", Installation: 7, Repository: githubapp.Repository{ID: 42}, Branch: "main"}, UserToken: "user-grant"}
	b, err := m.Configure(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if !b.Enabled || b.Generation != 1 || b.Repository.FullName != "example/backup" {
		t.Fatal(b)
	}
	reopened, err := Open(db, m.app, m.check)
	if err != nil {
		t.Fatal(err)
	}
	if b, err := reopened.Status("notes"); err != nil || !b.Connected {
		t.Fatal("binding lost after restart", err)
	}
	*private = false
	in.Generation = b.Generation
	if _, err = m.Configure(t.Context(), in); err == nil {
		t.Fatal("public backup destination accepted")
	}
}

func TestBackupLeasePinsReceiptAndRevocation(t *testing.T) {
	db := pgtest.Org(t)
	m, private, head := fixtureOn(t, db)
	b, err := m.Configure(t.Context(), Setup{Binding: Binding{Project: "notes", Actor: "alice", Owner: "local", Installation: 7, Repository: githubapp.Repository{ID: 42}, Branch: "main"}, UserToken: "user-grant"})
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("a", 40)
	if _, err = m.Lease(t.Context(), "another-owner", "notes", b.Generation, sha); err == nil {
		t.Fatal("wrong storage owner received credentials")
	}
	l, err := m.Lease(t.Context(), "local", "notes", b.Generation, sha)
	if err != nil {
		t.Fatal(err)
	}
	if l.Revision != sha || l.Token == "" || l.Remote != "https://github.com/example/backup.git" {
		t.Fatal("invalid lease")
	}
	if _, err = m.Lease(t.Context(), "local", "notes", b.Generation, sha); err == nil {
		t.Fatal("concurrent lease accepted")
	}
	r := Receipt{Lease: l.ID, Project: "notes", Generation: b.Generation, Revision: sha}
	*head = strings.Repeat("b", 40)
	if _, err = m.Complete(t.Context(), "local", r); err == nil {
		t.Fatal("unverified revision accepted")
	}
	*head = sha
	if _, err = m.Complete(t.Context(), "local", r); err != nil {
		t.Fatal(err)
	}
	if status, err := m.Status("notes"); err != nil || status.LastRevision != sha {
		t.Fatal("receipt missing", err)
	}
	l, err = m.Lease(t.Context(), "local", "notes", b.Generation, sha)
	if err != nil {
		t.Fatal(err)
	}
	*private = false
	if err = m.CheckLease(t.Context(), "local", l.ID); err == nil {
		t.Fatal("public destination lease survived")
	}
	*private = true
	l, err = m.Lease(t.Context(), "local", "notes", b.Generation, sha)
	if err != nil {
		t.Fatal(err)
	}
	_, raw, err := db.ReadDocument(t.Context(), db.Pool, "backups", "bindings", -1)
	if err != nil || strings.Contains(string(raw), "synthetic-secret") || !strings.Contains(string(raw), l.ID) {
		t.Fatal("credential persisted", err)
	}
	if _, err = m.Control(t.Context(), "notes", b.Generation, "pause"); err != nil {
		t.Fatal(err)
	}
	if err = m.CheckLease(t.Context(), "local", l.ID); err == nil {
		t.Fatal("paused lease survived")
	}
	if _, err = m.Complete(t.Context(), "local", r); err == nil {
		t.Fatal("stale receipt accepted")
	}
}

func TestBackupDivergenceRequiresExplicitResume(t *testing.T) {
	m, _, _ := fixture(t)
	b, err := m.Configure(t.Context(), Setup{Binding: Binding{Project: "notes", Actor: "alice", Owner: "local", Installation: 7, Repository: githubapp.Repository{ID: 42}, Branch: "main"}, UserToken: "user-grant"})
	if err != nil {
		t.Fatal(err)
	}
	l, err := m.Lease(t.Context(), "local", "notes", b.Generation, strings.Repeat("a", 40))
	if err != nil {
		t.Fatal(err)
	}
	b, err = m.Complete(t.Context(), "local", Receipt{Lease: l.ID, Project: l.Project, Generation: l.Generation, Revision: l.Revision, Error: "secret stderr", Diverged: true})
	if listed, _ := m.List("local"); err != nil || !b.Blocked || len(listed) != 0 || strings.Contains(b.Error, "secret") {
		t.Fatal(b, err)
	}
	if _, err = m.Control(t.Context(), "notes", b.Generation, "resume"); err != nil {
		t.Fatal(err)
	}
	if listed, err := m.List("local"); err != nil || len(listed) != 1 {
		t.Fatal("resume did not permit retry", err)
	}
}
