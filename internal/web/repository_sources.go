package web

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"time"

	"forgetmenote/internal/pg"
	"forgetmenote/internal/reposource"
	"forgetmenote/internal/workspace"
)

type repositoryGrant struct {
	Connection                                 string
	Subject, Project, Binding, Verifier, Token string
	Name                                       string
	Creating, Created                          bool
	ExternalProcessing                         bool
	Repository, Installation                   int64
	Generation                                 uint64
	Expires                                    time.Time
}
type repositoryConnections struct {
	github *reposource.GitHub
}

func (s *Server) ConfigureRepositorySources(file string) error {
	if err := s.privateRuntimeFile(file); err != nil {
		return err
	}
	cfg, err := reposource.LoadConfig(file)
	if err != nil {
		return err
	}
	return s.ConfigureRepositorySourcesConfig(cfg)
}
func (s *Server) ConfigureRepositorySourcesConfig(cfg reposource.Config) error {
	unlock, err := s.startupConfiguration()
	if err != nil {
		return err
	}
	defer unlock()

	if err := cfg.Validate(); err != nil {
		return err
	}
	for _, file := range []string{cfg.ClientSecretFile, cfg.PrivateKeyFile, cfg.WebhookSecretFile} {
		if err := s.privateRuntimeFile(file); err != nil {
			return err
		}
	}
	g, err := reposource.NewGitHub(cfg)
	if err != nil {
		return err
	}
	manager, err := reposource.Open(s.db, filepath.Join(s.data, "repository-sources"), s.blobs, g, func(project, actor string) error { return s.catalog.CheckNotebookOwner(actor, project) })
	if err != nil {
		return err
	}
	s.repositories = manager
	s.repositoryConnections = &repositoryConnections{github: g}
	manager.SetEmbedder(s.sourceEmbedder)
	manager.SetMapClassifier(s.sourceMapModel, s.classifySourceMap)
	manager.SetIndexPolicy(s.sourceIndexPolicy)
	return nil
}
func (s *Server) repositoryRoutes() {
	s.documentRoutes()
	s.repositoryCreationRoutes()
	s.mux.HandleFunc("POST /api/retrieval", s.retrievalHTTP)
	s.repositorySourceRoutes()
	s.handleProject("POST /api/repository/connect", "owner", s.repositoryConnect)
	s.handleProject("GET /api/repository/choices", "owner", s.repositoryChoices)
	s.handleProject("POST /api/repository/select", "owner", s.repositorySelect)
	s.handleProject("POST /api/repository/control", "owner", s.repositoryControl)
	s.mux.HandleFunc("GET /repository/github/callback", s.repositoryCallback)
}
func (s *Server) repositoryConnect(w http.ResponseWriter, r *http.Request) {
	g, err := s.repoApp()
	if err != nil {
		fail(w, err)
		return
	}
	var in struct{}
	if !decode(w, r, &in) {
		return
	}
	s.beginRepositoryConnection(w, r, g, repositoryGrant{Project: projectID(r.Context())})
}
func (s *Server) beginRepositoryConnection(w http.ResponseWriter, r *http.Request, g *reposource.GitHub, grant repositoryGrant) {
	a, err := s.catalog.Account(subject(r.Context()))
	if err != nil {
		fail(w, err)
		return
	}
	state, binding, verifier := RandomToken(), RandomToken(), RandomToken()
	grant.Subject, grant.Binding, grant.Verifier, grant.Generation, grant.Expires = subject(r.Context()), digest(binding), verifier, a.Generation, time.Now().Add(10*time.Minute)
	if err = s.beginFlow(r.Context(), "repository/pending", digest(state), grant.Subject, grant, 10*time.Minute, pg.FlowBound{Limit: flowLimit, Replace: true}); err != nil {
		fail(w, err)
		return
	}
	s.setBinding(w, repositoryBinding, binding)
	challenge := sha256.Sum256([]byte(verifier))
	q := url.Values{"client_id": {g.Config.ClientID}, "redirect_uri": {s.origin + "/repository/github/callback"}, "state": {state}, "code_challenge": {base64.RawURLEncoding.EncodeToString(challenge[:])}, "code_challenge_method": {"S256"}}
	if grant.Connection != "" {
		http.Redirect(w, r, "https://github.com/login/oauth/authorize?"+q.Encode(), http.StatusSeeOther)
		return
	}
	jsonResponse(w, map[string]string{"url": "https://github.com/login/oauth/authorize?" + q.Encode()})
}
func (s *Server) repositoryCallback(w http.ResponseWriter, r *http.Request) {
	g, err := s.repoApp()
	if err != nil {
		fail(w, err)
		return
	}
	binding, err := s.binding(r, repositoryBinding)
	if err != nil {
		fail(w, reposource.ErrDenied)
		return
	}
	p, valid, err := claimFlow(s, r.Context(), "repository/pending", digest(r.URL.Query().Get("state")), func(p repositoryGrant) bool {
		return p.Subject == subject(r.Context()) && p.Expires.After(time.Now()) && subtle.ConstantTimeCompare([]byte(p.Binding), []byte(digest(binding))) == 1
	})
	if err != nil {
		fail(w, err)
		return
	}
	if !valid {
		fail(w, reposource.ErrDenied)
		return
	}
	a, err := s.catalog.Account(p.Subject)
	if err != nil || a.Generation != p.Generation {
		fail(w, reposource.ErrDenied)
		return
	}
	if p.Creating {
		err = s.catalog.CheckCreate(p.Subject, p.Project, p.Name)
	} else {
		_, err = s.catalog.Access(p.Subject, p.Project, "owner")
	}
	if err != nil {
		fail(w, err)
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" || len(code) > 1024 || r.URL.Query().Get("error") != "" {
		fail(w, reposource.ErrDenied)
		return
	}
	token, err := g.Exchange(r.Context(), code, p.Verifier, s.origin+"/repository/github/callback")
	if err != nil {
		fail(w, err)
		return
	}
	p.Token = token
	p.Verifier = ""
	if err = s.saveGrant(r.Context(), "repository/grant", g.Client, p); err != nil {
		fail(w, err)
		return
	}
	if p.Connection != "" {
		if err = s.connectionReady(r.Context(), p); err != nil {
			fail(w, err)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, "GitHub authorized. Return to your terminal to finish the connection.")
		return
	}
	if p.Creating {
		http.Redirect(w, r, "/?repository=create&notebook="+url.QueryEscape(p.Project), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/?project="+url.QueryEscape(p.Project)+"&repository=connect", http.StatusSeeOther)
}
func (s *Server) repositoryGrant(r *http.Request) (repositoryGrant, error) {
	g, err := s.repoApp()
	if err != nil {
		return repositoryGrant{}, err
	}
	binding, err := s.binding(r, repositoryBinding)
	if err != nil {
		return repositoryGrant{}, reposource.ErrDenied
	}
	p, ok, err := s.grant(r.Context(), "repository/grant", digest(binding), g.Client)
	if err != nil {
		return p, err
	}
	if !ok || p.Subject != subject(r.Context()) || (p.Project != projectID(r.Context()) && (!p.Creating || p.Project != r.URL.Query().Get("project"))) || p.Expires.Before(time.Now()) {
		return p, reposource.ErrDenied
	}
	return p, s.repositoryGrantAuthority(p)
}
func (s *Server) repositoryGrantAuthority(p repositoryGrant) error {
	a, err := s.catalog.Account(p.Subject)
	if err != nil || a.Generation != p.Generation || p.Expires.Before(time.Now()) {
		return reposource.ErrDenied
	}
	if p.Creating && !p.Created {
		if !s.catalog.CanCreateProjects(p.Subject) {
			return workspace.ErrForbidden
		}
		return nil
	}
	_, err = s.catalog.Access(p.Subject, p.Project, "owner")
	return err
}
func (s *Server) repositoryChoices(w http.ResponseWriter, r *http.Request) {
	g, err := s.repoApp()
	if err != nil {
		fail(w, err)
		return
	}
	p, err := s.repositoryGrant(r)
	if err != nil {
		fail(w, err)
		return
	}
	choices, err := g.Choices(r.Context(), p.Token)
	respond(w, choices, err)
}
func (s *Server) repositorySelect(w http.ResponseWriter, r *http.Request) {
	repositories, err := s.repos()
	if err != nil {
		fail(w, err)
		return
	}
	var in struct {
		Installation      int64 `json:"installation"`
		Repository        int64 `json:"repository"`
		ShareWithNotebook bool  `json:"shareWithNotebook"`
	}
	if !decode(w, r, &in) {
		return
	}
	if !in.ShareWithNotebook {
		http.Error(w, "Confirm sharing source material with this notebook's members", 400)
		return
	}
	p, err := s.repositoryGrant(r)
	if err != nil {
		fail(w, err)
		return
	}
	state, err := integrationActivity(s, p.Subject, "repository.connect", p.Project, func() (reposource.State, error) {
		return repositories.Connect(r.Context(), p.Project, p.Subject, p.Token, in.Installation, in.Repository)
	})
	if err != nil {
		fail(w, err)
		return
	}
	// A grant left behind expires with its flow.
	_ = s.endFlow(r.Context(), "repository/grant", p.Binding)
	s.clearBinding(w, repositoryBinding)
	jsonResponse(w, state)
}
func (s *Server) repositoryControl(w http.ResponseWriter, r *http.Request) {
	repositories, err := s.repos()
	if err != nil {
		fail(w, err)
		return
	}
	var in struct {
		Action string `json:"action"`
	}
	if !decode(w, r, &in) {
		return
	}
	state, err := integrationActivity(s, subject(r.Context()), "repository."+in.Action, projectID(r.Context()), func() (reposource.State, error) {
		return repositories.Control(projectID(r.Context()), subject(r.Context()), in.Action)
	})
	respond(w, state, err)
}
func (s *Server) repositoryWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "POST required", 405)
		return
	}
	g, err := s.repoApp()
	if err != nil {
		fail(w, err)
		return
	}
	repositories, err := s.repos()
	if err != nil {
		fail(w, err)
		return
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, (1<<20)+1))
	if err != nil || len(b) > 1<<20 {
		http.Error(w, "Webhook too large", 413)
		return
	}
	if !g.VerifyWebhook(b, r.Header.Get("X-Hub-Signature-256")) {
		fail(w, reposource.ErrDenied)
		return
	}
	var event struct {
		Repository struct {
			ID int64 `json:"id"`
		} `json:"repository"`
		Installation struct {
			ID int64 `json:"id"`
		} `json:"installation"`
	}
	if json.Unmarshal(b, &event) != nil {
		http.Error(w, "Invalid webhook", 400)
		return
	}
	kind := r.Header.Get("X-GitHub-Event")
	if kind == "push" || kind == "repository" || kind == "installation_repositories" || kind == "installation" {
		err = repositories.Wake(event.Repository.ID, event.Installation.ID, r.Header.Get("X-GitHub-Delivery"))
		if err != nil {
			fail(w, err)
			return
		}
	}
	w.WriteHeader(http.StatusAccepted)
}
