package web

import (
	"context"
	"encoding/json"
	"errors"
	"forgetmenote/internal/actions"
	"forgetmenote/internal/auth"
	"forgetmenote/internal/automation"
	"net/http"
	"strings"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/pg"
	"forgetmenote/internal/workspace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type CredentialSpec = auth.CredentialSpec
type APICredential = auth.APICredential
type issuedCredential = auth.IssuedCredential
type credentialContext = auth.CredentialContext
type credentialContextKey = auth.CredentialKey

// IssueCredential also serves the operator command, which needs no stopped
// instance: the database serializes it with the running gateways.
func IssueCredential(projects *catalog.Catalog, db *pg.DB, actor string, spec CredentialSpec) (issuedCredential, error) {
	state, err := auth.OpenState(db)
	if err != nil {
		return issuedCredential{}, err
	}
	return auth.IssueCredentialFor(context.Background(), projects, state, actor, actor, spec)
}
func issueCredential(projects *catalog.Catalog, state *oauthState, actor string, spec CredentialSpec) (issuedCredential, error) {
	return auth.IssueCredentialFor(context.Background(), projects, state, actor, actor, spec)
}
func issueCredentialFor(projects *catalog.Catalog, state *oauthState, issuer, actor string, spec CredentialSpec) (issuedCredential, error) {
	return auth.IssueCredentialFor(context.Background(), projects, state, issuer, actor, spec)
}

// Action issue is bounded by the calling OAuth authorization's scopes and lifetime.
func (s *Server) issueCallerCredential(ctx context.Context, actor string, spec CredentialSpec) (issuedCredential, error) {
	return auth.IssueCredentialFor(ctx, s.catalog, s.oauth, subject(ctx), actor, spec)
}
func (s *Server) credentialToken(r *http.Request) bool {
	raw := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !strings.HasPrefix(raw, "fmn_") {
		return false
	}
	audience := "mcp"
	if r.URL != nil && strings.HasPrefix(r.URL.Path, apiPrefix+"/") {
		audience = "api"
	}
	credential, ok := s.oauth.CredentialToken(s.catalog, raw, audience)
	if !ok {
		return false
	}
	g := credential.Grant
	setSubject(r, g.Subject)
	ctx := context.WithValue(r.Context(), credentialContextKey{}, credential)
	ctx = context.WithValue(ctx, scopeKey{}, strings.Join(g.Scopes, " "))
	ctx = context.WithValue(ctx, clientKey{}, g.Name)
	*r = *r.WithContext(ctx)
	return true
}
func credentialFrom(ctx context.Context) (credentialContext, bool) { return auth.CredentialFrom(ctx) }

// checkCredential looks the caller's credential up again, as each disclosure
// of results does.
func (s *Server) checkCredential(ctx context.Context) error {
	if err := s.checkAutomationAuth(ctx); err != nil {
		return err
	}
	return s.oauth.CheckCredential(ctx, s.catalog)
}

// checkInvocation checks the caller once per invocation. A credential this
// invocation's admission just resolved is not looked up again until a
// disclosure calls checkCredential; one restored from persisted work is.
func (s *Server) checkInvocation(ctx context.Context) error {
	if credential, ok := credentialFrom(ctx); ok && credential.Admitted() {
		return s.checkAutomationAuth(ctx)
	}
	return s.checkCredential(ctx)
}

// clientRestrictionsAllow applies the invocation's automation and credential
// notebook restrictions, which checkInvocation does not look up.
func clientRestrictionsAllow(ctx context.Context, project string) bool {
	if a, ok := automation.IdentityFrom(ctx); ok && !a.Reads(project) {
		return false
	}
	return auth.ProjectAllowed(ctx, project)
}
func (s *Server) clientProjectAllowed(ctx context.Context, project string) bool {
	return clientRestrictionsAllow(ctx, project) && s.checkInvocation(ctx) == nil
}
func (s *Server) clientAccess(ctx context.Context, actor, project, role string) (*workspace.Store, error) {
	if a, ok := automation.IdentityFrom(ctx); ok && !a.Writes(project) && role != "read" {
		return nil, workspace.ErrForbidden
	}
	if !s.clientProjectAllowed(ctx, project) {
		return nil, workspace.ErrForbidden
	}
	return s.catalog.Access(actor, project, role)
}
func (s *Server) clientProjects(ctx context.Context, actor string) []catalog.Project {
	out := []catalog.Project{}
	if s.checkInvocation(ctx) != nil {
		return out
	}
	for _, p := range s.catalog.List(actor) {
		if !clientRestrictionsAllow(ctx, p.ID) {
			continue
		}
		p.Capabilities.Edit = p.Capabilities.Edit && s.canWrite(ctx)
		p.Capabilities.Manage = p.Capabilities.Manage && s.hasScope(ctx, "notes:manage")
		p.Capabilities.Read = p.Capabilities.Read && s.hasScope(ctx, "notes:read")
		p.Capabilities.Refresh = p.Capabilities.Refresh && p.Capabilities.Manage
		p.Capabilities.Backup = p.Capabilities.Backup && p.Capabilities.Manage
		out = append(out, p)
	}
	return out
}

// clientRoleAllowed reports whether the invocation may use p, as clientProjects
// listed it, at role. It applies the role's consent, automation restriction and
// current catalog access without looking the credential up again.
func (s *Server) clientRoleAllowed(ctx context.Context, actor string, p catalog.Project, role string) bool {
	if role == "read" && !s.hasScope(ctx, "notes:read") || role != "read" && !s.canWrite(ctx) {
		return false
	}
	if a, ok := automation.IdentityFrom(ctx); ok && !a.Writes(p.ID) && role != "read" {
		return false
	}
	_, err := s.catalog.Access(actor, p.ID, role)
	return err == nil
}

func clientActionScope(tool *mcp.Tool) string {
	name := actions.LegacyName(tool.Name)
	switch name {
	case "refresh_repository_source", "set_semantic_config", "set_retrieval_settings", "set_project_instructions", "set_notebook_wiki":
		return "notes:manage"
	}
	if strings.HasPrefix(name, "configure_") {
		return "notes:manage"
	}
	if tool.Annotations != nil && tool.Annotations.ReadOnlyHint {
		return "notes:read"
	}
	return "notes:write"
}
func (s *Server) clientActionAvailable(ctx context.Context, name, scope string) bool {
	name = actions.LegacyName(name)
	if !s.hasScope(ctx, scope) || accountUIActions[name] {
		return false
	}
	if _, token := credentialFrom(ctx); token && (name == "create_api_credential" || organizationActions[name] && name != "project_groups" && name != "set_project_group") {
		return false
	}
	if !auth.Global(ctx) {
		switch name {
		case "create_project", "import_project_bundle", "administration", "set_user_access":
			return false
		}
	}
	return true
}
func (s *Server) guardClientAction(ctx context.Context, name, scope string, raw json.RawMessage) error {
	_, token := credentialFrom(ctx)
	if !token && !auth.API(ctx) {
		return nil
	}
	if err := s.checkInvocation(ctx); err != nil {
		return err
	}
	if !s.clientActionAvailable(ctx, name, scope) {
		return workspace.ErrForbidden
	}
	if !token {
		return nil
	}
	var args struct {
		Project  string   `json:"project"`
		Projects []string `json:"projects"`
	}
	if json.Unmarshal(raw, &args) != nil {
		return errors.New("invalid action arguments")
	}
	if args.Project != "" && !clientRestrictionsAllow(ctx, args.Project) {
		return workspace.ErrForbidden
	}
	for _, p := range args.Projects {
		if !clientRestrictionsAllow(ctx, p) {
			return workspace.ErrForbidden
		}
	}
	return nil
}

func (s *Server) credentialTools(server *mcp.Server) {
	addManagementTool(s, server, "create_api_credential", "Create an expiring credential for your account with explicit notebook, permission, and API/MCP audience restrictions. The secret is returned once. Requires personal management consent; API credentials cannot mint other credentials. An OAuth caller can grant only its own scopes, and the credential ends no later than that authorization and is revoked with it.", false, func(ctx context.Context, in CredentialSpec) (issuedCredential, error) {
		if _, ok := credentialFrom(ctx); ok {
			return issuedCredential{}, workspace.ErrForbidden
		}
		return s.issueCallerCredential(ctx, subject(ctx), in)
	})
	addManagementTool(s, server, "list_api_credentials", "List your API credential metadata without secrets.", true, func(ctx context.Context, in struct{}) ([]APICredential, error) {
		id := ""
		if current, token := credentialFrom(ctx); token {
			id = current.Grant.ID
		}
		return s.oauth.CredentialList(subject(ctx), id), nil
	})
	addManagementTool(s, server, "revoke_api_credential", "Revoke one of your API credentials immediately.", false, func(ctx context.Context, in struct {
		ID string `json:"id"`
	}) (actionResult, error) {
		if current, token := credentialFrom(ctx); token && current.Grant.ID != in.ID {
			return actionResult{}, workspace.ErrForbidden
		}
		err := s.revokeCredential(subject(ctx), subject(ctx), in.ID)
		return actionResult{err == nil}, err
	})
}
