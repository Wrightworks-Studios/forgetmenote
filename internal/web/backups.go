package web

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"forgetmenote/internal/durable"
	"forgetmenote/internal/jsonfile"
	"io"
	"net/http"
	"net/url"
	"time"

	"forgetmenote/internal/backuphttp"
	"forgetmenote/internal/backups"
	"forgetmenote/internal/drivebackup"
	"forgetmenote/internal/githubapp"
	"forgetmenote/internal/pg"
	"forgetmenote/internal/workspace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type backupWorker = backups.WorkerConfig
type backupConfig = backups.AppConfig

// A backupService is the instance's managed backups: a notebook's GitHub
// binding through the Backups GitHub App, its Google Drive and Dropbox
// destinations through drives, or both. Either may be absent.
type backupService struct {
	manager    *backups.Manager
	workerHTTP *backuphttp.HTTP
	app        *githubapp.Client
	drives     *drivebackup.Client
	config     backupConfig
}

// ConfigureBackups and ConfigureBackupsConfig install the Backups GitHub App,
// the drive providers and the workers. On an instance that turns the
// managed-backups module off they install nothing, and the file, the secrets
// and the worker secrets stay unread.
func (s *Server) ConfigureBackups(file string) error {
	if !s.modules.Enabled("managed-backups") {
		return nil
	}
	if err := s.privateRuntimeFile(file); err != nil {
		return err
	}
	var cfg backupConfig
	if err := jsonfile.Load(file, 65536, &cfg); err != nil {
		return errors.New("invalid backup App configuration")
	}
	return s.ConfigureBackupsConfig(cfg)
}
func (s *Server) ConfigureBackupsConfig(cfg backups.AppConfig) error {
	unlock, err := s.startupConfiguration()
	if err != nil {
		return err
	}
	defer unlock()
	if !s.modules.Enabled("managed-backups") {
		return nil
	}

	if len(cfg.Workers) == 0 || len(cfg.Workers) > 256 || !cfg.GitHub() && cfg.Drives == nil {
		return errors.New("invalid backup App configuration")
	}
	if cfg.GitHub() && s.repositoryConnections != nil && cfg.App.AppID == s.repositoryConnections.github.Config.AppID {
		return errors.New("Sources and Backups require separate GitHub Apps")
	}
	var secretFiles []string
	if cfg.GitHub() {
		secretFiles = append(secretFiles, cfg.App.ClientSecretFile, cfg.App.PrivateKeyFile, cfg.App.WebhookSecretFile)
	}
	if cfg.Drives != nil {
		secretFiles = append(secretFiles, cfg.Drives.SecretFiles()...)
	}
	for _, v := range cfg.Workers {
		secretFiles = append(secretFiles, v.TokenFile)
	}
	for _, file := range secretFiles {
		if err := s.privateRuntimeFile(file); err != nil {
			return err
		}
	}
	workers := map[string]backuphttp.Worker{}
	for id, w := range cfg.Workers {
		if id == "" || w.Owner == "" {
			return errors.New("worker needs an ID and storage owner")
		}
		// The byte limit admits a 4096-byte secret followed by its newline.
		secret, e := durable.ReadSecret(w.TokenFile, 4097)
		if errors.Is(e, durable.ErrOpen) {
			return errors.New("cannot read backup worker secret")
		}
		if e != nil || len(secret) < 32 || len(secret) > 4096 {
			return errors.New("invalid backup worker secret")
		}
		workers[id] = backuphttp.NewWorker(w.Owner, []byte(secret))
	}
	service := &backupService{config: cfg}
	if cfg.GitHub() {
		if service.app, err = githubapp.New(cfg.App, "write"); err != nil {
			return err
		}
	}
	if cfg.Drives != nil {
		if service.drives, err = drivebackup.New(*cfg.Drives); err != nil {
			return err
		}
	}
	// Every lease and status rechecks the person who connected the
	// destination: still the notebook's Owner, with the same sign-in and
	// notebook access epochs, under the same notebook authority, and the
	// notebook's own person when it is personal.
	service.manager, err = backups.Open(s.db, service.app, service.drives, func(b backups.Binding) error {
		if b.NotebookGeneration > 0 && s.catalog.Authority(b.Project).Generation != b.NotebookGeneration || s.catalog.Kind(b.Project) != "notes" || s.catalog.StorageOwner(b.Project) != b.Owner || s.catalog.Epoch(b.Actor) != b.Epoch || s.catalog.ProjectAccessEpoch(b.Actor, b.Project) != b.Authority {
			return backups.ErrDenied
		}
		if err := s.personalBackupAllowed(b.Actor, b.Project); err != nil {
			return err
		}
		_, e := s.catalog.Access(b.Actor, b.Project, "owner")
		return e
	})
	if err == nil {
		service.manager.Outgoing = s.outgoing
		service.workerHTTP = backuphttp.New(service.manager, workers)
		s.backups = service
	}
	return err
}

// backupRoutes are the managed-backups module's routes: connecting a
// notebook's backup and controlling it.
func (s *Server) backupRoutes(register routeRegistrar) {
	register.project("GET /api/managed-backup", "owner", func(w http.ResponseWriter, r *http.Request) {
		out, err := s.backupStatus(r.Context(), projectID(r.Context()))
		respond(w, out, err)
	})
	register.project("POST /api/managed-backup/connect", "owner", s.backupConnect)
	register.project("GET /api/managed-backup/choices", "owner", s.backupChoices)
	register.project("POST /api/managed-backup/select", "owner", s.backupSelect)
	register.project("POST /api/managed-backup/create", "owner", s.backupCreate)
	register.project("POST /api/managed-backup/control", "owner", func(w http.ResponseWriter, r *http.Request) {
		if s.catalog.Kind(projectID(r.Context())) != "notes" {
			fail(w, workspace.ErrForbidden)
			return
		}
		if !s.backupsAvailable(w) {
			return
		}
		if err := s.outgoing.Check(); err != nil {
			fail(w, err)
			return
		}
		var in struct {
			Kind       string `json:"kind"`
			Generation uint64 `json:"generation"`
			Action     string `json:"action"`
		}
		if !decode(w, r, &in) {
			return
		}
		out, err := s.controlBackup(r.Context(), projectID(r.Context()), in.Kind, in.Generation, in.Action)
		respond(w, out, err)
	})
	register.global("GET /backup/github/callback", s.backupCallback)
	s.driveRoutes(register)
}
func (s *Server) backupStatus(ctx context.Context, project string) (any, error) {
	store, err := s.toolStore(ctx, project, "owner")
	if err != nil {
		return nil, err
	}
	if s.backups == nil {
		return map[string]bool{"configured": false}, nil
	}
	b, err := s.backups.manager.Status(project)
	if err != nil {
		return nil, err
	}
	st, err := store.Status(ctx)
	if err != nil {
		return nil, err
	}
	drives, err := s.driveStatus(project, st.Revision)
	if err != nil {
		return nil, err
	}
	// configured, the binding's fields and installUrl describe the GitHub
	// destination, as before drives; drives lists each configured drive kind.
	out := struct {
		backups.Binding
		Configured  bool          `json:"configured"`
		AllowCreate bool          `json:"allowCreate"`
		Pending     bool          `json:"pending"`
		Head        string        `json:"head"`
		InstallURL  string        `json:"installUrl,omitempty"`
		Drives      []driveStatus `json:"drives"`
		Connectable bool          `json:"connectable"`
	}{Binding: backups.Binding{Project: project}, Head: st.Revision, Drives: drives, Connectable: s.personalBackupAllowed(subject(ctx), project) == nil}
	if s.backups.app != nil {
		out.Binding, out.Configured, out.AllowCreate, out.Pending, out.InstallURL = b, true, s.backups.config.AllowCreate, b.LastRevision != st.Revision, "https://github.com/apps/"+s.backups.config.App.Slug+"/installations/new"
	}
	return out, nil
}

// backupReady is the GitHub backup service for a browser request that may
// act on the selected notebook's GitHub binding; otherwise it writes the
// refusal.
func (s *Server) backupReady(w http.ResponseWriter, r *http.Request) (*backupService, bool) {
	if s.catalog.Kind(projectID(r.Context())) != "notes" {
		fail(w, workspace.ErrForbidden)
		return nil, false
	}
	service, err := s.gitHubBackups()
	if err == nil {
		err = s.personalBackupAllowed(subject(r.Context()), projectID(r.Context()))
	}
	if err == nil {
		err = s.outgoing.Check()
	}
	if err != nil {
		fail(w, err)
		return nil, false
	}
	return service, true
}
func (s *Server) backupConnect(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.backupReady(w, r); !ok {
		return
	}
	var in struct{}
	if !decode(w, r, &in) {
		return
	}
	s.beginBackupConnection(w, r, repositoryGrant{Project: projectID(r.Context())})
}
func (s *Server) beginBackupConnection(w http.ResponseWriter, r *http.Request, grant repositoryGrant) {
	// A backup setup ends in a destination for pushes; a paused copy starts none.
	if err := s.outgoing.Check(); err != nil {
		fail(w, err)
		return
	}
	a, err := s.catalog.Account(subject(r.Context()))
	if err != nil {
		fail(w, err)
		return
	}
	state, binding, verifier := RandomToken(), RandomToken(), RandomToken()
	g := s.backups
	grant.Subject, grant.Binding, grant.Verifier, grant.Generation, grant.Expires = subject(r.Context()), digest(binding), verifier, a.Generation, time.Now().Add(10*time.Minute)
	if err = s.beginFlow(r.Context(), "backup/pending", digest(state), grant.Subject, grant, 10*time.Minute, pg.FlowBound{Limit: flowLimit, Replace: true}); err != nil {
		fail(w, err)
		return
	}
	s.setBinding(w, backupBinding, binding)
	hash := sha256.Sum256([]byte(verifier))
	q := url.Values{"client_id": {g.config.App.ClientID}, "redirect_uri": {s.origin + "/backup/github/callback"}, "state": {state}, "code_challenge": {base64.RawURLEncoding.EncodeToString(hash[:])}, "code_challenge_method": {"S256"}}
	if grant.Connection != "" {
		http.Redirect(w, r, "https://github.com/login/oauth/authorize?"+q.Encode(), http.StatusSeeOther)
		return
	}
	jsonResponse(w, map[string]string{"url": "https://github.com/login/oauth/authorize?" + q.Encode()})
}
func (s *Server) backupCallback(w http.ResponseWriter, r *http.Request) {
	g, err := s.gitHubBackups()
	if err != nil {
		fail(w, err)
		return
	}
	if err := s.outgoing.Check(); err != nil {
		fail(w, err)
		return
	}
	binding, err := s.binding(r, backupBinding)
	if err != nil {
		fail(w, backups.ErrDenied)
		return
	}
	p, valid, err := claimFlow(s, r.Context(), "backup/pending", digest(r.URL.Query().Get("state")), func(p repositoryGrant) bool {
		return p.Subject == subject(r.Context()) && p.Expires.After(time.Now()) && subtle.ConstantTimeCompare([]byte(p.Binding), []byte(digest(binding))) == 1
	})
	if err != nil {
		fail(w, err)
		return
	}
	if !valid || s.backupGrantAuthority(p) != nil {
		fail(w, backups.ErrDenied)
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" || len(code) > 1024 || r.URL.Query().Get("error") != "" {
		fail(w, backups.ErrDenied)
		return
	}
	p.Token, err = g.app.Exchange(r.Context(), code, p.Verifier, s.origin+"/backup/github/callback")
	if err != nil {
		fail(w, err)
		return
	}
	if s.backupGrantAuthority(p) != nil {
		fail(w, backups.ErrDenied)
		return
	}
	p.Verifier = ""
	if err = s.saveGrant(r.Context(), "backup/grant", g.app, p); err != nil {
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
	http.Redirect(w, r, "/?project="+url.QueryEscape(p.Project)+"&backup=connect", http.StatusSeeOther)
}
func (s *Server) backupGrantAuthority(p repositoryGrant) error {
	a, err := s.catalog.Account(p.Subject)
	if err != nil || a.Generation != p.Generation || p.Expires.Before(time.Now()) || s.catalog.Kind(p.Project) != "notes" {
		return backups.ErrDenied
	}
	if err = s.personalBackupAllowed(p.Subject, p.Project); err != nil {
		return err
	}
	_, err = s.catalog.Access(p.Subject, p.Project, "owner")
	return err
}
func (s *Server) backupGrant(r *http.Request) (repositoryGrant, error) {
	binding, err := s.binding(r, backupBinding)
	if err != nil {
		return repositoryGrant{}, backups.ErrDenied
	}
	p, ok, err := s.grant(r.Context(), "backup/grant", digest(binding), s.backups.app)
	if err != nil {
		return p, err
	}
	if !ok || p.Subject != subject(r.Context()) || p.Project != projectID(r.Context()) {
		return p, backups.ErrDenied
	}
	return p, s.backupGrantAuthority(p)
}
func (s *Server) backupChoices(w http.ResponseWriter, r *http.Request) {
	service, ok := s.backupReady(w, r)
	if !ok {
		return
	}
	p, err := s.backupGrant(r)
	if err != nil {
		fail(w, err)
		return
	}
	choices, err := service.app.Choices(r.Context(), p.Token)
	if err != nil {
		fail(w, err)
		return
	}
	out := []githubapp.Choice{}
	for _, c := range choices {
		if c.Private && c.Permissions.Push {
			out = append(out, c)
		}
	}
	jsonResponse(w, out)
}
func (s *Server) backupSetup(p repositoryGrant, generation uint64) backups.Setup {
	return backups.Setup{Binding: backups.Binding{Project: p.Project, Actor: p.Subject, Owner: s.catalog.StorageOwner(p.Project), Epoch: s.catalog.Epoch(p.Subject), Authority: s.catalog.ProjectAccessEpoch(p.Subject, p.Project), Generation: generation, NotebookGeneration: s.catalog.Authority(p.Project).Generation}, UserToken: p.Token}
}
func (s *Server) backupSelect(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.backupReady(w, r); !ok {
		return
	}
	var in struct {
		Installation int64  `json:"installation"`
		Repository   int64  `json:"repository"`
		Generation   uint64 `json:"generation"`
		Branch       string `json:"branch"`
		ShareHistory bool   `json:"shareHistory"`
	}
	if !decode(w, r, &in) {
		return
	}
	if !in.ShareHistory {
		http.Error(w, "Confirm sharing the complete committed history with the destination's GitHub readers", 400)
		return
	}
	p, err := s.backupGrant(r)
	if err != nil {
		fail(w, err)
		return
	}
	out, err := s.selectManagedBackup(r.Context(), p, in.Installation, in.Repository, in.Generation, in.Branch)
	respond(w, out, err)
}
func (s *Server) selectManagedBackup(ctx context.Context, p repositoryGrant, installation, repository int64, generation uint64, branch string) (backups.Binding, error) {
	if err := s.backupGrantAuthority(p); err != nil {
		return backups.Binding{}, err
	}
	if repositories := s.storedRepositories(); repositories != nil {
		st, e := repositories.Status(p.Project)
		if e == nil && st.Repository.ID == repository {
			return backups.Binding{}, backups.ErrDenied
		}
	}
	setup := s.backupSetup(p, generation)
	setup.Installation = installation
	setup.Repository.ID = repository
	setup.Branch = branch
	return integrationActivity(s, p.Subject, "backup.connect", p.Project, func() (backups.Binding, error) { return s.backups.manager.Configure(ctx, setup) })
}

func (s *Server) backupCreate(w http.ResponseWriter, r *http.Request) {
	service, ok := s.backupReady(w, r)
	if !ok {
		return
	}
	if !service.config.AllowCreate {
		fail(w, backups.ErrDenied)
		return
	}
	var in struct {
		Owner      string `json:"owner"`
		Name       string `json:"name"`
		Generation uint64 `json:"generation"`
		Confirm    bool   `json:"confirm"`
	}
	if !decode(w, r, &in) {
		return
	}
	if !in.Confirm {
		http.Error(w, "Confirm creating a private GitHub repository", 400)
		return
	}
	p, err := s.backupGrant(r)
	if err != nil {
		fail(w, err)
		return
	}
	out, err := integrationActivity(s, p.Subject, "backup.create", p.Project, func() (githubapp.Repository, error) {
		return service.manager.Create(r.Context(), s.backupSetup(p, in.Generation), in.Owner, in.Name)
	})
	respond(w, out, err)
}

func (s *Server) backupWorkerHTTP(w http.ResponseWriter, r *http.Request) {
	if s.backups == nil {
		http.Error(w, "Worker authentication required", 401)
		return
	}
	s.backups.workerHTTP.ServeHTTP(w, r)
}
func (s *Server) backupTools(server *mcp.Server) {
	addManagementTool(s, server, "managed_backup_status", "Read the selected notebook's managed backup status: its GitHub binding and its Google Drive and Dropbox destinations, with each one's last verified snapshot. Requires Owner and notes:manage. Only saved Git history is backed up.", true, func(ctx context.Context, in projectArg) (any, error) { return s.backupStatus(ctx, in.Project) })
	addManagementTool(s, server, "control_managed_backup", "Pause, resume, disconnect, or request an immediate backup of an already configured private destination. kind is github (the default), gdrive or dropbox. Requires Owner and notes:manage; expected generation prevents stale changes. Resume a divergent destination only after the user resolves it. Setup, encryption and destination sharing consent happen in the browser.", false, func(ctx context.Context, in struct {
		projectArg
		Kind       string `json:"kind,omitempty"`
		Generation uint64 `json:"generation"`
		Action     string `json:"action"`
	}) (any, error) {
		if _, err := s.toolStore(ctx, in.Project, "owner"); err != nil {
			return nil, err
		}
		return s.controlBackup(ctx, in.Project, in.Kind, in.Generation, in.Action)
	})
	addManagementTool(s, server, "managed_backup_overview", "List every notebook's managed backup destinations and when each last verified a snapshot, notebooks without one first. Admin only; no credentials. Operator-run host backups are not visible to the app.", true, func(ctx context.Context, in struct{}) (backupOverview, error) {
		return s.backupOverview(subject(ctx))
	})
}

// controlBackup applies an Owner's control to one destination of a notebook,
// for the browser and MCP alike; the caller has checked the Owner's role and
// credential scope.
func (s *Server) controlBackup(ctx context.Context, project, kind string, generation uint64, action string) (any, error) {
	if s.catalog.Kind(project) != "notes" {
		return nil, workspace.ErrForbidden
	}
	if err := s.personalBackupAllowed(subject(ctx), project); err != nil {
		return nil, err
	}
	if kind == "" || kind == "github" {
		service, err := s.gitHubBackups()
		if err != nil {
			return nil, err
		}
		return integrationActivity(s, subject(ctx), "backup."+action, project, func() (backups.Binding, error) {
			return service.manager.Control(ctx, project, generation, action)
		})
	}
	service, err := s.driveBackups(kind)
	if err != nil {
		return nil, err
	}
	d, err := integrationActivity(s, subject(ctx), "backup."+kind+"."+action, project, func() (backups.Drive, error) {
		return service.manager.DriveControl(ctx, kind, project, generation, action)
	})
	if err != nil {
		return nil, err
	}
	return s.driveView(d, ""), nil
}
