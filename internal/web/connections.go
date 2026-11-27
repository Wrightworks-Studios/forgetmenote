package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"

	"forgetmenote/internal/auth"
	"forgetmenote/internal/githubapp"
	"forgetmenote/internal/pg"
	"forgetmenote/internal/reposource"
	"forgetmenote/internal/workspace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// A short-lived bridge binds a terminal request to the same account's browser
// session. Only the existing cookie/PKCE callback can supply its GitHub grant.
type githubSetup struct {
	ID, Purpose, Subject, Project, Name, Binding string
	Creating, Started                            bool
	Epoch                                        uint64
	Expires                                      time.Time
}
type connectionStart struct {
	Project  string `json:"project"`
	Name     string `json:"name,omitempty"`
	Purpose  string `json:"purpose"`
	Creating bool   `json:"creating,omitempty"`
}
type connectionSelection struct {
	Connection         string `json:"connection"`
	Installation       int64  `json:"installation,omitempty"`
	Repository         int64  `json:"repository,omitempty"`
	Generation         uint64 `json:"generation,omitempty"`
	Branch             string `json:"branch,omitempty"`
	ShareWithNotebook  bool   `json:"shareWithNotebook,omitempty"`
	ShareHistory       bool   `json:"shareHistory,omitempty"`
	ExternalProcessing bool   `json:"externalProcessing,omitempty"`
	CreateOwner        string `json:"createOwner,omitempty"`
	CreateName         string `json:"createName,omitempty"`
}

func (s *Server) connectionAuthority(ctx context.Context, p githubSetup) error {
	if p.Subject != subject(ctx) || p.Expires.Before(time.Now()) || !s.catalog.ValidSession(p.Subject, p.Epoch) {
		return workspace.ErrForbidden
	}
	if p.Creating {
		if !auth.Global(ctx) || !s.catalog.CanCreateProjects(p.Subject) {
			return workspace.ErrForbidden
		}
		return nil
	}
	_, err := s.clientAccess(ctx, p.Subject, p.Project, "owner")
	return err
}
func (s *Server) connection(ctx context.Context, id string) (githubSetup, error) {
	p, ok, err := readFlow[githubSetup](s, ctx, "connection/ticket", digest(id))
	if err != nil {
		return p, err
	}
	if !ok {
		return p, workspace.ErrForbidden
	}
	return p, s.connectionAuthority(ctx, p)
}
func (s *Server) connectionGrant(ctx context.Context, p githubSetup) (repositoryGrant, error) {
	if err := s.connectionAuthority(ctx, p); err != nil {
		return repositoryGrant{}, err
	}
	var grant repositoryGrant
	var ok bool
	var err error
	if p.Purpose == "source" && s.repositoryConnections != nil {
		grant, ok, err = s.grant(ctx, "repository/grant", p.Binding, s.repositoryConnections.github.Client)
	}
	if p.Purpose == "backup" && s.backups != nil {
		grant, ok, err = s.grant(ctx, "backup/grant", p.Binding, s.backups.app)
	}
	if err != nil {
		return grant, err
	}
	if !ok || grant.Connection != p.ID || grant.Subject != p.Subject {
		return grant, workspace.ErrForbidden
	}
	if p.Purpose == "backup" {
		return grant, s.backupGrantAuthority(grant)
	}
	return grant, s.repositoryGrantAuthority(grant)
}

// connectionReady hands the ticket the grant its browser callback stored. A
// ticket that has expired meanwhile is left to its terminal to restart.
func (s *Server) connectionReady(ctx context.Context, grant repositoryGrant) error {
	err := updateFlow(s, ctx, "connection/ticket", digest(grant.Connection), func(p *githubSetup) error {
		p.Binding = grant.Binding
		return nil
	})
	if errors.Is(err, pg.ErrNoFlow) {
		return nil
	}
	return err
}
func (s *Server) connectionRoutes() {
	s.mux.HandleFunc("GET /connections/start", func(w http.ResponseWriter, r *http.Request) {
		p, err := s.connection(r.Context(), r.URL.Query().Get("ticket"))
		if err != nil {
			fail(w, err)
			return
		}
		// A ticket starts one browser authorization, on whichever gateway.
		err = updateFlow(s, r.Context(), "connection/ticket", digest(p.ID), func(current *githubSetup) error {
			if current.Started {
				return workspace.ErrForbidden
			}
			current.Started = true
			return nil
		})
		if errors.Is(err, pg.ErrNoFlow) {
			err = workspace.ErrForbidden
		}
		if err != nil {
			fail(w, err)
			return
		}
		grant := repositoryGrant{Connection: p.ID, Project: p.Project, Creating: p.Creating, Name: p.Name}
		if p.Purpose != "source" {
			if _, err := s.managedBackups(); err != nil {
				fail(w, err)
				return
			}
			s.beginBackupConnection(w, r, grant)
			return
		}
		g, err := s.repoApp()
		if err != nil {
			fail(w, err)
			return
		}
		s.beginRepositoryConnection(w, r, g, grant)
	})
}
func (s *Server) connectionTools(server *mcp.Server) {
	addManagementTool(s, server, "begin_github_connection", "Start a GitHub Sources or Backups App connection. Open the returned URL and authorize as the same account; then select a repository through finish_github_connection.", false, func(ctx context.Context, in connectionStart) (map[string]string, error) {
		if in.Purpose != "source" && in.Purpose != "backup" || in.Creating && in.Purpose != "source" {
			return nil, errors.New("choose source or backup")
		}
		var unavailable error
		if in.Purpose == "source" {
			_, unavailable = s.repoApp()
		} else {
			_, unavailable = s.managedBackups()
		}
		if unavailable != nil {
			return nil, unavailable
		}
		p := githubSetup{ID: RandomToken(), Subject: subject(ctx), Project: in.Project, Name: in.Name, Purpose: in.Purpose, Creating: in.Creating, Epoch: s.catalog.Epoch(subject(ctx)), Expires: time.Now().Add(10 * time.Minute)}
		if err := s.connectionAuthority(ctx, p); err != nil {
			return nil, err
		}
		if p.Creating {
			if err := s.catalog.CheckCreate(p.Subject, p.Project, p.Name); err != nil {
				return nil, err
			}
		}
		if p.Purpose == "backup" && s.catalog.Kind(p.Project) != "notes" {
			return nil, workspace.ErrForbidden
		}
		if err := s.beginFlow(ctx, "connection/ticket", digest(p.ID), p.Subject, p, 10*time.Minute, pg.FlowBound{Limit: flowLimit}); err != nil {
			return nil, err
		}
		return map[string]string{"id": p.ID, "url": s.origin + "/connections/start?ticket=" + url.QueryEscape(p.ID)}, nil
	})
	addManagementTool(s, server, "github_connection_status", "Get verified repository choices after completing the GitHub browser authorization.", true, func(ctx context.Context, in struct {
		Connection string `json:"connection"`
	}) (map[string]any, error) {
		p, err := s.connection(ctx, in.Connection)
		if err != nil {
			return nil, err
		}
		if p.Binding == "" {
			return map[string]any{"ready": false}, nil
		}
		grant, err := s.connectionGrant(ctx, p)
		if err != nil {
			return nil, err
		}
		var choices []githubapp.Choice
		var slug string
		if p.Purpose == "source" {
			choices, err = s.repositoryConnections.github.Choices(ctx, grant.Token)
			slug = s.repositoryConnections.github.Config.Slug
		} else {
			choices, err = s.backups.app.Choices(ctx, grant.Token)
			slug = s.backups.config.App.Slug
			filtered := []githubapp.Choice{}
			for _, c := range choices {
				if c.Private && c.Permissions.Push {
					filtered = append(filtered, c)
				}
			}
			choices = filtered
		}
		if err != nil {
			return nil, err
		}
		if err = s.connectionAuthority(ctx, p); err != nil {
			return nil, err
		}
		return map[string]any{"ready": true, "choices": choices, "installUrl": "https://github.com/apps/" + slug + "/installations/new"}, nil
	})
	addManagementTool(s, server, "finish_github_connection", "Complete a verified GitHub selection. Sources require shareWithNotebook; backups require shareHistory. External processing stays disabled unless explicitly selected. createOwner/createName request a new private backup repository when operator configuration permits it.", false, func(ctx context.Context, in connectionSelection) (any, error) {
		p, err := s.connection(ctx, in.Connection)
		if err != nil {
			return nil, err
		}
		grant, err := s.connectionGrant(ctx, p)
		if err != nil {
			return nil, err
		}
		if p.Purpose == "source" {
			if !in.ShareWithNotebook {
				return nil, errors.New("confirm sharing the source with notebook members")
			}
			if p.Creating {
				return s.completeRepositoryCreation(ctx, grant, repositorySelection{in.Installation, in.Repository, in.ExternalProcessing})
			}
			return integrationActivity(s, p.Subject, "repository.connect", p.Project, func() (reposource.State, error) {
				return s.repositories.Connect(ctx, p.Project, p.Subject, grant.Token, in.Installation, in.Repository)
			})
		}
		if !in.ShareHistory {
			return nil, errors.New("confirm sharing the full committed history with GitHub readers")
		}
		if in.CreateName != "" {
			if !s.backups.config.AllowCreate {
				return nil, workspace.ErrForbidden
			}
			return integrationActivity(s, p.Subject, "backup.create", p.Project, func() (githubapp.Repository, error) {
				return s.backups.manager.Create(ctx, s.backupSetup(grant, in.Generation), in.CreateOwner, in.CreateName)
			})
		}
		return s.selectManagedBackup(ctx, grant, in.Installation, in.Repository, in.Generation, in.Branch)
	})
}
