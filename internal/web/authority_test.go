package web

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"forgetmenote/internal/backuphttp"
	"forgetmenote/internal/backups"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/githubapp"
	"forgetmenote/internal/notebooks"
	"forgetmenote/internal/pg/pgtest"
	"forgetmenote/internal/reposource"
	"forgetmenote/internal/workspace"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAuthorityActionsRequireVerifiedGitHubSource(t *testing.T) {
	app, _ := fixture(t)
	w := apiCall(app, "POST", "/api/v1/actions/preview_notebook_authority_change", `{"project":"default","to":"github","repository":"example/notebook","operatorWritersStopped":true}`, testToken)
	if w.Code != 503 {
		t.Fatalf("unconfigured handoff: %d %s", w.Code, w.Body.String())
	}
	list := apiCall(app, "POST", "/api/v1/actions/list_projects", `{}`, testToken)
	if !strings.Contains(list.Body.String(), `"type":"forgetmenote"`) {
		t.Fatal("preview changed authority")
	}
}

// Only GitHub's network boundary is simulated. The notebook, upstream tree,
// source index, catalog, storage transactions and worker protocol are real.
func authorityFixture(t *testing.T) (*Server, *workspace.Store, func(map[string][]byte)) {
	t.Helper()
	app, store := fixture(t)
	ctx := t.Context()
	for p, text := range map[string]string{"note.md": "# Central\noriginalterm", "draft.md": "---\nfmn-container: draft\n---\n# Draft\ndraftonlyterm", "archive.md": "---\nfmn-container: archive\n---\n# Archive\narchiveonlyterm", "removed.md": "# Removed\ntrashonlyterm"} {
		id, _ := workspace.NewNoteID()
		b, _ := workspace.WithNoteID([]byte(text), id)
		if _, err := store.Save(ctx, p, b, "", "fixture"); err != nil {
			t.Fatal(err)
		}
	}
	st, _ := store.Status(ctx)
	if _, err := store.TrashNotes(ctx, []string{"removed.md"}, st.Revision, "fixture"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save(ctx, "assets/photo.png", []byte{0x89, 'P', 'N', 'G', 0}, "", "fixture"); err != nil {
		t.Fatal(err)
	}
	st, _ = store.Status(ctx)
	if _, err := store.AddComment(ctx, "note.md", "discussiononlyterm", st.Revision, "fixture"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.WriteInstructions(ctx, []byte("instructiononlyterm"), "", "fixture"); err != nil {
		t.Fatal(err)
	}
	st, _ = store.Status(ctx)
	dir := filepath.Join(app.data, "authority-fixture")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	git := func(root string, args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", root, "-c", "core.hooksPath=/dev/null"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatal(err, string(out))
		}
		return strings.TrimSpace(string(out))
	}
	// The upstream starts as a clone of the notebook's Git export, which a
	// backup bundle carries with every commit ID.
	bundle := filepath.Join(dir, "notebook.bundle")
	if err := store.Backup(ctx, bundle); err != nil {
		t.Fatal(err)
	}
	upstreamDir := filepath.Join(dir, "upstream")
	git(dir, "clone", "--no-hardlinks", bundle, upstreamDir)
	git(upstreamDir, "config", "user.name", "Fixture")
	git(upstreamDir, "config", "user.email", "fixture@example.test")
	branch := git(upstreamDir, "branch", "--show-current")
	upstream := reposource.Repository{ID: 42, FullName: "example/notebook", DefaultBranch: branch, Private: true}
	upstream.Permissions.Push = true
	var head atomic.Value
	head.Store(st.Revision)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/user/installations":
			json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "installations": []map[string]int{{"id": 7}}})
		case "/user/installations/7/repositories":
			json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "repositories": []reposource.Repository{upstream}})
		case "/repositories/42":
			json.NewEncoder(w).Encode(upstream)
		case "/repos/example/notebook/installation":
			json.NewEncoder(w).Encode(map[string]any{"id": 7, "permissions": map[string]string{"contents": "write"}})
		case "/app/installations/7/access_tokens":
			json.NewEncoder(w).Encode(map[string]any{"token": "synthetic-token", "expires_at": time.Now().Add(time.Hour)})
		case "/repos/example/notebook/branches":
			json.NewEncoder(w).Encode([]any{})
		case "/repos/example/notebook/git/ref/heads/" + branch:
			json.NewEncoder(w).Encode(map[string]any{"object": map[string]string{"sha": head.Load().(string)}})
		case "/repos/example/notebook/commits/" + branch:
			json.NewEncoder(w).Encode(map[string]string{"sha": head.Load().(string)})
		case "/installation/token":
			w.WriteHeader(204)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	for name, b := range map[string][]byte{"key": pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), "client": []byte("synthetic"), "webhook": []byte(strings.Repeat("s", 32))} {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := reposource.Config{AppID: "1", Slug: "fixture", ClientID: "fixture", PrivateKeyFile: filepath.Join(dir, "key"), ClientSecretFile: filepath.Join(dir, "client"), WebhookSecretFile: filepath.Join(dir, "webhook")}
	g, err := reposource.NewGitHub(cfg)
	if err != nil {
		t.Fatal(err)
	}
	g.API = server.URL
	g.HTTP = server.Client()
	root := filepath.Join(dir, "sources")
	source := strings.Repeat("b", 32)
	cache := filepath.Join(root, "default", source, "source.git")
	if err = os.MkdirAll(filepath.Dir(cache), 0700); err != nil {
		t.Fatal(err)
	}
	git(dir, "clone", "--bare", "--no-hardlinks", upstreamDir, cache)
	state := reposource.State{ID: source, Project: "default", Actor: catalog.Bootstrap, Installation: 7, Repository: upstream, Connected: true, Generation: 1, Downloaded: st.Revision}
	raw, _ := json.Marshal(map[string]any{"version": 1, "active": source, "sources": map[string]reposource.State{source: state}})
	if _, err = app.db.WriteDocument(ctx, app.db.Pool, "repository-sources", "default", 0, raw); err != nil {
		t.Fatal(err)
	}
	m, err := reposource.Open(app.db, root, nil, g, func(p, a string) error { return app.catalog.CheckNotebookOwner(a, p) })
	if err != nil {
		t.Fatal(err)
	}
	app.repositories = m
	app.repositoryConnections = &repositoryConnections{github: g}
	if _, err = m.Refresh(ctx, "default"); err != nil {
		t.Fatal(err)
	}
	backupApp, err := githubapp.New(g.Client.Config, "write")
	if err != nil {
		t.Fatal(err)
	}
	backupApp.API = server.URL
	backupApp.HTTP = server.Client()
	bm, err := backups.Open(app.db, backupApp, nil, func(b backups.Binding) error {
		if app.catalog.Authority(b.Project).Generation != b.NotebookGeneration {
			return backups.ErrDenied
		}
		_, err := app.catalog.Access(b.Actor, b.Project, "owner")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	app.backups = &backupService{manager: bm, app: backupApp, workerHTTP: backuphttp.New(bm, map[string]backuphttp.Worker{"fixture": backuphttp.NewWorker("local", []byte(testToken))}), config: backupConfig{Workers: map[string]backupWorker{"fixture": {Owner: "local"}}}}
	b, err := bm.Configure(ctx, backups.Setup{Binding: backups.Binding{Project: "default", Actor: catalog.Bootstrap, Owner: "local", NotebookGeneration: 1, Installation: 7, Repository: upstream, Branch: branch}, UserToken: "synthetic-user"})
	if err != nil {
		t.Fatal(err)
	}
	l, err := bm.Lease(ctx, "local", "default", b.Generation, st.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = bm.Complete(ctx, "local", backups.Receipt{Lease: l.ID, Project: "default", Generation: b.Generation, Revision: st.Revision}); err != nil {
		t.Fatal(err)
	}
	return app, store, func(files map[string][]byte) {
		t.Helper()
		for p, b := range files {
			if err := os.MkdirAll(filepath.Dir(filepath.Join(upstreamDir, p)), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(upstreamDir, p), b, 0600); err != nil {
				t.Fatal(err)
			}
		}
		git(upstreamDir, "add", ".")
		git(upstreamDir, "commit", "-m", "Upstream edit")
		sha := git(upstreamDir, "rev-parse", "HEAD")
		git(cache, "fetch", upstreamDir, "HEAD")
		head.Store(sha)
	}
}

func reopenAuthorityFixture(t *testing.T, old *Server) *Server {
	t.Helper()
	data, g, bg, config := old.data, old.repositoryConnections.github, old.backups.app, old.backups.config
	old.Close()
	old.catalog.Close()
	c, err := catalog.Open(data, nil, pgtest.Dir(t, data))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	// The instance's database outlives the restart, as its backup bindings do.
	app, err := New(c, old.db, data, "http://localhost", testToken)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	m, err := reposource.Open(app.db, filepath.Join(data, "authority-fixture", "sources"), nil, g, func(p, a string) error { return c.CheckNotebookOwner(a, p) })
	if err != nil {
		t.Fatal(err)
	}
	app.repositories = m
	app.repositoryConnections = &repositoryConnections{github: g}
	bm, err := backups.Open(app.db, bg, nil, func(b backups.Binding) error {
		if c.Authority(b.Project).Generation != b.NotebookGeneration {
			return backups.ErrDenied
		}
		_, err := c.Access(b.Actor, b.Project, "owner")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	app.backups = &backupService{manager: bm, app: bg, workerHTTP: backuphttp.New(bm, map[string]backuphttp.Worker{"fixture": backuphttp.NewWorker("local", []byte(testToken))}), config: config}
	return app
}

func TestAuthorityRecoveryAtEveryPublicationBoundary(t *testing.T) {
	for stop := 0; stop <= 8; stop++ {
		t.Run([]string{"catalog-fence", "owner-fence", "source-fence", "backup-fence", "worker-drained", "published", "owner-completed", "source-completed", "backup-completed"}[stop], func(t *testing.T) {
			app, store, _ := authorityFixture(t)
			ctx := context.WithValue(t.Context(), identityKey{}, catalog.Bootstrap)
			before, err := store.NotebookSnapshot(ctx)
			if err != nil {
				t.Fatal(err)
			}
			p, err := app.previewAuthority(ctx, authorityPreviewInput{Project: "default", To: "github", OperatorWritersStopped: true})
			if err != nil {
				t.Fatal(err)
			}
			p, _, err = app.catalog.BeginAuthority(catalog.Bootstrap, "default", p.ID)
			if err != nil {
				t.Fatal(err)
			}
			steps := []func() error{
				func() error {
					_, e := store.PrepareAuthority(ctx, workspace.AuthorityRequest{ID: p.ID, From: p.From, To: p.To, Generation: p.Generation, Revision: p.Revision})
					return e
				},
				func() error {
					_, e := app.repositories.BeginAuthority(p.Project, p.Actor, p.ID, p.Source, p.SourceGeneration, p.SourceRevision)
					return e
				},
				func() error {
					_, e := app.backups.manager.BeginAuthority(ctx, p.Project, p.ID, p.BackupGeneration, p.Revision)
					return e
				},
				func() error {
					b, e := app.backups.manager.Status(p.Project)
					if e != nil {
						return e
					}
					return app.backups.manager.AcknowledgeDrain("local", p.Project, p.ID, b.Generation)
				},
				func() error { var e error; p, e = app.catalog.PublishAuthority(p.Actor, p.Project, p.ID); return e },
				func() error { _, e := store.FinishAuthority(ctx, p.ID, true); return e },
				func() error { return app.repositories.FinishAuthority(p.Project, p.ID, p.To, true) },
				func() error { return app.backups.manager.FinishAuthority(p.Project, p.ID, true) },
			}
			for _, step := range steps[:stop] {
				if err = step(); err != nil {
					t.Fatal(err)
				}
			}
			app = reopenAuthorityFixture(t, app)
			if err = app.RecoverAuthority(ctx); err != nil {
				t.Fatal(err)
			}
			if err = app.RecoverAuthority(ctx); err != nil {
				t.Fatal("recovery replay", err)
			}
			want, phase, gen := "forgetmenote", "failed", uint64(1)
			if stop >= 5 {
				want, phase, gen = "github", "complete", 2
			}
			if a := app.catalog.Authority("default"); a.Type != want || a.Generation != gen {
				t.Fatal(a)
			}
			p, err = app.catalog.AuthorityTransfer(catalog.Bootstrap, "default")
			if err != nil || p.Phase != phase {
				t.Fatal(p, err)
			}
			store, err = app.catalog.Access(catalog.Bootstrap, "default", "read")
			if err != nil {
				t.Fatal(err)
			}
			after, err := store.NotebookSnapshot(ctx)
			if err != nil || !reflect.DeepEqual(before.Files, after.Files) || after.Revision != before.Revision {
				t.Fatal("recovery changed notebook bytes or history", err)
			}
			_, err = store.Save(ctx, "after.md", []byte("after"), "", "fixture")
			if stop < 5 && err != nil || stop >= 5 && !errors.Is(err, workspace.ErrForbidden) {
				t.Fatal("wrong post-recovery write authority", err)
			}
		})
	}
}

func TestAuthorityRoundTripThroughAPIAndWorker(t *testing.T) {
	app, store, update := authorityFixture(t)
	ctx := t.Context()
	before, err := store.NotebookSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	invoke := func(name string, in any) catalog.AuthorityTransfer {
		t.Helper()
		raw, _ := json.Marshal(in)
		w := apiCall(app, "POST", "/api/v1/actions/"+name, string(raw), testToken)
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", name, w.Code, w.Body.String())
		}
		var p catalog.AuthorityTransfer
		if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	p := invoke("preview_notebook_authority_change", authorityPreviewInput{Project: "default", To: "github", Repository: "example/notebook", OperatorWritersStopped: true})
	p = invoke("apply_notebook_authority_change", authorityApplyInput{Project: "default", Preview: p.ID})
	if p.Phase != "preparing" || app.catalog.Authority("default").Type != "forgetmenote" {
		t.Fatal("published before worker drain", p)
	}
	if _, err := store.Save(ctx, "late.md", []byte("late"), "", "fixture"); !errors.Is(err, workspace.ErrForbidden) {
		t.Fatal("late write accepted", err)
	}
	b, err := app.backups.manager.Status("default")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"project": "default", "handoff": p.ID, "generation": b.Generation})
	r := httptest.NewRequest("POST", app.origin+"/internal/backups/v1/drain", strings.NewReader(string(raw)))
	r.Header.Set("Authorization", "Bearer "+testToken)
	r.Header.Set("X-Backup-Worker", "fixture")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	app.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal("worker acknowledgement", w.Code, w.Body.String())
	}
	p = invoke("apply_notebook_authority_change", authorityApplyInput{Project: "default", Preview: p.ID})
	if p.Phase != "complete" || app.catalog.Authority("default").Generation != 2 {
		t.Fatal(p)
	}
	if again := invoke("apply_notebook_authority_change", authorityApplyInput{Project: "default", Preview: p.ID}); again.ID != p.ID || again.Phase != "complete" {
		t.Fatal("replay not idempotent")
	}
	if _, err = app.repositories.Refresh(ctx, "default"); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"list_documents", "notebook_map"} {
		w := apiCall(app, "POST", "/api/v1/actions/"+action, `{"project":"default","drafts":"exclude","archives":"exclude"}`, testToken)
		if w.Code != 200 || strings.Contains(w.Body.String(), "draft.md") || strings.Contains(w.Body.String(), "archive.md") || strings.Contains(w.Body.String(), "AGENTS.md") || strings.Contains(w.Body.String(), "comments/") {
			t.Fatal(action, w.Code, w.Body.String())
		}
	}
	w = apiCall(app, "POST", "/api/v1/actions/list_documents", `{"project":"default","collection":"drafts"}`, testToken)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "draft.md") || strings.Contains(w.Body.String(), "note.md") {
		t.Fatal("draft selection", w.Code, w.Body.String())
	}
	updated := append(append([]byte{}, before.Files["note.md"]...), []byte("\nupstreamonlyterm")...)
	update(map[string][]byte{"note.md": updated})
	if _, err = app.repositories.Refresh(ctx, "default"); err != nil {
		t.Fatal(err)
	}
	source, _ := app.repositories.Status("default")
	p = invoke("preview_notebook_authority_change", authorityPreviewInput{Project: "default", To: "forgetmenote"})
	p = invoke("apply_notebook_authority_change", authorityApplyInput{Project: "default", Preview: p.ID})
	if p.Phase != "complete" || app.catalog.Authority("default").Generation != 3 {
		t.Fatal(p)
	}
	after, err := store.NotebookSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	before.Files["note.md"] = updated
	if !reflect.DeepEqual(before.Files, after.Files) {
		t.Fatal("round trip dropped or changed notebook files")
	}
	old, err := store.Read(ctx, "note.md", before.Revision)
	if err != nil || strings.Contains(string(old.Content), "upstreamonlyterm") {
		t.Fatal("history lost", err)
	}
	raw, _ = json.Marshal(documentRequest{documentRef: documentRef{Project: "default", Origin: "repository", Source: source.ID, Path: "note.md", Revision: source.Downloaded}})
	w = apiCall(app, "POST", "/api/v1/actions/read_document", string(raw), testToken)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "upstreamonlyterm") {
		t.Fatal("retained source reference", w.Code, w.Body.String())
	}
	if b, err = app.backups.manager.Status("default"); err != nil || b.Connected || b.Enabled || b.AuthorityTarget {
		t.Fatal("backup was automatically resumed", b, err)
	}
	if _, err = store.Save(ctx, "central.md", []byte("central again"), "", "fixture"); err != nil {
		t.Fatal(err)
	}
	if err = app.RecoverAuthority(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestAuthorityStatusAndHistoricalReferencesSurvivePublication(t *testing.T) {
	app, store := fixture(t)
	ctx := t.Context()
	content, _ := workspace.WithNoteID([]byte("# Remember\nretainedonlyterm"), strings.Repeat("a", 32))
	saved, err := store.Save(ctx, "remember.md", content, "", "fixture")
	if err != nil {
		t.Fatal(err)
	}
	doc, _ := store.Read(ctx, "remember.md", "")
	noteID, _ := workspace.NoteID(doc.Content)
	p, err := app.catalog.PreviewAuthority(catalog.Bootstrap, "default", catalog.AuthorityTransfer{To: "github", Revision: saved.Revision})
	if err != nil {
		t.Fatal(err)
	}
	p, _, err = app.catalog.BeginAuthority(catalog.Bootstrap, "default", p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.PrepareAuthority(ctx, workspace.AuthorityRequest{ID: p.ID, Generation: p.Generation, From: p.From, To: p.To, Revision: p.Revision}); err != nil {
		t.Fatal(err)
	}
	p, err = app.catalog.PublishAuthority(catalog.Bootstrap, "default", p.ID)
	if err != nil {
		t.Fatal(err)
	}
	w := apiCall(app, "POST", "/api/v1/actions/notebook_authority_status", `{"project":"default"}`, testToken)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"phase":"publishing"`) {
		t.Fatalf("handoff status unavailable: %d %s", w.Code, w.Body.String())
	}
	if _, err = store.FinishAuthority(ctx, p.ID, true); err != nil {
		t.Fatal(err)
	}
	if err = app.catalog.CompleteAuthority(p.Project, p.ID, p.Revision); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]string{"project": "default", "path": "remember.md", "noteId": noteID, "revision": saved.Revision})
	w = apiCall(app, "POST", "/api/v1/actions/read_document", string(raw), testToken)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "retainedonlyterm") {
		t.Fatalf("old citation lost: %d %s", w.Code, w.Body.String())
	}
	w = apiCall(app, "POST", "/api/v1/actions/search_notebooks", `{"projects":["default"],"query":"retainedonlyterm","mode":"text"}`, testToken)
	var found scopedRetrieval
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &found) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	if len(found.Results) != 0 {
		t.Fatal("retained editable tree leaked into current GitHub corpus")
	}
}

func TestAuthorityImportRecoveryBeforeAndAfterPublication(t *testing.T) {
	for _, step := range []string{"staged", "published", "imported"} {
		t.Run(step, func(t *testing.T) {
			app, store, update := authorityFixture(t)
			ctx := context.WithValue(t.Context(), identityKey{}, catalog.Bootstrap)
			p, err := app.previewAuthority(ctx, authorityPreviewInput{Project: "default", To: "github", OperatorWritersStopped: true})
			if err != nil {
				t.Fatal(err)
			}
			p, err = app.applyAuthority(ctx, authorityApplyInput{Project: "default", Preview: p.ID})
			if err != nil {
				t.Fatal(err)
			}
			b, err := app.backups.manager.Status("default")
			if err != nil {
				t.Fatal(err)
			}
			if err = app.backups.manager.AcknowledgeDrain("local", "default", p.ID, b.Generation); err != nil {
				t.Fatal(err)
			}
			p, err = app.applyAuthority(ctx, authorityApplyInput{Project: "default", Preview: p.ID})
			if err != nil || p.Phase != "complete" {
				t.Fatal(p, err)
			}
			before, err := store.NotebookSnapshot(ctx)
			if err != nil {
				t.Fatal(err)
			}
			update(map[string][]byte{"empty.md": {}, "new.md": []byte("# Upstream addition")})
			if _, err = app.repositories.Refresh(ctx, "default"); err != nil {
				t.Fatal(err)
			}
			p, err = app.previewAuthority(ctx, authorityPreviewInput{Project: "default", To: "forgetmenote"})
			if err != nil {
				t.Fatal(err)
			}
			p, _, err = app.catalog.BeginAuthority(p.Actor, p.Project, p.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = store.PrepareAuthority(ctx, workspace.AuthorityRequest{ID: p.ID, From: p.From, To: p.To, Generation: p.Generation, Revision: p.Revision}); err != nil {
				t.Fatal(err)
			}
			if _, err = app.repositories.BeginAuthority(p.Project, p.Actor, p.ID, p.Source, p.SourceGeneration, p.SourceRevision); err != nil {
				t.Fatal(err)
			}
			snapshot, err := app.repositories.NotebookSnapshot(ctx, p.Project, p.Source, p.SourceRevision, p.SourceBranch)
			if err != nil {
				t.Fatal(err)
			}
			if err = app.repositories.PinNotebook(ctx, p.Project, p.ID, snapshot); err != nil {
				t.Fatal(err)
			}
			if _, err = store.StageAuthority(ctx, p.ID, snapshot); err != nil {
				t.Fatal(err)
			}
			if step != "staged" {
				p, err = app.catalog.PublishAuthority(p.Actor, p.Project, p.ID)
				if err != nil {
					t.Fatal(err)
				}
			}
			if step == "imported" {
				if _, err = store.FinishAuthority(ctx, p.ID, true); err != nil {
					t.Fatal(err)
				}
			}
			app = reopenAuthorityFixture(t, app)
			savedSources := app.repositories
			app.repositories = nil
			if err = app.RecoverAuthority(ctx); err == nil || !strings.Contains(err.Error(), "modules on") || !app.catalog.AuthorityChanging("default") {
				t.Fatal("missing provider silently cleared fence", err)
			}
			app.repositories = savedSources
			if err = app.RecoverAuthority(ctx); err != nil {
				t.Fatal(err)
			}
			store, err = app.catalog.Access(catalog.Bootstrap, "default", "read")
			if err != nil {
				t.Fatal(err)
			}
			after, err := store.NotebookSnapshot(ctx)
			if err != nil {
				t.Fatal(err)
			}
			want := snapshot.Files
			if step == "staged" {
				want = before.Files
			}
			if !reflect.DeepEqual(want, after.Files) {
				t.Fatal("recovery did not preserve the complete expected tree")
			}
			if _, err = store.Read(ctx, "note.md", before.Revision); err != nil {
				t.Fatal("lost original history", err)
			}
		})
	}
}

func TestAuthorityPreviewRejectsUpstreamMovement(t *testing.T) {
	app, store, update := authorityFixture(t)
	ctx := context.WithValue(t.Context(), identityKey{}, catalog.Bootstrap)
	p, err := app.previewAuthority(ctx, authorityPreviewInput{Project: "default", To: "github", OperatorWritersStopped: true})
	if err != nil {
		t.Fatal(err)
	}
	update(map[string][]byte{"other.md": []byte("newer upstream")})
	if _, err = app.applyAuthority(ctx, authorityApplyInput{Project: "default", Preview: p.ID}); err == nil {
		t.Fatal("stale upstream was accepted")
	}
	if app.catalog.Authority("default").Type != "forgetmenote" || app.catalog.AuthorityChanging("default") {
		t.Fatal("failed preview changed authority")
	}
	if _, err = store.Save(ctx, "local.md", []byte("still editable"), "", "fixture"); err != nil {
		t.Fatal("failed preview left local writes fenced", err)
	}
}

// A pending handoff whose lock another gateway holds is that gateway's: this
// gateway neither recovers nor applies it until the lock is free.
func TestAuthorityRecoveryLeavesAHandoffAnotherGatewayHolds(t *testing.T) {
	app, _, _ := authorityFixture(t)
	ctx := context.WithValue(t.Context(), identityKey{}, catalog.Bootstrap)
	p, err := app.previewAuthority(ctx, authorityPreviewInput{Project: "default", To: "github", OperatorWritersStopped: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = app.catalog.BeginAuthority(catalog.Bootstrap, "default", p.ID); err != nil {
		t.Fatal(err)
	}
	gateway, _ := pgtest.Gateway(t, app.db)
	release, ok, err := gateway.TryExclusive(ctx, notebooks.HandoffLock("default"))
	if err != nil || !ok {
		t.Fatal("hold the handoff lock", ok, err)
	}
	if err = app.RecoverAuthority(ctx); err != nil {
		t.Fatal(err)
	}
	if !app.catalog.AuthorityChanging("default") {
		t.Fatal("recovery took over a handoff another gateway holds")
	}
	if _, err = app.applyAuthority(ctx, authorityApplyInput{Project: "default", Preview: p.ID}); !errors.Is(err, workspace.ErrConflict) {
		t.Fatal("a handoff another gateway holds was applied here", err)
	}
	release()
	if err = app.RecoverAuthority(ctx); err != nil {
		t.Fatal(err)
	}
	if p, err = app.catalog.AuthorityTransfer(catalog.Bootstrap, "default"); err != nil || p.Phase != "failed" || app.catalog.AuthorityChanging("default") {
		t.Fatal("the released handoff was not recovered", p.Phase, err)
	}
}
