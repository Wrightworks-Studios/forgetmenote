package web

import (
	"context"
	"forgetmenote/internal/reposource"
	"forgetmenote/internal/workspace"
	"net/http"
)

func (s *Server) repositoryCreationRoutes() {
	s.mux.HandleFunc("POST /api/projects/repository/connect", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ ID, Name string }
		if !decode(w, r, &in) {
			return
		}
		if err := s.catalog.CheckCreate(subject(r.Context()), in.ID, in.Name); err != nil {
			fail(w, err)
			return
		}
		g, err := s.repoApp()
		if err != nil {
			fail(w, err)
			return
		}
		s.beginRepositoryConnection(w, r, g, repositoryGrant{Creating: true, Project: in.ID, Name: in.Name})
	})
	s.mux.HandleFunc("GET /api/projects/repository/choices", func(w http.ResponseWriter, r *http.Request) {
		g, err := s.repoApp()
		if err != nil {
			fail(w, err)
			return
		}
		p, err := s.repositoryGrant(r)
		if err != nil || !p.Creating {
			fail(w, reposource.ErrDenied)
			return
		}
		choices, err := g.Choices(r.Context(), p.Token)
		respond(w, map[string]any{"project": p.Project, "name": p.Name, "choices": choices, "installUrl": "https://github.com/apps/" + g.Config.Slug + "/installations/new"}, err)
	})
	s.mux.HandleFunc("POST /api/projects/repository/finish", s.finishRepositoryCreation)
}

func (s *Server) finishRepositoryCreation(w http.ResponseWriter, r *http.Request) {
	if _, err := s.repos(); err != nil {
		fail(w, err)
		return
	}
	var in struct {
		Installation, Repository              int64
		ShareWithNotebook, ExternalProcessing bool
	}
	if !decode(w, r, &in) {
		return
	}
	if !in.ShareWithNotebook {
		http.Error(w, "Confirm sharing this source with notebook members", 400)
		return
	}
	p, err := s.repositoryGrant(r)
	if err != nil {
		fail(w, err)
		return
	}
	out, err := s.completeRepositoryCreation(r.Context(), p, repositorySelection{in.Installation, in.Repository, in.ExternalProcessing})
	respond(w, out, err)
}

type repositorySelection struct {
	Installation, Repository int64
	ExternalProcessing       bool
}

func (s *Server) completeRepositoryCreation(ctx context.Context, grant repositoryGrant, in repositorySelection) (map[string]string, error) {
	// One creation or import completes at a time on every gateway; duplicate
	// submissions cannot create another notebook or reconnect it to a
	// different repository.
	repositories, err := s.repos()
	if err != nil {
		return nil, err
	}
	g, err := s.repoApp()
	if err != nil {
		return nil, err
	}
	release, err := s.db.Exclusive(ctx, "repository-creation")
	if err != nil {
		return nil, flowError(err)
	}
	defer release()
	p, ok, err := s.grant(ctx, "repository/grant", grant.Binding, g.Client)
	if err != nil {
		return nil, err
	}
	if !ok || !p.Creating || s.repositoryGrantAuthority(p) != nil {
		return nil, reposource.ErrDenied
	}
	if p.Created {
		if _, err = s.catalog.Access(p.Subject, p.Project, "owner"); err != nil {
			return nil, err
		}
		if in.Repository != p.Repository || in.Installation != p.Installation || in.ExternalProcessing != p.ExternalProcessing {
			return nil, workspace.ErrConflict
		}
		state, e := repositories.Status(p.Project)
		if e == nil && state.Connected && state.Repository.ID == in.Repository && state.Installation == in.Installation {
			if p.ExternalProcessing && !state.IndexConfig.ExternalProcessing {
				cfg := state.IndexConfig
				cfg.ExternalProcessing = true
				if _, e = repositories.ConfigureIndex(p.Project, p.Subject, state.Generation, cfg); e != nil {
					return nil, e
				}
			}
			return map[string]string{"project": p.Project}, nil
		}
	}
	choices, err := g.Choices(ctx, p.Token)
	if err != nil {
		return nil, err
	}
	found := false
	for _, c := range choices {
		if c.ID == in.Repository && c.Installation == in.Installation {
			found = true
		}
	}
	if !found {
		return nil, reposource.ErrDenied
	}
	if _, _, _, err = g.Resolve(ctx, in.Installation, in.Repository); err != nil {
		return nil, err
	}
	if !p.Created {
		if err = s.catalog.CreateKind(p.Subject, p.Project, p.Name, "repository"); err != nil {
			return nil, err
		}
		p.Created, p.Repository, p.Installation = true, in.Repository, in.Installation
		p.ExternalProcessing = in.ExternalProcessing
		if err = s.saveGrant(ctx, "repository/grant", g.Client, p); err != nil {
			return nil, err
		}
	}
	state, err := integrationActivity(s, p.Subject, "repository.connect", p.Project, func() (reposource.State, error) {
		return repositories.Connect(ctx, p.Project, p.Subject, p.Token, in.Installation, in.Repository)
	})
	if err != nil {
		return nil, err
	}
	if in.ExternalProcessing {
		cfg := state.IndexConfig
		cfg.ExternalProcessing = true
		_, err = repositories.ConfigureIndex(p.Project, p.Subject, state.Generation, cfg)
		if err != nil {
			return nil, err
		}
	}
	return map[string]string{"project": p.Project}, nil
}
