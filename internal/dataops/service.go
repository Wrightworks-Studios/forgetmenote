// Package dataops owns typed data-operation execution across transports.
// Connector credentials and I/O remain with connectors; catalog authority and
// notebook profile scope are rechecked through the existing instance policy.
package dataops

import (
	"context"
	"encoding/json"
	"errors"
	"forgetmenote/internal/auth"
	"forgetmenote/internal/automation"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/connectors"
	"forgetmenote/internal/outgoing"
	"forgetmenote/internal/workspace"
	"log/slog"
	"path/filepath"
	"time"
)

type Hooks struct {
	Manager      func() *connectors.Manager
	Allowed      func(context.Context, string, string) bool
	WriteAllowed func(context.Context, Call) bool
	ReadProfile  func(context.Context, Call) error
	Submit       func(context.Context, Call, json.RawMessage) (connectors.Result, error)
	Observe      func(context.Context, Call, connectors.Receipt, error, time.Time)
	// Outgoing, when paused, refuses writes; read-only queries still answer.
	Outgoing *outgoing.Mode
}

// Service is the application boundary for one configured connector invocation.
// Hooks retain their concrete policy, managed-write and learning owners.
type Service struct {
	catalog *catalog.Catalog
	hooks   Hooks
}

func New(projects *catalog.Catalog, hooks Hooks) (*Service, error) {
	if projects == nil || hooks.Manager == nil || hooks.Allowed == nil || hooks.WriteAllowed == nil || hooks.ReadProfile == nil || hooks.Submit == nil || hooks.Observe == nil {
		return nil, errors.New("data operations require shared authority, profile, receipt and observation owners")
	}
	return &Service{catalog: projects, hooks: hooks}, nil
}

type Call struct {
	Project         string                      `json:"project,omitempty"`
	Connection      string                      `json:"connection"`
	Operation       string                      `json:"operation"`
	Arguments       map[string]any              `json:"arguments"`
	SchemaHash      string                      `json:"schemaHash,omitempty"`
	ProfilePath     string                      `json:"profilePath,omitempty"`
	ProfileRevision string                      `json:"profileRevision,omitempty"`
	LearningContext *automation.LearningContext `json:"learningContext,omitempty" jsonschema:"Optional task ID, stable call request ID, and task intent for usage learning. No credentials or full conversation transcript."`
}
type Result struct {
	connectors.Result
	ProfilePath     string `json:"profilePath,omitempty"`
	ProfileRevision string `json:"profileRevision,omitempty"`
	// Warning reports a completed write whose outcome the activity log could
	// not record. The write happened; repeating it repeats its effect.
	Warning string `json:"warning,omitempty"`
}

// Execute records a pending activity receipt, calls the operation, then records
// its outcome and observes the call. When the outcome record fails, a failed
// call still returns its own error, and a completed read fails closed: nothing
// it would disclose is recorded, and repeating it is harmless. A completed
// write is still returned, with a Warning, since refusing its result would
// invite the caller to repeat an external effect that already happened. The
// pending receipt remains in the activity log either way.
func (s *Service) Execute(ctx context.Context, in Call) (Result, error) {
	manager := s.hooks.Manager()
	op, ok := manager.ConfiguredOperation(in.Connection, in.Operation)
	if !ok || !auth.HasScope(ctx, "data:read") || !s.hooks.Allowed(ctx, in.Connection, in.Operation) ||
		!op.ReadOnly && !s.hooks.WriteAllowed(ctx, in) {
		return Result{}, workspace.ErrForbidden
	}
	if !op.ReadOnly {
		if err := s.hooks.Outgoing.Check(); err != nil {
			return Result{}, err
		}
	}
	if in.ProfilePath != "" {
		if in.Project == "" || in.ProfileRevision == "" || filepath.Ext(in.ProfilePath) != ".md" {
			return Result{}, errors.New("profile references require notebook, Markdown path, and revision")
		}
		if err := s.hooks.ReadProfile(ctx, in); err != nil {
			return Result{}, err
		}
	}
	if !s.hooks.Allowed(ctx, in.Connection, in.Operation) {
		return Result{}, workspace.ErrForbidden
	}
	if err := s.catalog.RecordActivity(auth.Subject(ctx), "data.call", in.Connection, "pending", map[string]string{"operation": in.Operation}); err != nil {
		return Result{}, err
	}
	raw, _ := json.Marshal(in.Arguments)
	ctx = connectors.WithAuthority(ctx, func(ctx context.Context) error {
		if !s.hooks.Allowed(ctx, in.Connection, in.Operation) {
			return workspace.ErrForbidden
		}
		return nil
	})
	started := time.Now()
	var result connectors.Result
	var err error
	if !op.ReadOnly && manager.ManagedWrites(in.Connection) {
		result, err = s.hooks.Submit(ctx, in, raw)
	} else {
		result, err = manager.Call(ctx, in.Connection, in.Operation, raw, in.SchemaHash)
	}
	outcome := "succeeded"
	if err != nil {
		outcome = "failed"
	}
	recorded := s.catalog.RecordActivity(auth.Subject(ctx), "data.call", in.Connection, outcome, map[string]string{"operation": in.Operation, "receipt": result.Receipt.ID})
	if !s.hooks.Allowed(ctx, in.Connection, in.Operation) {
		return Result{}, workspace.ErrForbidden
	}
	s.hooks.Observe(ctx, in, result.Receipt, err, started)
	out := Result{Result: result, ProfilePath: in.ProfilePath, ProfileRevision: in.ProfileRevision}
	if recorded != nil {
		slog.Warn("data call outcome not recorded", "connection", in.Connection, "operation", in.Operation, "outcome", outcome, "receipt", result.Receipt.ID, "error", recorded)
		switch {
		case err != nil:
			// The call's own failure remains its answer.
		case op.ReadOnly:
			return Result{}, recorded
		default:
			out.Warning = "The write completed, but the activity log could not record its outcome; do not repeat it."
		}
	}
	return out, err
}
