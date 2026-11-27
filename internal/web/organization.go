package web

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"

	"forgetmenote/internal/backups"
	"forgetmenote/internal/branding"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/integrations"
	"forgetmenote/internal/workspace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var organizationActions = map[string]bool{
	"set_assistant_responsibilities": true, "assistant_policy": true, "set_assistant_policy": true,
	"provision_work_notebook": true,
	"chat_clients":            true, "set_chat_clients": true,
	"ai_settings": true, "set_ai_settings": true,
	"discover_automation_packages": true, "install_automation_package": true, "prepare_automation_package": true, "save_automation": true, "delete_automation": true, "validate_automation": true, "test_automation": true, "automation_run": true, "automation_status": true, "run_automation": true, "cancel_automation": true,
	"data_connection_access": true, "set_data_connection_access": true, "bind_external_identity": true,
	"data_connection_effects": true, "automation_effects": true, "resolve_automation_effect": true,
	"organization_status": true, "set_instance_name": true, "set_instance_branding": true, "set_group": true, "delete_group": true,
	"project_groups": true, "set_project_group": true, "create_invitation": true, "revoke_invitation": true,
	"create_password_reset": true, "create_service_account": true, "set_service_steward": true,
	"issue_service_credential": true, "revoke_user_credential": true, "offboarding_status": true, "reassign_notebook_ownership": true,
}

func (s *Server) instanceAdministrationRoutes() {
	s.mux.HandleFunc("POST /api/organization/action/{action}", s.organizationHTTP)
	s.mux.HandleFunc("GET /invitation", s.invitationPage)
	s.mux.HandleFunc("POST /auth/invitation/accept", s.acceptInvitation)
	s.mux.HandleFunc("POST /auth/invitation/inspect", s.inspectInvitation)
}
func (s *Server) organizationHTTP(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("action")
	if !organizationActions[name] {
		http.NotFound(w, r)
		return
	}
	a, ok := s.actions[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	limit := int64(64 << 10)
	if name == "set_instance_branding" {
		limit += (branding.MaxLogoBytes + 2) / 3 * 4
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		http.Error(w, "Request too large", 413)
		return
	}
	result, err := a.Call(r.Context(), raw)
	if err != nil {
		fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(result)
}
func (s *Server) organizationTools(server *mcp.Server) {
	addManagementTool(s, server, "provision_work_notebook", "Provision an ordinary private work notebook for an enabled person. Admin only. Specify the person, notebook ID and name; identical repeats are idempotent. Does not grant instance or notebook-creation privileges, repurpose an existing shared notebook, or change Inbox access.", false, func(ctx context.Context, in struct {
		User string `json:"user"`
		ID   string `json:"id"`
		Name string `json:"name"`
	}) (actionResult, error) {
		err := s.catalog.ProvisionWorkNotebook(subject(ctx), in.User, in.ID, in.Name)
		return actionResult{err == nil}, err
	})
	addManagementTool(s, server, "bind_external_identity", "Bind an external issuer/subject to an existing account as instance administrator. Never use an email match as proof of identity.", false, func(ctx context.Context, in struct {
		Issuer  string `json:"issuer"`
		Subject string `json:"subject"`
		User    string `json:"user"`
	}) (actionResult, error) {
		err := s.catalog.BindExternalIdentity(subject(ctx), in.Issuer, in.Subject, in.User)
		return actionResult{err == nil}, err
	})
	s.messagingTools(server)
	s.instanceAITools(server)
	addManagementTool(s, server, "organization_status", "Inspect this instance's groups, invitations and administrative activity. Admin only; no secrets.", true, func(ctx context.Context, in struct{}) (map[string]any, error) {
		org, err := s.catalog.Organization(subject(ctx))
		if err != nil {
			return nil, err
		}
		mode := "local"
		if s.oidc != nil {
			mode = "oidc"
		} else if s.proxy != nil {
			mode = "proxy"
		}
		summary := s.InstanceSummary()
		return map[string]any{"organization": org, "branding": currentBrand(s.catalog), "authentication": mode, "instance": summary, "omittedIntegrations": integrations.Omitted(), "modules": s.modules.States(),
			"headless": summary["headless"], "githubConfigured": summary["repositorySources"], "backupsConfigured": summary["managedBackups"], "chatConfigured": summary["chat"], "embeddingsConfigured": summary["embeddings"]}, nil
	})
	addManagementTool(s, server, "set_instance_name", "Set the instance display name. Admin only.", false, func(ctx context.Context, in struct {
		Name string `json:"name"`
	}) (actionResult, error) {
		err := s.catalog.SetInstanceName(subject(ctx), in.Name)
		return actionResult{err == nil}, err
	})
	addManagementTool(s, server, "set_instance_branding", "Set the instance name and logo. Admin only. Logo is base64 PNG or JPEG, up to 1 MiB and 2048 by 2048 pixels; omit to preserve, or use an empty string to restore the default. Applies to the UI and newly downloaded plugins.", false, func(ctx context.Context, in struct {
		Name string  `json:"name"`
		Logo *string `json:"logo,omitempty"`
	}) (actionResult, error) {
		if !s.catalog.IsAdmin(subject(ctx)) {
			return actionResult{}, workspace.ErrForbidden
		}
		var logo *[]byte
		if in.Logo != nil {
			if len(*in.Logo) > (branding.MaxLogoBytes+2)/3*4 {
				return actionResult{}, fmt.Errorf("%w: logo exceeds 1 MiB", catalog.ErrLocalInput)
			}
			raw, err := base64.StdEncoding.DecodeString(*in.Logo)
			if err != nil {
				return actionResult{}, fmt.Errorf("%w: logo must be base64 PNG or JPEG", catalog.ErrLocalInput)
			}
			logo = &raw
		}
		err := s.catalog.SetInstanceBranding(subject(ctx), in.Name, logo)
		return actionResult{err == nil}, err
	})
	addManagementTool(s, server, "set_group", "Create or update a flat group. Admin only; revision 0 creates, otherwise provide its current revision.", false, func(ctx context.Context, in catalog.Group) (catalog.Group, error) {
		return s.catalog.SetGroup(subject(ctx), in)
	})
	addManagementTool(s, server, "delete_group", "Delete a group and revoke all its notebook grants. Admin only; requires current revision.", false, func(ctx context.Context, in struct {
		ID       string `json:"id"`
		Revision uint64 `json:"revision"`
	}) (actionResult, error) {
		err := s.catalog.DeleteGroup(subject(ctx), in.ID, in.Revision)
		return actionResult{err == nil}, err
	})
	addManagementTool(s, server, "project_groups", "List group names for sharing an owned notebook. Owner/Admin only; omits group member lists.", true, func(ctx context.Context, in projectArg) ([]catalog.Group, error) {
		return s.catalog.ProjectGroups(subject(ctx), in.Project)
	})
	addManagementTool(s, server, "set_project_group", "Grant a group read/comment/edit access, or remove its grant with an empty role. Owner/Admin only.", false, func(ctx context.Context, in struct {
		Project string `json:"project"`
		Group   string `json:"group"`
		Role    string `json:"role"`
	}) (actionResult, error) {
		err := s.catalog.SetProjectGroup(subject(ctx), in.Project, in.Group, in.Role)
		return actionResult{err == nil}, err
	})
	addManagementTool(s, server, "create_invitation", "Create a single-use seven-day invitation. Admin only. Return its private link once; no email is sent.", false, func(ctx context.Context, in catalog.InvitationSpec) (map[string]any, error) {
		v, err := s.catalog.CreateInvitation(subject(ctx), in, "")
		if err != nil {
			return nil, err
		}
		return s.invitationOutput(v), nil
	})
	addManagementTool(s, server, "create_password_reset", "Create a single-use one-hour local password reset link. Admin only; unavailable with external login.", false, func(ctx context.Context, in struct {
		User string `json:"user"`
	}) (map[string]any, error) {
		if !s.localPasswords() {
			return nil, workspace.ErrForbidden
		}
		v, err := s.catalog.CreateInvitation(subject(ctx), catalog.InvitationSpec{}, in.User)
		if err != nil {
			return nil, err
		}
		return s.invitationOutput(v), nil
	})
	addManagementTool(s, server, "revoke_invitation", "Revoke an invitation or password reset link. Admin only.", false, func(ctx context.Context, in struct {
		ID string `json:"id"`
	}) (actionResult, error) {
		err := s.catalog.RevokeInvitation(subject(ctx), in.ID)
		return actionResult{err == nil}, err
	})
	addManagementTool(s, server, "create_service_account", "Create an instance-owned automation identity with a human steward and no initial access. Admin only.", false, func(ctx context.Context, in struct {
		Name    string `json:"name"`
		Steward string `json:"steward"`
	}) (catalog.User, error) {
		return s.catalog.CreateService(subject(ctx), in.Name, in.Steward)
	})
	addManagementTool(s, server, "set_service_steward", "Assign an active human steward to a service account. Admin only; grants no notebook access.", false, func(ctx context.Context, in struct {
		User    string `json:"user"`
		Steward string `json:"steward"`
	}) (actionResult, error) {
		err := s.catalog.SetServiceSteward(subject(ctx), in.User, in.Steward)
		return actionResult{err == nil}, err
	})
	addManagementTool(s, server, "issue_service_credential", "Issue an expiring, explicitly notebook-scoped API/MCP credential for a service account. Admin only; secret returned once.", false, func(ctx context.Context, in struct {
		User string `json:"user"`
		CredentialSpec
	}) (issuedCredential, error) {
		u, err := s.catalog.UserInfo(subject(ctx), in.User)
		if err != nil || u.Kind != "service" || u.Disabled {
			return issuedCredential{}, workspace.ErrForbidden
		}
		return s.issueCallerCredential(ctx, u.ID, in.CredentialSpec)
	})
	addManagementTool(s, server, "revoke_user_credential", "Revoke a selected user's API/MCP API credential. Admin only.", false, func(ctx context.Context, in struct {
		User string `json:"user"`
		ID   string `json:"id"`
	}) (actionResult, error) {
		if _, err := s.catalog.UserInfo(subject(ctx), in.User); err != nil {
			return actionResult{}, err
		}
		err := s.revokeCredential(subject(ctx), in.User, in.ID)
		return actionResult{err == nil}, err
	})
	addManagementTool(s, server, "offboarding_status", "Inspect a user's notebook ownership, groups, credentials, integration authorizations and automation stewardship. Admin only; no secrets or chat contents.", true, func(ctx context.Context, in struct {
		User string `json:"user"`
	}) (map[string]any, error) {
		return s.offboarding(subject(ctx), in.User)
	})
	addManagementTool(s, server, "reassign_notebook_ownership", "Transfer selected shared notebook ownership to an active person. Admin only; excludes personal Inboxes and does not transfer credentials or integrations.", false, func(ctx context.Context, in struct {
		User        string   `json:"user"`
		Replacement string   `json:"replacement"`
		Projects    []string `json:"projects"`
	}) (actionResult, error) {
		err := s.catalog.ReassignOwnership(subject(ctx), in.User, in.Replacement, in.Projects)
		return actionResult{err == nil}, err
	})
}
func (s *Server) invitationOutput(v catalog.IssuedInvitation) map[string]any {
	// The fragment keeps the secret out of HTTP request URLs and access logs.
	return map[string]any{"id": v.ID, "token": v.Token, "url": s.origin + "/invitation#" + v.Token, "expires": v.Expires, "reset": v.Reset}
}
func (s *Server) offboarding(actor, id string) (map[string]any, error) {
	report, err := s.catalog.Offboarding(actor, id)
	if err != nil {
		return nil, err
	}
	credentials := s.oauth.CredentialList(id, "")
	connections := []map[string]any{}
	// An offboarding report that omitted a connection would leave it running,
	// or let it run again once its module is back on; with a module off, the
	// connections stored while it ran are listed.
	repositories := s.storedRepositories()
	var stored map[string]backups.Binding
	if s.backups == nil && !s.modules.Enabled("managed-backups") {
		if stored, err = backups.Stored(context.Background(), s.db); err != nil {
			return nil, err
		}
	}
	for _, p := range s.catalog.List(actor) {
		if repositories != nil {
			state, e := repositories.Status(p.ID)
			if e == nil && state.Actor == id && state.Connected {
				connections = append(connections, map[string]any{"project": p.ID, "kind": "repository", "paused": state.Paused, "reauthorize": report.User.Disabled})
			}
		}
		b, ok := stored[p.ID]
		if s.backups != nil {
			if b, err = s.backups.manager.Status(p.ID); err != nil {
				return nil, err
			}
			ok = true
		}
		if ok && b.Actor == id && b.Connected {
			connections = append(connections, map[string]any{"project": p.ID, "kind": "backup", "paused": !b.Enabled, "reauthorize": report.User.Disabled || s.catalog.Epoch(id) != b.Epoch})
		}
	}
	return map[string]any{"account": report, "credentials": credentials, "connections": connections}, nil
}
func (s *Server) revokeCredential(actor, user, id string) error {
	return s.oauth.RevokeCredential(s.catalog, actor, user, id)
}
func (s *Server) invitationPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Referrer-Policy", "no-referrer")
	s.page.ExecuteTemplate(w, "invitation.html", map[string]bool{"Local": s.localPasswords(), "SignedIn": s.authenticated(r) && subject(r.Context()) != catalog.Bootstrap, "OIDC": s.oidc != nil})
}
func (s *Server) acceptInvitation(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token    string `json:"token"`
		Username string `json:"username"`
		Password string `json:"password"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if !decode(w, r, &in) {
		return
	}
	done, ok := s.passwordRequest(w, r, "invitation:"+digest(in.Token))
	if !ok {
		return
	}
	defer done()
	actor := ""
	if s.authenticated(r) {
		actor = subject(r.Context())
	}
	if in.Username != "" {
		actor = ""
	}
	if actor != "" && actor != catalog.Bootstrap {
		// Public invitation routes bypass the normal authenticated account setup.
		if _, err := s.catalog.Account(actor); err != nil {
			fail(w, err)
			return
		}
	}
	user, err := s.catalog.AcceptInvitation(in.Token, actor, in.Username, in.Password, s.localPasswords())
	respond(w, map[string]string{"id": user}, err)
}

// Re-enabling an account never restarts a repository writer implicitly, even
// one stored while the repositories module is off.
func (s *Server) updateUser(actor string, in catalog.UserUpdate) error {
	old, err := s.catalog.UserInfo(actor, in.ID)
	if err != nil && !s.catalog.IsAdmin(actor) {
		return err
	}
	pause := func() error {
		repositories := s.storedRepositories()
		if repositories == nil {
			return nil
		}
		for _, p := range s.catalog.List(catalog.Bootstrap) {
			state, e := repositories.Status(p.ID)
			if e == nil && state.Connected && state.Actor == in.ID && !state.Paused {
				if _, e = repositories.Control(p.ID, catalog.Bootstrap, "pause"); e != nil {
					return e
				}
			}
		}
		return nil
	}
	if old.Disabled && in.Disabled != nil && !*in.Disabled {
		if err = pause(); err != nil {
			return err
		}
	}
	if err = s.catalog.UpdateUser(actor, in); err != nil {
		return err
	}
	if in.Disabled != nil && *in.Disabled {
		return pause()
	}
	return nil
}

func (s *Server) inspectInvitation(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token string `json:"token"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if !decode(w, r, &in) {
		return
	}
	done, ok := s.passwordRequest(w, r, "invitation:"+digest(in.Token))
	if !ok {
		return
	}
	defer done()
	invite, err := s.catalog.InspectInvitation(in.Token)
	if err != nil {
		fail(w, err)
		return
	}
	signedIn := s.authenticated(r) && subject(r.Context()) != catalog.Bootstrap
	jsonResponse(w, map[string]any{"name": invite.Name, "reset": invite.Reset, "local": s.localPasswords(), "signedIn": signedIn})
}
