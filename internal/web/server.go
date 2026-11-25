package web

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"errors"
	"forgetmenote/internal/quicknotes"
	"forgetmenote/internal/reposource"
	"html/template"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"forgetmenote/internal/actions"
	"forgetmenote/internal/agentmemory"
	"forgetmenote/internal/attribution"
	"forgetmenote/internal/auth"
	"forgetmenote/internal/automation"
	"forgetmenote/internal/backups"
	"forgetmenote/internal/blob"
	"forgetmenote/internal/branding"
	"forgetmenote/internal/capacity"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/citationchecks"
	"forgetmenote/internal/connectors"
	"forgetmenote/internal/dataops"
	"forgetmenote/internal/decision"
	"forgetmenote/internal/knowledge"
	"forgetmenote/internal/messaging"
	"forgetmenote/internal/notebooks"
	"forgetmenote/internal/outgoing"
	"forgetmenote/internal/pg"
	"forgetmenote/internal/purgehttp"
	"forgetmenote/internal/search"
	"forgetmenote/internal/sourcehttp"
	"forgetmenote/internal/sources"
	"forgetmenote/internal/taskqueue"
	"forgetmenote/internal/webcore"
	"forgetmenote/internal/workspace"
)

// The browser loads the bundles in ui/dist by their manifest aliases; the font
// directory contributes only its license texts, since dist holds the faces.
//
//go:embed ui/*.html ui/page/*.html ui/fonts/*.txt ui/fonts/*.md ui/dist ui/views
var assets embed.FS

type Server struct {
	browserAssets   browserManifest
	customIcons     appIconCache
	lifecycleMu     sync.Mutex
	started, closed bool
	closeOnce       sync.Once
	// Same credential-free source boundary as document capture; replaceable by
	// local HTTP fixtures without weakening production DNS/redirect checks.
	scriptImageFetch     func(context.Context, string) ([]byte, string, string, error)
	scriptWebsiteCapture func(context.Context, string) (chat.WebsiteEvidence, error)
	personalConnections  *connectors.PersonalConnections
	dataConnections      *connectors.Manager
	dataOperations       *dataops.Service
	automations          *automationService
	python               *automation.Python
	slack                *slackService
	tasks                *taskqueue.Service
	// messageClients are the configured messaging transports, by client ID.
	messageClients map[string]messaging.Transport
	headless       bool
	outgoing       *outgoing.Mode
	authority      *notebooks.Authority

	repositoryConnections *repositoryConnections

	links               *notebooks.Links
	repositories        *reposource.Manager
	backups             *backupService
	quicknotes          *quicknotes.Service
	citations           *citationchecks.Runtime
	jevState            *jevState
	decisions           *decision.Policy
	chat                *chatService
	actions             actions.Registry
	oauthRevocations    passwordRequests
	oauthRequests       passwordRequests
	oauthGrants         passwordRequests
	readinessMu         sync.Mutex
	readinessAt         time.Time
	readinessOK         bool
	exports             exportCache
	revisions           *revisionHub
	legacy              *legacySessions
	mcpTools            []func()
	indexes             *search.Indexes
	memory              *agentmemory.Service
	corpusSearch        *search.Coordinator
	aiMu                sync.Mutex
	catalog             *catalog.Catalog
	proxy               *auth.Proxy
	data, origin, token string
	mux                 *http.ServeMux
	routes              *webcore.NotebookRoutes
	page                *template.Template
	renderer            *webcore.Renderer
	oauth               *oauthState
	db                  *pg.DB
	leave               func()
	github              *githubConnection
	oidc                *oidcLogin
	passwords           passwordRequests
	sourceJobs          *sources.Jobs
	sourceHTTP          *sourcehttp.HTTP
	purge               *purgehttp.Service
	sourceWork          chan struct{}
	rpcWork             chan struct{}
	rpcReadWork         chan struct{}
	rpcDecodeWork       chan struct{}
	rpcStreams          chan struct{}
	rpcHTTP             rpcHTTPRegistry
	// blobs keeps source job files, repository source generations and
	// export bundles; closeBlobs releases a store NewWithBlobs opened.
	blobs      blob.Store
	closeBlobs func() error

	// backupDownloads admits browser backups of at most backupBytes each.
	backupDownloads chan struct{}
	backupBytes     int64
	parsedOrigin    atomic.Pointer[parsedOrigin]
}

// Optional services are configured at startup. A request that needs one that
// is not configured fails with its ErrUnavailable (503).

// repos is the repository source manager.
func (s *Server) repos() (*reposource.Manager, error) {
	if s.repositories == nil {
		return nil, reposource.ErrUnavailable
	}
	return s.repositories, nil
}

// repoApp is the GitHub App that repository source connections authorize with.
func (s *Server) repoApp() (*reposource.GitHub, error) {
	if s.repositoryConnections == nil {
		return nil, reposource.ErrUnavailable
	}
	return s.repositoryConnections.github, nil
}

// managedBackups is the managed backup service.
func (s *Server) managedBackups() (*backupService, error) {
	if s.backups == nil {
		return nil, backups.ErrUnavailable
	}
	return s.backups, nil
}

// ConfigureHeadless is a startup setting; it does not change content ownership.
func (s *Server) ConfigureHeadless(enabled bool) error {
	unlock, err := s.startupConfiguration()
	if err != nil {
		return err
	}
	defer unlock()
	s.headless = enabled
	return nil
}

type noteInput struct {
	RequireCheckpoint   bool   `json:"requireCheckpoint,omitempty"`
	AuthorityGeneration uint64 `json:"authorityGeneration,omitempty"`
	Project             string `json:"project"`
	Path                string `json:"path"`
	Content             string `json:"content"`
	Revision            string `json:"revision"`
}
type noteOutput = webcore.NoteOutput

// New serves one instance. db is its Postgres control plane, which the caller owns
// and closes after Close. Its blobs stay in its data directory.
func New(projects *catalog.Catalog, db *pg.DB, data, origin, token string) (*Server, error) {
	return NewWithBlobs(projects, db, nil, data, origin, token)
}

// NewWithBlobs serves an instance whose blobs are in blobs, which the caller
// owns, or in the data directory when blobs is nil.
func NewWithBlobs(projects *catalog.Catalog, db *pg.DB, blobs blob.Store, data, origin, token string) (*Server, error) {
	if err := validateBrowserAssets(assets); err != nil {
		return nil, err
	}
	manifest, err := loadBrowserManifest(assets)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.RawQuery != "" || u.Path != "" || u.User != nil || u.Fragment != "" {
		return nil, errors.New("PUBLIC_URL must be an HTTP(S) origin without a path")
	}
	if u.Scheme == "http" && u.Hostname() != "localhost" && u.Hostname() != "127.0.0.1" {
		return nil, errors.New("use HTTPS for a remotely accessible PUBLIC_URL")
	}
	if len(token) < 32 {
		return nil, errors.New("admin token must have at least 32 characters")
	}
	page, err := template.New("pages").Funcs(pageFuncs(projects, manifest)).ParseFS(assets, "ui/*.html", "ui/page/*.html")
	if err != nil {
		return nil, err
	}
	s := &Server{outgoing: &outgoing.Mode{}, rpcStreams: make(chan struct{}, 128), sourceWork: make(chan struct{}, capacity.SourceRequests.Limit()), rpcWork: make(chan struct{}, capacity.WriteRequests.Limit()), rpcReadWork: make(chan struct{}, capacity.ReadRequests.Limit()), rpcDecodeWork: make(chan struct{}, capacity.DecodeRequests.Limit()), catalog: projects, data: data, origin: strings.TrimSuffix(origin, "/"), token: token, page: page, mux: http.NewServeMux(), routes: webcore.NewNotebookRoutes(), renderer: webcore.NewRenderer()}
	s.browserAssets = manifest
	s.backupDownloads, s.backupBytes = make(chan struct{}, liveExports), maxExportBytes
	complete := false
	defer func() {
		if !complete {
			s.Close()
		}
	}()
	s.blobs = blobs
	if blobs == nil {
		dir, err := blob.OpenDir(data)
		if err != nil {
			return nil, err
		}
		s.blobs, s.closeBlobs = dir, dir.Close
	}

	s.indexes = search.NewIndexes(db, projects, s.chatEmbeddingPolicy)
	s.links = notebooks.NewLinks(s.origin, s.clientAccess, s.clientProjects, s.acquireEngine)
	s.corpusSearch = search.NewCoordinator(projects, s.indexes, s.toolStore, s.documentContentScope, s.rerankRepositoryResults)
	s.authority = notebooks.NewAuthority(db, projects, s.toolStore, s.checkCredential, func(ctx context.Context, project string, write bool) error {
		if !s.clientProjectAllowed(ctx, project) || write && !s.canWrite(ctx) {
			return workspace.ErrForbidden
		}
		return nil
	})

	// What a request of a stopped process was writing is temporary: a stored
	// export is in Postgres and outlives the process, and a backup download or
	// an import starts again.
	if err := removeLeftovers(data, s.blobs); err != nil {
		return nil, err
	}
	if db == nil {
		return nil, errors.New("ForgetMeNote requires its Postgres database; see docs/postgres.md")
	}
	s.db = db
	// The flow sweep the first export pass begins keeps purge receipts.
	if err = s.initPurge(); err != nil {
		return nil, err
	}
	// Exports another gateway left expire here too.
	s.expireExports()
	// Request windows are shared, so each limit holds for the instance.
	s.passwords.db, s.passwords.group = db, "passwords"
	s.oauthRequests.db, s.oauthRequests.group = db, "oauth-requests"
	s.oauthRevocations.db, s.oauthRevocations.group = db, "oauth-revocations"
	s.oauthGrants.db, s.oauthGrants.group = db, "oauth-grants"
	// Other gateways leave the work this one claims alone while it is present.
	if s.leave, err = db.KeepPresence(); err != nil {
		return nil, err
	}
	s.oauth, err = auth.OpenState(db)
	if err != nil {
		return nil, err
	}
	if err = s.initChat(); err != nil {
		return nil, err
	}
	if err = s.initJev(); err != nil {
		return nil, err
	}
	s.decisions = decision.NewPolicy(projects, s.jevConfigured, s.jevState.preferences)
	if err = s.initMemory(); err != nil {
		return nil, err
	}
	if err = s.initAutomations(); err != nil {
		return nil, err
	}
	if err = s.initTasks(); err != nil {
		return nil, err
	}
	if err = s.initDataOperations(); err != nil {
		return nil, err
	}
	s.jevRoutes()
	if err = s.initQuicknotes(); err != nil {
		return nil, err
	}
	s.quicknotesRoutes()
	s.repositoryRoutes()
	s.backupRoutes()
	s.notebookLinkRoutes()
	s.dataConnectionRoutes()
	s.personalAssistantRoutes()
	s.personalConnectionRoutes()
	s.mux.HandleFunc("GET /readyz", s.ready)
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	s.mux.HandleFunc("GET /login", s.loginPage)
	s.mux.HandleFunc("POST /login", s.login)
	s.mux.HandleFunc("POST /auth/password/login", s.passwordLogin)
	s.mux.HandleFunc("POST /api/local-users", s.createLocalUser)
	s.mux.HandleFunc("POST /account/setup", s.setupAdmin)
	s.mux.HandleFunc("POST /account/password", s.changeLocalPassword)
	s.mux.HandleFunc("POST /logout", s.logout)
	s.mux.HandleFunc("POST /auth/oidc/login", s.startOIDC)
	s.mux.HandleFunc("GET /auth/oidc/callback", s.oidcCallback)
	s.mux.HandleFunc("POST /account/oidc/link", s.startOIDC)
	s.mux.HandleFunc("POST /account/sessions/revoke", s.revokeSessions)
	s.mux.Handle("GET /static/", browserAssetHandler(assets, manifest))
	s.mux.Handle("GET /static/brand/", http.StripPrefix("/static/brand/", http.FileServer(http.FS(branding.Files()))))
	s.mux.HandleFunc("GET /static/brand/instance.png", s.instanceIcon)
	s.mux.HandleFunc("GET /favicon.ico", s.instanceIcon)
	s.mux.HandleFunc("GET /manifest.webmanifest", s.webManifest)
	for _, icon := range appIcons {
		s.mux.HandleFunc("GET /static/brand/"+icon.custom, s.appIcon(icon.size, icon.maskable, icon.file, icon.custom))
	}
	s.mux.HandleFunc("GET /{$}", s.home)
	s.mux.HandleFunc("GET /custom.css", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css")
		path := filepath.Join(s.data, "custom.css")
		if _, err := os.Stat(path); err == nil {
			http.ServeFile(w, r, path)
		}
	})
	s.handleProject("POST /api/move", "owner", s.move)
	s.handleProject("POST /api/duplicate", "owner", s.duplicate)
	// Legacy unsupported GET/HEAD requests to these owner operations fail authorization
	// before returning 404, even though neither endpoint has a read handler.
	s.routes.RejectPath("/api/move", webcore.MethodRoles{Read: "owner", Write: "owner"})
	s.routes.RejectPath("/api/duplicate", webcore.MethodRoles{Read: "owner", Write: "owner"})
	s.handleProject("POST /api/metadata", "edit", s.setMetadata)
	s.handleProject("POST /api/container", "edit", s.setContainer)
	s.handleProject("POST /api/folder-container", "edit", s.setFolderContainer)
	s.handleProject("GET /api/collections", "read", s.collections)
	s.handleProject("GET /api/locks", "read", s.locks)
	s.handleProject("POST /api/locks", "owner", s.setLock)
	s.mux.HandleFunc("GET /api/me", s.me)
	s.mux.HandleFunc("GET /account", s.account)
	s.mux.HandleFunc("GET /account/connect", s.connectAI)
	s.mux.HandleFunc("GET /account/connect/download/{client}", s.downloadCompanion)
	s.mux.HandleFunc("POST /account/github/connect", s.connectGitHub)
	s.mux.HandleFunc("GET /account/github/callback", s.githubCallback)
	s.mux.HandleFunc("POST /account/github/attribution", s.githubAttribution)
	s.mux.HandleFunc("POST /account/github/disconnect", s.disconnectGitHub)
	s.mux.HandleFunc("GET /api/projects", s.projects)
	s.mux.HandleFunc("POST /api/projects", s.createProject)
	s.mux.HandleFunc("POST /api/projects/import", s.importProject)
	s.mux.HandleFunc("GET /api/admin", s.adminPage)
	s.mux.HandleFunc("GET /api/people", s.findPeople)
	s.mux.HandleFunc("POST /api/users", s.setUser)
	s.mux.HandleFunc("POST /api/members", s.setMember)
	s.commentRoutes()
	s.handleProject("GET /api/notes", "read", s.list)
	s.handleProject("GET /api/browse", "read", s.browse)
	notebookJSON(s, "GET /api/note", "read", s.readNote)
	s.liveRoutes()
	notebookJSON(s, "POST /api/note", "edit", s.saveNote)
	s.handleProject("GET /api/history", "read", s.history)
	s.handleProject("POST /api/preview", "read", s.preview)
	s.handleProject("GET /api/status", "read", s.status)
	s.handleProject("POST /api/upload", "edit", s.upload)
	s.handleProject("GET /assets/{path...}", "read", s.asset)
	s.handleProject("GET /api/view", "read", s.view)
	s.handleProject("POST /api/view/preview", "read", s.viewPreview)
	s.handleProject("GET /api/backup", "read", s.backup)
	if err = s.initSourceJobs(); err != nil {
		return nil, err
	}
	s.sourceHTTP.Register(s.handleProject)
	s.knowledgeRoutes()
	s.wikiRoutes()
	s.trashRoutes()
	s.chatRoutes()
	s.connectionRoutes()
	s.authorityRoutes()
	s.purge.Register(s.handleProject)
	s.instanceAdministrationRoutes()

	s.mux.Handle("/mcp", s.mcpHandler())
	complete = true
	return s, nil
}
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r = notebookVocabulary(r)
	target := parseRequestPath(r.URL.Path)
	p := target.path

	// Normalize API rejections while preserving HTML, OAuth/MCP contracts,
	// existing JSON envelopes and streaming success responses. The route
	// decides: a notebook asset in an api directory is not an API call.
	if strings.HasPrefix(target.route, "/api/") {
		errors := webcore.JSONErrors(w, target.api || p == apiPrefix)
		defer errors.Finish()
		w = errors
	}
	tracked := &outcomeWriter{}
	if p != "/healthz" && p != "/readyz" && !target.static {
		tracked.ResponseWriter = w
		start := time.Now()
		// Admission derives new requests; the route belongs to the one served.
		defer func() { logOutcome(start, r, tracked) }()
		w = tracked
	}
	s.setSecurityHeaders(w.Header())
	if p != "/healthz" && r.Host != s.originHost() {
		http.Error(w, "Unexpected host", 403)
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" && origin != s.origin {
		http.Error(w, "Cross-origin request rejected", 403)
		return
	}
	if s.headless && s.serveHeadless(w, r, target) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, requestBodyLimit(target))
	tracked.route = directRoute(r)
	if s.serveIntegration(w, r, p) || s.oauth != nil && s.oauthRoute(w, r) {
		return
	}
	if !publicPaths[p] && !target.static && !s.signedIn(w, r, target) {
		return
	}
	if p != "/logout" && subject(r.Context()) != "" {
		s.migrateSessionCookie(w, r)
	}
	if r.Method != "GET" && r.Method != "HEAD" && !target.rpc && !s.adminBearer(r) && r.Header.Get("Origin") != s.origin {
		http.Error(w, "Same-origin request required", 403)
		return
	}
	if !s.attribute(w, r, target) {
		return
	}
	// Discard client-supplied or legacy-session lifetime identifiers.
	r.Header.Del(rpcLifetimeHeader)
	if target.rpc {
		var done func()
		var admitted bool
		if r, done, admitted = s.admitRPC(w, r, target); !admitted {
			return
		}
		defer done()
	}
	if target.api {
		r = r.WithContext(context.WithValue(r.Context(), apiContextKey{}, true))
		s.apiHTTP(w, r)
		return
	}
	if !s.selectProject(w, r, target) {
		return
	}
	if projectStore(r.Context()) != nil {
		s.routes.Mux.ServeHTTP(w, r)
	} else {
		s.mux.ServeHTTP(w, r)
	}
}

// notebookVocabulary serves the notebook vocabulary through the project
// routes: /api/notebooks… is /api/projects… and /notebooks/… is /p/…, so
// both enter the same authentication, admission and route owner.
func notebookVocabulary(r *http.Request) *http.Request {
	path := r.URL.Path
	switch {
	case path == "/api/notebooks" || strings.HasPrefix(path, "/api/notebooks/"):
		path = "/api/projects" + strings.TrimPrefix(path, "/api/notebooks")
	case strings.HasPrefix(path, "/notebooks/"):
		path = "/p/" + strings.TrimPrefix(path, "/notebooks/")
	default:
		return r
	}
	r = r.Clone(r.Context())
	r.URL.Path = path
	r.URL.RawPath = ""
	return r
}

// requestPath is what ServeHTTP decides from a request's path, decided once.
type requestPath struct {
	path string
	// A notebook request /p/<notebook>/<route> is scoped. Any other path,
	// including /p/<notebook> without a route, is its own route.
	notebook, route string
	scoped          bool
	// api is the action API (/api/v1/…); rpc is the action API or MCP.
	api, rpc bool
	static   bool
}

func parseRequestPath(path string) requestPath {
	target := requestPath{path: path, route: path, api: strings.HasPrefix(path, apiPrefix+"/"), static: strings.HasPrefix(path, "/static/")}
	target.rpc = target.api || path == "/mcp"
	if rest, ok := strings.CutPrefix(path, "/p/"); ok {
		if notebook, route, ok := strings.Cut(rest, "/"); ok {
			target.notebook, target.route, target.scoped = notebook, "/"+route, true
		}
	}
	return target
}

func (s *Server) setSecurityHeaders(h http.Header) {
	h.Set("X-Content-Type-Options", "nosniff")
	// Keep the Origin header on same-origin form POSTs without leaking
	// private page URLs when navigating to another site.
	h.Set("Referrer-Policy", "same-origin")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; media-src 'self'; frame-src 'self' https://www.youtube-nocookie.com https://player.vimeo.com; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	if strings.HasPrefix(s.origin, "https:") {
		h.Set("Strict-Transport-Security", "max-age=31536000")
	}
}

// serveHeadless answers the browser app's entry points on a headless
// instance: a description at /, and no app assets.
func (s *Server) serveHeadless(w http.ResponseWriter, r *http.Request, target requestPath) bool {
	switch {
	case target.path == "/" && (r.Method == "GET" || r.Method == "HEAD"):
		jsonResponse(w, map[string]any{"service": "ForgetMeNote", "headless": true, "api": "/api/v1", "mcp": "/mcp"})
	case target.path == "/custom.css" || target.path == "/manifest.webmanifest" || target.static && !s.headlessAsset(target.path):
		http.NotFound(w, r)
	default:
		return false
	}
	return true
}

// serveIntegration serves the endpoints that authenticate their own callers
// (Slack and GitHub signatures, backup worker credentials) before any session
// or bearer check.
func (s *Server) serveIntegration(w http.ResponseWriter, r *http.Request, path string) bool {
	switch {
	case path == "/integrations/slack/events":
		s.slackEvents(w, r)
	case path == "/integrations/slack/interactions":
		s.slackInteractions(w, r)
	case path == "/integrations/slack/commands":
		s.slackCommands(w, r)
	case strings.HasPrefix(path, "/internal/backups/v1/"):
		s.backupWorkerHTTP(w, r)
	case path == "/repository/github/webhook":
		s.repositoryWebhook(w, r)
	default:
		return false
	}
	return true
}

// signedIn authenticates a request for a private path: MCP and the action
// API with a bearer grant, the browser with its session. It answers a
// request that is not signed in.
func (s *Server) signedIn(w http.ResponseWriter, r *http.Request, target requestPath) bool {
	p := target.path
	switch {
	case target.rpc && !s.bearer(r):
		metadata := s.origin + "/.well-known/oauth-protected-resource" + strings.TrimPrefix(s.oauthResource(r), s.origin)
		w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+metadata+`"`)
		if target.api {
			apiError(w, 401, "unauthorized", "Authentication required")
		} else {
			http.Error(w, "Authentication required", 401)
		}
	case target.rpc || s.authenticated(r):
		return true
	case p == "/connections/start" || p == "/account/connect" || p == "/account/assistant" || p == "/account/link" && r.Method == "GET":
		http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), 303)
	case p == "/":
		http.Redirect(w, r, "/login", 303)
	default:
		http.Error(w, "Authentication required", 401)
	}
	return false
}

// attribute names the signed-in account and its transport on every change
// the request makes.
func (s *Server) attribute(w http.ResponseWriter, r *http.Request, target requestPath) bool {
	id := subject(r.Context())
	if id == "" {
		return true
	}
	account, err := s.catalog.Account(id)
	if err != nil {
		fail(w, err)
		return false
	}
	identity := account.Identity()
	if s.catalog.IsService(id) {
		identity.Name = "Automation " + account.ID[:12]
	}
	identity.Source = "browser"
	if target.rpc {
		identity.Source = "mcp"
		if target.api {
			identity.Source = "api"
		}
		identity.Client, _ = r.Context().Value(clientKey{}).(string)
	}
	*r = *r.WithContext(attribution.With(r.Context(), identity))
	return true
}

// admitRPC admits an MCP or action API request: a POST takes a decoding
// permit and an MCP GET a stream slot. done ends the request's lifetime.
func (s *Server) admitRPC(w http.ResponseWriter, r *http.Request, target requestPath) (admitted *http.Request, done func(), ok bool) {
	lifetime := &rpcHTTPLifetime{}
	if r.Method == "POST" {
		select {
		case s.rpcDecodeWork <- struct{}{}:
			lifetime.decoding = &rpcPermit{work: s.rpcDecodeWork, responseDone: true}
		default:
			if target.api {
				apiFail(w, capacity.ErrBusy)
			} else {
				http.Error(w, capacity.ErrBusy.Error(), 429)
			}
			return r, nil, false
		}
	}
	id, finish := s.rpcHTTP.register(lifetime)
	r.Header.Set(rpcLifetimeHeader, id)
	r = r.WithContext(context.WithValue(r.Context(), rpcHTTPKey{}, lifetime))
	if target.api || r.Method != "GET" {
		return r, finish, true
	}
	release, err := lifetime.admit(s.rpcStreams)
	if err != nil {
		http.Error(w, err.Error(), 429)
		finish()
		return r, nil, false
	}
	return r, func() { release(); finish() }, true
}

// browserBodyBytes bounds a browser request that carries no notebook content.
const browserBodyBytes = 1 << 20

// requestBodyLimit bounds a request body before its handler applies its own
// limit. Only routes that carry note text, attachments, bundles or source
// originals, and the integrations that set their own limits, admit more than
// an ordinary browser request.
func requestBodyLimit(target requestPath) int64 {
	// MCP attachment uploads encode the same configured file limit as base64 JSON.
	if target.rpc {
		return encodedUploadRequestLimit()
	}
	route := target.route
	switch {
	case route == "/api/sources/preview" || route == "/api/sources/import" || route == "/api/sources/jobs":
		// Source preview/save carry base64 originals in JSON. Save also carries the
		// extracted Markdown, whose JSON representation can escape each byte sixfold.
		return int64((workspace.MaxSourceBytes()+2)/3*4) + int64(workspace.MaxSourceText())*6 + (1 << 20)
	case contentRoutes[route], strings.HasPrefix(route, "/api/chat/") && strings.HasSuffix(route, "/turns"):
		return max(int64(workspace.MaxFileBytes()), workspace.MaxBundleBytes(), int64(workspace.MaxSourceBytes())) + (1 << 20)
	}
	return browserBodyBytes
}

// contentRoutes carry a note, an attachment, a bundle or the instance logo in
// their body; a chat turn may attach private draft text. Branding and Slack's
// endpoints apply their own limits.
var contentRoutes = map[string]bool{
	"/api/note": true, "/api/preview": true, "/api/live": true, "/api/live/patch": true, "/api/upload": true,
	"/api/view/preview": true, "/api/projects/import": true, "/api/quicknotes": true,
	"/api/knowledge/groups": true, "/api/knowledge/proposals": true, "/api/knowledge/instructions": true, "/api/knowledge/embeddings": true,
	"/api/organization/action/set_instance_branding": true, "/integrations/slack/events": true,
	"/integrations/slack/interactions": true, "/integrations/slack/commands": true,
}

// publicPaths, with /static/, are served before sign-in.
var publicPaths = map[string]bool{
	"/healthz": true, "/readyz": true, "/favicon.ico": true, "/manifest.webmanifest": true, "/login": true,
	"/auth/password/login": true, "/invitation": true, "/auth/invitation/accept": true, "/auth/invitation/inspect": true,
	"/auth/oidc/login": true, "/auth/oidc/callback": true,
}

// originHost is the Host every request must name. New validated the origin;
// the parse is kept until the origin changes, which only tests do.
func (s *Server) originHost() string {
	if parsed := s.parsedOrigin.Load(); parsed != nil && parsed.origin == s.origin {
		return parsed.host
	}
	u, _ := url.Parse(s.origin)
	s.parsedOrigin.Store(&parsedOrigin{origin: s.origin, host: u.Host})
	return u.Host
}

type parsedOrigin struct{ origin, host string }

// directRoute names the requests ServeHTTP dispatches without a ServeMux, so
// their log lines carry a route. Other paths, which callers choose, are never
// named: an unmatched request logs no path.
func directRoute(r *http.Request) string {
	p := r.URL.Path
	switch {
	case r.Method == "GET" && (p == apiPrefix+"/actions" || p == apiPrefix+"/capabilities"):
		return "GET " + p
	case r.Method == "POST" && strings.HasPrefix(p, apiPrefix+"/actions/"):
		return "POST " + apiPrefix + "/actions/{action}"
	case strings.HasPrefix(p, apiPrefix+"/"):
		return apiPrefix + "/"
	case strings.HasPrefix(p, "/internal/backups/v1/"):
		return "/internal/backups/v1/"
	case directRoutes[p]:
		return p
	}
	return ""
}

var directRoutes = map[string]bool{
	"/integrations/slack/events": true, "/integrations/slack/interactions": true, "/integrations/slack/commands": true,
	"/repository/github/webhook": true, "/oauth/register": true, "/oauth/authorize": true, "/oauth/token": true, "/oauth/revoke": true,
	"/.well-known/oauth-protected-resource": true, "/.well-known/oauth-protected-resource/mcp": true,
	"/.well-known/oauth-protected-resource/api/v1": true, "/.well-known/oauth-authorization-server": true,
}

// recoveryKey reports whether value is the recovery key. An empty key, which
// New never accepts, matches nothing.
func (s *Server) recoveryKey(value string) bool {
	return s.token != "" && subtle.ConstantTimeCompare([]byte(value), []byte(s.token)) == 1
}
func (s *Server) adminBearer(r *http.Request) bool {
	value, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && s.recoveryKey(value)
}
func (s *Server) bearer(r *http.Request) bool {
	if s.adminBearer(r) {
		setSubject(r, catalog.Bootstrap)
		return true
	}
	return s.oauth != nil && (s.credentialToken(r) || s.validAccessToken(r))
}
func (s *Server) authenticated(r *http.Request) bool {
	if s.adminBearer(r) {
		setSubject(r, catalog.Bootstrap)
		return true
	}
	if user := s.proxyIdentity(r); user != "" {
		setSubject(r, user)
		return s.catalog.Interactive(user)
	}
	token, ok := sessionToken(r)
	if !ok {
		return false
	}
	if s.browserSession(r, token) {
		return true
	}
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return false
	}
	expiry, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || expiry < time.Now().Unix() {
		return false
	}
	valid := s.catalog.ValidSession(catalog.Bootstrap, 0) && hmac.Equal([]byte(parts[1]), []byte(s.sign(parts[0])))
	if valid {
		setSubject(r, catalog.Bootstrap)
	}
	return valid
}
func (s *Server) sign(value string) string {
	h := hmac.New(sha256.New, []byte(s.token))
	h.Write([]byte(value))
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}
func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	s.allowOIDCForm(w, r)
	s.renderPage(w, "login.html", struct {
		Next  string
		OIDC  bool
		Local bool
		Proxy bool
	}{safeLoginNext(r.URL.Query().Get("next")), s.oidc != nil, s.localPasswords(), s.proxy != nil})
}

// renderPage writes a page only when it rendered completely.
func (s *Server) renderPage(w http.ResponseWriter, name string, data any) {
	var page bytes.Buffer
	if err := s.page.ExecuteTemplate(&page, name, data); err != nil {
		fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(page.Bytes())
}
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form", 400)
		return
	}
	// An operator-chosen key may be guessable; failed guesses share the password
	// attempt budget, and a correct key returns its charge.
	done, ok := s.passwordRequest(w, r, catalog.Bootstrap)
	if !ok {
		return
	}
	defer done()
	if !s.recoveryKey(r.Form.Get("token")) {
		http.Error(w, "Invalid access key", 401)
		return
	}
	s.refundPasswordRequest(r, catalog.Bootstrap)
	if err := s.createSession(w, catalog.Bootstrap); err != nil {
		fail(w, err)
		return
	}
	next := safeLoginNext(r.Form.Get("next"))
	http.Redirect(w, r, next, 303)
}
func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	s.allowOIDCForm(w, r)
	// CodeMirror mounts its own stylesheet; authorize only this page's styles.
	styleNonce := RandomToken()
	w.Header().Set("Content-Security-Policy", strings.Replace(w.Header().Get("Content-Security-Policy"), "style-src 'self'", "style-src 'self' 'nonce-"+styleNonce+"'", 1))
	entries := []workspace.Entry{}
	s.renderPage(w, "index.html", struct {
		Entries    []workspace.Entry
		Origin     string
		StyleNonce string
	}{entries, s.origin, styleNonce})
}
func jsonResponse(w http.ResponseWriter, value any)       { webcore.JSON(w, value) }
func fail(w http.ResponseWriter, err error)               { webcore.Fail(w, err) }
func respond(w http.ResponseWriter, value any, err error) { webcore.Respond(w, value, err) }
func decode(w http.ResponseWriter, r *http.Request, value any) bool {
	return webcore.Decode(w, r, value)
}
func (s *Server) render(content string) string                  { return s.renderer.Render(content) }
func (s *Server) output(document workspace.Document) noteOutput { return s.renderer.Output(document) }

type readNoteInput struct {
	projectArg
	Path     string `json:"path"`
	Revision string `json:"revision,omitempty"`
}

func (in noteInput) projectName() string { return in.Project }
func (in *noteInput) setNotebook(id string) error {
	target := projectArg{in.Project}
	err := target.setNotebook(id)
	in.Project = target.Project
	return err
}

// Notes are Markdown; attachments have their own read and upload operations.
// Every transport authorizes the caller before these checks.
func (s *Server) readNote(ctx context.Context, store *workspace.Store, in readNoteInput) (noteOutput, error) {
	if !strings.HasSuffix(in.Path, ".md") {
		return noteOutput{}, webcore.Invalid("notes are Markdown (.md) files; read media as an attachment")
	}
	v, err := purgehttp.PinnedRead(ctx, store, in.Path, in.Revision, func(revision string) (workspace.Document, error) {
		return readChatNote(ctx, store, in.Path, revision)
	})
	return s.output(v), err
}
func (s *Server) saveNote(ctx context.Context, store *workspace.Store, in noteInput) (noteOutput, error) {
	if !strings.HasSuffix(in.Path, ".md") {
		return noteOutput{}, webcore.Invalid("notes must be Markdown (.md) files")
	}
	// A save prepared under another authority must not commit.
	if in.AuthorityGeneration != 0 && s.catalog.Authority(in.Project).Generation != in.AuthorityGeneration {
		return noteOutput{}, workspace.ErrConflict
	}
	if in.RequireCheckpoint {
		ctx = workspace.WithExplicitCheckpoint(ctx)
	}
	v, err := store.Save(ctx, in.Path, []byte(in.Content), in.Revision, subject(ctx))
	return s.output(v), err
}
func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	values := r.URL.Query()
	query := strings.ToLower(strings.TrimSpace(values.Get("q")))
	searching := values.Get("includeTrash") == "true" || query != "" || values.Get("tag") != "" || values.Get("status") != "" || values.Get("path") != "" || values.Has("archives") || values.Has("drafts")
	if !searching {
		entries, err := projectStore(r.Context()).ListCollection(r.Context(), values.Get("collection"))
		respond(w, entries, err)
		return
	}
	result, err := withEngine(s, r.Context(), projectStore(r.Context()), func(e *knowledge.Engine) (workspace.ScopedEntries, error) {
		return e.Find(r.Context(), query, values.Get("tag"), values.Get("status"), values.Get("path"), values.Get("includeTrash") == "true", 100, knowledge.ContainerSelection{Archives: values.Get("archives"), Drafts: values.Get("drafts"), Collection: values.Get("collection")})
	})
	if err != nil {
		fail(w, err)
		return
	}
	w.Header().Set("X-Content-Scope", result.ScopeID)
	jsonResponse(w, result.Entries)
}
func (s *Server) history(w http.ResponseWriter, r *http.Request) {
	v, err := projectStore(r.Context()).History(r.Context(), r.URL.Query().Get("path"))
	if err != nil {
		fail(w, err)
		return
	}
	out, err := s.presentHistory(r.Context(), projectID(r.Context()), v, nil)
	respond(w, out, err)
}
func (s *Server) preview(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Content string `json:"content"`
	}
	if !decode(w, r, &in) {
		return
	}
	jsonResponse(w, map[string]string{"html": s.render(in.Content)})
}
func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	v, err := projectStore(r.Context()).Status(r.Context())
	respond(w, v, err)
}

var mediaTypes = map[string]string{".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".webp": "image/webp", ".avif": "image/avif", ".gif": "image/gif", ".pdf": "application/pdf", ".mp3": "audio/mpeg", ".wav": "audio/wav", ".ogg": "audio/ogg", ".mp4": "video/mp4", ".webm": "video/webm", ".svg": "image/svg+xml"}

func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		if tooLarge := new(http.MaxBytesError); errors.As(err, &tooLarge) {
			fail(w, err)
		} else {
			http.Error(w, "Upload is invalid", 400)
		}
		return
	}
	defer r.MultipartForm.RemoveAll()
	f, h, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "File required", 400)
		return
	}
	defer f.Close()
	ext := strings.ToLower(filepath.Ext(h.Filename))
	if attachmentType("assets/"+h.Filename) == "" {
		http.Error(w, "Unsupported media type", 400)
		return
	}
	content, err := io.ReadAll(io.LimitReader(f, int64(workspace.MaxFileBytes())+1))
	if err != nil {
		fail(w, err)
		return
	}
	path := r.FormValue("path")
	if path == "" {
		path = "assets/" + h.Filename
	}
	if strings.ToLower(filepath.Ext(path)) != ext || !strings.HasPrefix(path, "assets/") {
		http.Error(w, "Upload path must be in assets/ with the original extension", 400)
		return
	}
	doc, err := saveAttachment(r.Context(), projectStore(r.Context()), path, content, r.FormValue("revision"), subject(r.Context()))
	respond(w, map[string]string{"path": doc.Path, "revision": doc.Revision}, err)
}
func (s *Server) asset(w http.ResponseWriter, r *http.Request) {
	path := r.PathValue("path")
	store := projectStore(r.Context())
	doc, err := purgehttp.PinnedRead(r.Context(), store, path, r.URL.Query().Get("revision"), func(revision string) (workspace.Document, error) {
		return store.Read(r.Context(), path, revision)
	})
	if err != nil {
		fail(w, err)
		return
	}
	typ := attachmentType(path)
	if typ == "" {
		typ = "application/octet-stream"
	}
	w.Header().Set("Content-Type", typ)
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	if strings.HasPrefix(path, "assets/sources/") || typ == "image/svg+xml" || !strings.HasPrefix(typ, "image/") && !strings.HasPrefix(typ, "audio/") && !strings.HasPrefix(typ, "video/") {
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filepath.Base(path)}))
	}
	http.ServeContent(w, r, filepath.Base(path), time.Time{}, bytes.NewReader(doc.Content))
}
func (s *Server) backup(w http.ResponseWriter, r *http.Request) {
	release, err := s.admitBackup()
	if err != nil {
		w.Header().Set("Retry-After", "5")
		fail(w, err)
		return
	}
	defer release()
	bundle, err := s.createBackup(r.Context(), projectStore(r.Context()))
	if err != nil {
		fail(w, err)
		return
	}
	defer os.Remove(bundle.Name())
	defer bundle.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="forgetmenote.bundle"`)
	http.ServeContent(w, r, "forgetmenote.bundle", time.Time{}, bundle)
}
func RandomToken() string { return auth.Token() }
