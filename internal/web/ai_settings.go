package web

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/knowledge"
	"forgetmenote/internal/reposource"
	"forgetmenote/internal/search"
	"forgetmenote/internal/workspace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var errInstanceAI = catalog.ErrManagedAI

func (s *Server) instanceAITools(server *mcp.Server) {
	addManagementTool(s, server, "ai_settings", "Read instance-wide AI policy, provider availability and notebook processing status. Admin only; never returns credentials.", true, func(ctx context.Context, in struct{}) (map[string]any, error) {
		return s.instanceAIStatus(ctx, subject(ctx))
	})
	addManagementTool(s, server, "set_ai_settings", "Set instance-wide AI features for existing and future notebooks. Admin only. Supply the current config revision. Enabling permits processing by the configured providers; model changes require rebuild=true. Keys never enable features by themselves.", false, func(ctx context.Context, in struct {
		Config  catalog.AISettings `json:"config"`
		Rebuild bool               `json:"rebuild,omitempty"`
	}) (map[string]any, error) {
		if !s.catalog.IsAdmin(subject(ctx)) {
			return nil, workspace.ErrForbidden
		}
		s.aiMu.Lock()
		defer s.aiMu.Unlock()
		in.Config.Model = strings.TrimSpace(in.Config.Model)
		current, managed := s.catalog.AISettings()
		if current.Revision != in.Config.Revision {
			return nil, workspace.ErrConflict
		}
		endpoint := s.indexes.Endpoint()
		judge, localOnly := s.jevConfigured()
		vision, transcription := s.sourceProviderAvailability()
		if (in.Config.SourceOCR || in.Config.SourceDescriptions) && !vision || in.Config.SourceTranscription && !transcription {
			return nil, fmt.Errorf("%w: configure an allowed hosted source processor first", catalog.ErrLocalInput)
		}
		if in.Config.Embeddings && (endpoint == nil || s.chatEmbeddingPolicy(endpoint) == nil) {
			return nil, fmt.Errorf("%w: an allowed embedding provider must be configured first", catalog.ErrLocalInput)
		}
		if (in.Config.Rerank || in.Config.MapClassification || in.Config.Citations || in.Config.Reviews) && (judge == nil || localOnly) {
			return nil, fmt.Errorf("%w: an allowed decision provider must be configured first", catalog.ErrLocalInput)
		}
		if in.Config.Embeddings && !in.Rebuild {
			if managed && current.Model != "" && (current.Model != in.Config.Model || current.Dimensions != in.Config.Dimensions) {
				return nil, fmt.Errorf("%w: confirm rebuilding embeddings when changing the model or dimensions", catalog.ErrLocalInput)
			}
			for _, project := range s.catalog.List(catalog.Bootstrap) {
				if project.ProtectedInbox != "" {
					continue
				}
				if project.Kind == "repository" {
					if s.repositories != nil {
						state, err := s.repositories.Status(project.ID)
						if err != nil {
							return nil, err
						}
						previous := state.IndexConfig
						if previous.Model != "" && (previous.Model != in.Config.Model || previous.Dimensions != in.Config.Dimensions) {
							return nil, fmt.Errorf("%w: existing repository embeddings use a different model; confirm rebuilding embeddings", catalog.ErrLocalInput)
						}
					}
					continue
				}
				store, err := s.catalog.Access(catalog.Bootstrap, project.ID, "read")
				if err != nil {
					return nil, err
				}
				previous, err := withEngine(s, ctx, store, func(e *knowledge.Engine) (knowledge.Config, error) { return e.Config(), nil })
				if err != nil {
					return nil, err
				}
				if previous.Model != "" && previous.ID() != search.EmbeddingConfig(previous, in.Config).ID() {
					return nil, fmt.Errorf("%w: existing notebook embeddings use a different configuration; confirm rebuilding embeddings", catalog.ErrLocalInput)
				}
			}
		}
		if _, err := s.catalog.SetAISettings(subject(ctx), in.Config); err != nil {
			return nil, err
		}
		// The persisted policy immediately fences external calls. Reconciliation
		// is repeatable after interruption; failures remain visible in status.
		s.syncInstanceAI(ctx)
		return s.instanceAIStatus(ctx, subject(ctx))
	})
}

func (s *Server) instanceAIStatus(ctx context.Context, actor string) (map[string]any, error) {
	if !s.catalog.IsAdmin(actor) {
		return nil, workspace.ErrForbidden
	}
	config, managed := s.catalog.AISettings()
	endpoint := s.indexes.Endpoint()
	embeddings := endpoint != nil && s.chatEmbeddingPolicy(endpoint) != nil
	judge, localOnly := s.jevConfigured()
	sourceVision, sourceTranscription := s.sourceProviderAvailability()
	notebooks := []map[string]any{}
	for _, project := range s.catalog.List(actor) {
		if project.ProtectedInbox != "" {
			continue
		}
		row := map[string]any{"id": project.ID, "name": project.Name, "kind": project.Kind}
		if project.Kind == "repository" && s.repositories != nil {
			state, err := s.repositories.Status(project.ID)
			if err != nil {
				row["error"] = err.Error()
			} else {
				row["index"], row["paused"] = state.Index, state.Paused
			}
		} else if store, err := s.catalog.Access(actor, project.ID, "read"); err == nil {
			pending, err := withEngine(s, ctx, store, func(e *knowledge.Engine) (knowledge.Pending, error) { return e.Pending(ctx, 1) })
			if err != nil {
				row["error"] = err.Error()
			} else {
				row["semantic"] = pending.Status
			}
		}
		notebooks = append(notebooks, row)
	}
	return map[string]any{"config": config, "managed": managed, "providers": map[string]any{"embeddings": embeddings, "decisions": judge != nil, "localOnly": localOnly, "sourceVision": sourceVision, "sourceTranscription": sourceTranscription}, "notebooks": notebooks}, nil
}

func (s *Server) syncInstanceAI(ctx context.Context) {
	policy, managed := s.catalog.AISettings()
	if !managed {
		return
	}
	for _, project := range s.catalog.List(catalog.Bootstrap) {
		if ctx.Err() != nil {
			return
		}
		if project.Kind == "repository" {
			if s.repositories != nil {
				_, _ = s.repositories.Status(project.ID)
			}
			continue
		}
		if store, err := s.catalog.Access(catalog.Bootstrap, project.ID, "read"); err == nil {
			_ = s.indexes.Register(ctx, store)
			if policy.Reviews && project.Ordinary() {
				_ = s.prepareInstanceReview(ctx, project.ID)
			}
		}
	}
}

func (s *Server) sourceIndexPolicy(state reposource.State) (reposource.IndexConfig, string) {
	config := state.IndexConfig
	settings, managed := s.catalog.AISettings()
	if !managed {
		return config, state.MapActor
	}
	config.Embeddings, config.Jev, config.MapJev = settings.Embeddings, settings.Rerank, settings.MapClassification
	config.ExternalProcessing = settings.Embeddings || settings.Rerank || settings.MapClassification || settings.Citations || settings.Reviews || settings.Writing
	if settings.Embeddings {
		config.Model, config.Dimensions = settings.Model, settings.Dimensions
	}
	actor := ""
	if config.MapJev {
		actor = catalog.Bootstrap
	}
	return config, actor
}

func (s *Server) rejectNotebookAI(w http.ResponseWriter) bool {
	if _, managed := s.catalog.AISettings(); managed {
		http.Error(w, errInstanceAI.Error(), http.StatusConflict)
		return true
	}
	return false
}

func (s *Server) decisionPreferences(user string) (jevPreferences, error) {
	return s.decisions.Preferences(user)
}
func (s *Server) prepareInstanceReview(ctx context.Context, project string) error {
	settings, err := s.loadJevNotebook(project)
	if err != nil {
		return err
	}
	if settings.AutoChecks && settings.EnabledBy == catalog.Bootstrap {
		return nil
	}
	store, err := s.catalog.Access(catalog.Bootstrap, project, "owner")
	if err != nil {
		return err
	}
	snapshot, err := store.SnapshotScoped(ctx, "", jevActiveScope())
	if err != nil {
		return err
	}
	account, err := s.catalog.Account(catalog.Bootstrap)
	if err != nil {
		return err
	}
	s.jevState.mu.Lock()
	defer s.jevState.mu.Unlock()
	// Change the current record so concurrent organization updates survive.
	_, err = s.jevState.updateNotebook(project, func(settings *jevNotebookSettings, _ bool) error {
		if settings.AutoChecks && settings.EnabledBy == catalog.Bootstrap {
			return nil
		}
		settings.AutoChecks, settings.EnabledBy, settings.AccountID = true, catalog.Bootstrap, account.ID
		settings.Baseline, settings.BaselineRevision = map[string]string{}, snapshot.Revision
		for _, file := range snapshot.Files {
			settings.Baseline[file.Path] = file.Hash
		}
		settings.Observed, settings.ObservedAt = nil, ""
		return nil
	})
	return err
}
