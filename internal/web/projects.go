package web

import (
	"context"
	"forgetmenote/internal/auth"
	"net/http"
	"strings"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/webcore"
	"forgetmenote/internal/workspace"
)

type identityKey = auth.IdentityKey
type projectKey = webcore.NotebookStoreKey
type projectIDKey = webcore.NotebookIDKey

func projectID(ctx context.Context) string { return webcore.NotebookID(ctx) }

func setSubject(r *http.Request, id string) {
	*r = *r.WithContext(context.WithValue(r.Context(), identityKey{}, id))
}
func subject(ctx context.Context) string                { return auth.Subject(ctx) }
func projectStore(ctx context.Context) *workspace.Store { return webcore.NotebookStore(ctx) }

// ConfigureProxy enables trusted-header SSO only when both the socket peer and a server-held secret match.
func (s *Server) ConfigureProxy(secret string, cidrs []string) error {
	unlock, err := s.startupConfiguration()
	if err != nil {
		return err
	}
	defer unlock()

	proxy, err := auth.NewProxy(secret, cidrs)
	if err == nil {
		s.proxy = proxy
	}
	return err
}
func (s *Server) proxyIdentity(r *http.Request) string {
	secret := ""
	for _, value := range r.Header.Values("X-ForgetMeNote-Proxy-Secret") {
		if value == "" || secret != "" && secret != value {
			return ""
		}
		secret = value
	}
	return s.proxy.Identity(r.RemoteAddr, secret, r.Header.Get("Remote-User"))
}

// handleProject keeps authority beside its handler; method matching remains Go's
// ServeMux responsibility. Missing metadata never falls back to a permissive role.
func (s *Server) handleProject(pattern, role string, handler http.HandlerFunc) {
	s.routes.Register(pattern, role, handler)
}

// selectProject binds a notebook request to its notebook once the caller
// holds the role its route requires, and serves the request its route.
func (s *Server) selectProject(w http.ResponseWriter, r *http.Request, target requestPath) bool {
	if !target.scoped {
		if strings.HasPrefix(target.path, "/p/") {
			http.NotFound(w, r)
			return false
		}
		if target.path == "/api/chat/connections" || target.path == "/api/jev/settings" {
			return true
		}
		if _, pattern := s.routes.Mux.Handler(r); pattern != "" {
			http.Error(w, "Project required", 400)
			return false
		}
		return true
	}
	// The request is served its route whether or not it is admitted; no
	// response or log line names the path.
	r.URL.Path = target.route
	pattern, required, err := s.routes.Match(r)
	if err != nil {
		fail(w, err)
		return false
	}
	generation := s.catalog.Authority(target.notebook).Generation
	store, err := s.catalog.Access(subject(r.Context()), target.notebook, required)
	if err != nil {
		fail(w, err)
		return false
	}
	if pattern == "" {
		http.NotFound(w, r)
		return false
	}
	*r = *r.WithContext(workspace.WithAuthority(context.WithValue(context.WithValue(r.Context(), projectIDKey{}, target.notebook), projectKey{}, store), generation))
	return true
}
func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	name, err := s.catalog.SubjectName(subject(r.Context()))
	if err != nil {
		fail(w, err)
		return
	}
	jsonResponse(w, map[string]any{"id": subject(r.Context()), "displayName": name, "username": s.catalog.LocalUsername(subject(r.Context())), "admin": s.catalog.IsAdmin(subject(r.Context())), "canCreateProjects": s.catalog.CanCreateProjects(subject(r.Context())), "sso": s.proxy != nil, "passwordAvailable": s.localPasswords(), "setupAvailable": s.setupAvailable(r)})
}
func (s *Server) projects(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, s.catalog.List(subject(r.Context())))
}
func (s *Server) createProject(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Kind string `json:"kind,omitempty"`
	}
	if !decode(w, r, &in) {
		return
	}
	respond(w, map[string]bool{"ok": true}, s.catalog.CreateKind(subject(r.Context()), in.ID, in.Name, in.Kind))
}
func (s *Server) adminPage(w http.ResponseWriter, r *http.Request) {
	result, err := s.administration(subject(r.Context()))
	respond(w, result, err)
}
func (s *Server) administration(actor string) (map[string]any, error) {
	users, projects, err := s.catalog.Administration(actor)
	if err != nil {
		return nil, err
	}
	names := map[string]string{}
	for _, u := range users {
		if name := s.catalog.LocalUsername(u.ID); name != "" {
			names[u.ID] = name
		}
	}
	result := map[string]any{"users": users, "projects": projects, "usernames": names}
	if s.catalog.IsAdmin(actor) {
		result["instance"] = s.InstanceSummary()
	}
	return result, nil
}

func (s *Server) setUser(w http.ResponseWriter, r *http.Request) {
	var in catalog.UserUpdate
	if !decode(w, r, &in) {
		return
	}
	respond(w, map[string]bool{"ok": true}, s.updateUser(subject(r.Context()), in))
}
func (s *Server) setMember(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Project string `json:"project"`
		User    string `json:"user"`
		Role    string `json:"role"`
	}
	if !decode(w, r, &in) {
		return
	}
	respond(w, map[string]bool{"ok": true}, s.catalog.Member(subject(r.Context()), in.Project, in.User, in.Role))
}
func (s *Server) toolStore(ctx context.Context, project, role string) (*workspace.Store, error) {
	if role == "read" && !s.hasScope(ctx, "notes:read") {
		return nil, workspace.ErrForbidden
	}
	if role != "read" && !s.canWrite(ctx) {
		return nil, workspace.ErrForbidden
	}
	return s.clientAccess(ctx, subject(ctx), project, role)
}

func (s *Server) findPeople(w http.ResponseWriter, r *http.Request) {
	people, err := s.catalog.FindPeople(subject(r.Context()), r.URL.Query().Get("project"), r.URL.Query().Get("q"))
	respond(w, people, err)
}
