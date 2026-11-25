package reposource

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
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

func TestSourceConnectionAuthorityRefreshRestartAndRevocation(t *testing.T) {
	upstreamDir, sha := upstream(t)
	var head atomic.Value
	head.Store(sha)
	var revoked atomic.Bool
	var holdRefresh atomic.Bool
	refreshStarted, releaseRefresh := make(chan struct{}, 1), make(chan struct{}, 1)
	repo := Repository{ID: 42, FullName: "example/source", DefaultBranch: "trunk", Private: true}
	var metadata atomic.Value
	metadata.Store(repo)
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if revoked.Load() {
			http.Error(w, "revoked", 403)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/user/installations":
			if r.Header.Get("Authorization") != "Bearer owner-grant" {
				http.Error(w, "denied", 403)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "installations": []map[string]int{{"id": 7}}})
		case "/user/installations/7/repositories":
			json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "repositories": []Repository{metadata.Load().(Repository)}})
		case "/app/installations/7/access_tokens":
			var in struct {
				IDs         []int64           `json:"repository_ids"`
				Permissions map[string]string `json:"permissions"`
			}
			if json.NewDecoder(r.Body).Decode(&in) != nil || len(in.IDs) != 1 || in.IDs[0] != 42 || in.Permissions["contents"] != "read" {
				t.Error("overbroad installation token request")
			}
			json.NewEncoder(w).Encode(map[string]any{"token": "secret-installation-token", "expires_at": time.Now().Add(time.Hour)})
		case "/repositories/42":
			if holdRefresh.Load() {
				refreshStarted <- struct{}{}
				select {
				case <-releaseRefresh:
				case <-r.Context().Done():
					return
				}
			}
			json.NewEncoder(w).Encode(metadata.Load())
		case "/repos/example/source/commits/trunk", "/repos/example/source/commits/stable":
			json.NewEncoder(w).Encode(map[string]any{"sha": head.Load()})
		default:
			http.NotFound(w, r)
		}
	}))
	defer gh.Close()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	g := testGitHub(t, key, gh, Config{AppID: "123", Limits: Limits{History: 2, CacheBytes: 8 << 20, EvidenceBytes: 1 << 20, FileBytes: 1 << 20, Files: 100, Seconds: 10}})
	root := t.TempDir()
	db := pgtest.Org(t)
	m, err := Open(db, root, nil, g, func(project, actor string) error {
		if project != "alpha" || actor != "owner" {
			return ErrDenied
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	m.cacheFor = func(dir string) *cache {
		return &cache{dir: dir, limits: g.Config.Limits, allowFile: true, remoteURL: func(Repository) string { return "file://" + upstreamDir }}
	}
	ctx := context.Background()
	if _, err = m.Connect(ctx, "alpha", "owner", "bad-grant", 7, 42); err == nil {
		t.Fatal("accepted unverified installation")
	}
	s, err := m.Connect(ctx, "alpha", "owner", "owner-grant", 7, 42)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.Tree(ctx, "alpha", s.ID, ""); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("initial download should be unavailable, not an authorization failure: %v", err)
	}
	s, err = m.Refresh(ctx, "alpha")
	if err != nil || s.Downloaded != sha {
		t.Fatalf("refresh: %+v %v", s, err)
	}
	t.Run("concurrent refresh is explained and invalid controls leave work running", func(t *testing.T) {
		holdRefresh.Store(true)
		finished := make(chan error, 1)
		go func() { _, refreshErr := m.Refresh(ctx, "alpha"); finished <- refreshErr }()
		select {
		case <-refreshStarted:
		case <-time.After(5 * time.Second):
			t.Fatal("refresh did not reach GitHub")
		}
		readDone := make(chan error, 1)
		go func(id string) {
			file, readErr := m.File(ctx, "alpha", id, sha, "README.md")
			if readErr == nil && file.Revision != sha {
				readErr = errors.New("cached read changed its pinned revision")
			}
			readDone <- readErr
		}(s.ID)
		select {
		case readErr := <-readDone:
			if readErr != nil {
				t.Errorf("cached source read during refresh: %v", readErr)
			}
		case <-time.After(time.Second):
			t.Error("cached source reads block while refresh waits on GitHub metadata")
		}
		if _, busyErr := m.Refresh(ctx, "alpha"); busyErr == nil || !strings.Contains(busyErr.Error(), "already running") {
			t.Errorf("concurrent refresh should explain existing work: %v", busyErr)
		}
		// Rejected controls must not cancel another request's refresh.
		if _, controlErr := m.Control("alpha", "owner", "invalid"); !errors.Is(controlErr, ErrDenied) {
			t.Errorf("invalid control: %v", controlErr)
		}
		wakeDone := make(chan error, 1)
		go func() { wakeDone <- m.Wake(42, 7, "arrived-during-refresh") }()
		select {
		case wakeErr := <-wakeDone:
			if wakeErr != nil {
				t.Error(wakeErr)
			}
		case <-time.After(time.Second):
			t.Error("webhook blocked on metadata lookup")
		}
		releaseRefresh <- struct{}{}
		holdRefresh.Store(false)
		if refreshErr := <-finished; refreshErr != nil {
			t.Errorf("invalid control interrupted refresh: %v", refreshErr)
		}
		if state, err := m.Status("alpha"); err != nil || !state.Pending {
			t.Errorf("refresh lost a concurrent webhook: %+v %v", state, err)
		}
	})
	t.Run("index settings changed during lookup fence the old refresh", func(t *testing.T) {
		holdRefresh.Store(true)
		finished := make(chan error, 1)
		go func() { _, refreshErr := m.Refresh(ctx, "alpha"); finished <- refreshErr }()
		select {
		case <-refreshStarted:
		case <-time.After(5 * time.Second):
			t.Fatal("refresh did not reach GitHub")
		}
		configured := make(chan error, 1)
		go func() {
			_, configErr := m.ConfigureIndex("alpha", "owner", s.Generation, IndexConfig{Exclude: []string{"ignored/"}})
			configured <- configErr
		}()
		select {
		case configErr := <-configured:
			if configErr != nil {
				t.Error(configErr)
			}
		case <-time.After(time.Second):
			t.Error("settings change blocked on metadata lookup")
		}
		releaseRefresh <- struct{}{}
		holdRefresh.Store(false)
		if refreshErr := <-finished; !errors.Is(refreshErr, ErrConflict) {
			t.Errorf("old refresh was not fenced: %v", refreshErr)
		}
		state, err := m.Status("alpha")
		if err != nil || state.Generation != s.Generation+1 || !state.Pending || len(state.IndexConfig.Exclude) != 1 || state.IndexConfig.Exclude[0] != "ignored/" {
			t.Errorf("refresh overwrote current settings: %+v %v", state, err)
		}
	})
	if _, err = m.Capture(ctx, "alpha", s.ID, sha, []string{"README.md"}); err != nil {
		t.Fatal(err)
	}
	if err = os.RemoveAll(filepath.Join(root, "alpha", s.ID, "source.git", "objects")); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Refresh(ctx, "alpha"); err != nil {
		t.Fatal("disposable cache was not reconstructed", err)
	}
	if err = os.WriteFile(filepath.Join(upstreamDir, "new.go"), []byte("package sample\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, upstreamDir, "add", ".")
	gitRun(t, upstreamDir, "commit", "-m", "Code")
	head.Store(gitRun(t, upstreamDir, "rev-parse", "HEAD"))
	s, err = m.Refresh(ctx, "alpha")
	if err != nil || s.Downloaded == sha {
		t.Fatalf("changed refresh: %v", err)
	}
	changes, err := m.Changes(ctx, "alpha", s.ID, sha, s.Downloaded)
	if err != nil || len(changes.Paths) != 1 || changes.Paths[0] != "new.go" {
		t.Fatalf("changes: %+v %v", changes, err)
	}
	t.Run("processing changes preserve removals and exclusions", func(t *testing.T) {
		before := s.Downloaded
		for _, name := range []string{"visible.go", "hidden.go", ".env"} {
			if err := os.WriteFile(filepath.Join(upstreamDir, name), []byte("synthetic fixture\n"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Remove(filepath.Join(upstreamDir, "new.go")); err != nil {
			t.Fatal(err)
		}
		gitRun(t, upstreamDir, "add", ".")
		gitRun(t, upstreamDir, "commit", "-m", "Synthetic changes")
		head.Store(gitRun(t, upstreamDir, "rev-parse", "HEAD"))
		var err error
		s, err = m.Refresh(ctx, "alpha")
		if err != nil {
			t.Fatal(err)
		}
		cfg := s.IndexConfig
		cfg.Exclude = []string{"hidden.go"}
		s, err = m.ConfigureIndex("alpha", "owner", s.Generation, cfg)
		if err != nil {
			t.Fatal(err)
		}
		visible, err := m.IndexedChanges(ctx, "alpha", s.ID, before, s.Downloaded)
		if err != nil || len(visible.Paths) != 2 || len(visible.Removed) != 1 || visible.Removed[0] != "new.go" {
			t.Fatalf("filtered comparison: %+v %v", visible, err)
		}
		for _, path := range visible.Paths {
			if path != "visible.go" && path != "new.go" {
				t.Fatal("excluded metadata exposed", path)
			}
		}
	})

	if err = m.Wake(42, 7, "delivery-one"); err != nil {
		t.Fatal(err)
	}
	t.Run("default branch rename and rewritten history", func(t *testing.T) {
		replacement := repo
		replacement.DefaultBranch = "stable"
		metadata.Store(replacement)
		s, err = m.Refresh(ctx, "alpha")
		if err != nil || s.Repository.DefaultBranch != "stable" || !s.BaselineRequired {
			t.Fatalf("branch change: %+v %v", s, err)
		}
		gitRun(t, upstreamDir, "checkout", "--orphan", "rewritten")
		gitRun(t, upstreamDir, "rm", "-rf", ".")
		if err := os.WriteFile(filepath.Join(upstreamDir, "README.md"), []byte("# Rewritten system\n"), 0600); err != nil {
			t.Fatal(err)
		}
		gitRun(t, upstreamDir, "add", "README.md")
		gitRun(t, upstreamDir, "commit", "-m", "New baseline")
		previous := s.Downloaded
		head.Store(gitRun(t, upstreamDir, "rev-parse", "HEAD"))
		s, err = m.Refresh(ctx, "alpha")
		if err != nil || !s.BaselineRequired {
			t.Fatalf("rewrite lost baseline requirement: %+v %v", s, err)
		}
		changes, err := m.Changes(ctx, "alpha", s.ID, previous, s.Downloaded)
		if err != nil || !changes.BaselineRequired {
			t.Fatalf("invented a complete diff across rewritten history: %+v %v", changes, err)
		}
		if original, err := m.File(ctx, "alpha", s.ID, sha, "README.md"); err != nil || original.Content != "# Sample\nA documented system.\n" || !original.Retained {
			t.Fatalf("rewritten history lost evidence: %+v %v", original, err)
		}
	})
	if err = m.Wake(0, 7, "installation-change"); err != nil {
		t.Fatal(err)
	}
	if err = m.Wake(42, 7, "delivery-one"); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(db, root, nil, g, m.check)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	status, err := reopened.Status("alpha")
	if err != nil || !status.Pending || status.Downloaded != s.Downloaded {
		t.Fatalf("restart: %+v %v", status, err)
	}
	_, savedState, err := db.ReadDocument(ctx, db.Pool, stateKind, "alpha", -1)
	var persisted diskState
	if err != nil || json.Unmarshal(savedState, &persisted) != nil || len(persisted.Sources[status.ID].Deliveries) != 3 {
		t.Fatalf("delivery deduplication was not retained across restart: %v", err)
	}
	revoked.Store(true)
	if _, err = m.File(ctx, "alpha", s.ID, sha, "README.md"); err == nil {
		t.Fatal("revoked installation read retained source")
	}
	if _, err = m.Refresh(ctx, "alpha"); err == nil {
		t.Fatal("revoked installation refreshed")
	}
	status, _ = m.Status("alpha")
	if !status.Paused {
		t.Fatal("revoked source was not paused")
	}
	filepath.WalkDir(root, func(p string, d os.DirEntry, e error) error {
		if e == nil && !d.IsDir() {
			b, _ := os.ReadFile(p)
			if strings.Contains(string(b), "secret-installation-token") || strings.Contains(string(b), "owner-grant") {
				t.Errorf("credential persisted in %s", p)
			}
		}
		return e
	})
	if _, saved, err := db.ReadDocument(ctx, db.Pool, stateKind, "alpha", -1); err != nil || strings.Contains(string(saved), "secret-installation-token") || strings.Contains(string(saved), "owner-grant") {
		t.Errorf("credential persisted in the source state: %v", err)
	}
}
