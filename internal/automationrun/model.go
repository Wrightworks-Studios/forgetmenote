package automationrun

import (
	"context"
	"errors"
	"slices"
	"time"

	"forgetmenote/internal/automation"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/workspace"
)

// Resolve the same runtime provider and restrictions for validation and execution.
func (s *Service) Provider(spec automation.Spec) (*chat.Config, chat.Connection, *chat.Provider, error) {
	snapshot := s.hooks.Chat().Snapshot()
	config := snapshot.Config
	id := spec.AIConnection
	if id == "" && config != nil {
		id = config.DefaultConnection
	}
	provider := snapshot.Providers[id]
	if config == nil || provider == nil || !spec.ExternalProcessing {
		return nil, chat.Connection{}, nil, errors.New("automation AI is not configured or consented")
	}
	for _, c := range config.Connections {
		if c.ID != id {
			continue
		}
		if config.LocalOnly && !c.Local {
			return nil, chat.Connection{}, nil, workspace.ErrForbidden
		}
		model := spec.Model
		if model == "" {
			model = c.DefaultModel
		}
		if !slices.Contains(c.Models, model) {
			return nil, chat.Connection{}, nil, errors.New("automation model is not configured")
		}
		if spec.Script != nil && slices.Contains(spec.Script.Capabilities, "ai.images") && !c.Capabilities.Images {
			return nil, chat.Connection{}, nil, errors.New("structured review requires a configured vision model")
		}
		return config, c, provider, nil
	}
	return nil, chat.Connection{}, nil, errors.New("automation AI connection is not configured")
}

// ReserveModel waits for the connection's capacity and holds it for one
// model use until the returned release.
func (s *Service) ReserveModel(ctx context.Context, config *chat.Config, connection chat.Connection) (context.Context, func(), error) {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(connection.Limits.RunSeconds)*time.Second)
	activeID := "automation-" + chat.NewID()
	// Background work waits for the existing admission owner instead of failing
	// merely because another independent job currently occupies the provider.
	for {
		if err := ctx.Err(); err != nil {
			cancel()
			return ctx, nil, err
		}
		_, err := s.hooks.Chat().Admit(config, connection, activeID, cancel, "", nil, nil)
		if err == nil {
			break
		}
		snapshot := s.hooks.Chat().Snapshot()
		if !errors.Is(err, chat.ErrCapacity) || snapshot.Closed || snapshot.Config != config {
			cancel()
			return ctx, nil, errors.New("model configuration is unavailable or changed")
		}
		select {
		case <-ctx.Done():
			cancel()
			return ctx, nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return ctx, func() {
		s.hooks.Chat().Finish(activeID, connection.ID)
		cancel()
	}, nil
}

// Model is a model admitted for one use by an automation run. It
// holds its connection's capacity until release.
type Model struct {
	Config     *chat.Config
	Connection chat.Connection
	Provider   *chat.Provider
	Release    func()
}

// ModelCall admits one model use for an automation run: the
// instance AI policy must allow it, ctx is bound to the run's source scope and
// the run's provider is resolved. Once admit, when set, accepts the model, for
// example by the run's token budget, the connection's capacity is reserved.
// The returned context ends with the connection's run allowance.
func (s *Service) ModelCall(ctx context.Context, r *automation.Run, admit func(Model) error) (context.Context, Model, error) {
	if err := s.hooks.AIWriting(); err != nil {
		return ctx, Model{}, err
	}
	ctx, err := s.SourceContext(ctx, r)
	if err != nil {
		return ctx, Model{}, err
	}
	var model Model
	if model.Config, model.Connection, model.Provider, err = s.Provider(r.Spec); err != nil {
		return ctx, Model{}, err
	}
	if admit != nil {
		if err := admit(model); err != nil {
			return ctx, Model{}, err
		}
	}
	ctx, model.Release, err = s.ReserveModel(ctx, model.Config, model.Connection)
	return ctx, model, err
}

// TokenBudget refuses a request whose estimate exceeds what remains
// of the run's token budget.
func TokenBudget(r *automation.Run, estimate int) error {
	if r.Spec.MaxTokens > 0 && r.InputTokens+r.OutputTokens+estimate > r.Spec.MaxTokens {
		return errors.New("automation token budget reached")
	}
	return nil
}
