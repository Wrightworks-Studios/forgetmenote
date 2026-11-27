package web

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"forgetmenote/internal/attribution"
	"forgetmenote/internal/auth"
	"forgetmenote/internal/automation"
	"forgetmenote/internal/workspace"
)

type automationService = automation.Runtime

func (s *Server) initAutomations() error {
	if s.automations != nil {
		return nil
	}
	runtime, err := automation.OpenRuntime(s.db)
	if err != nil {
		return err
	}
	s.automations = runtime
	return nil
}
func (s *Server) ConfigureAutomations(file string) error {
	if err := s.privateRuntimeFile(file); err != nil {
		return err
	}
	cfg, err := automation.Load(file)
	if err != nil {
		return err
	}
	return s.ConfigureAutomationsConfig(cfg)
}
func (s *Server) ConfigureAutomationsConfig(c automation.Config) error {
	unlock, err := s.startupConfiguration()
	if err != nil {
		return err
	}
	defer unlock()

	if e := c.Validate(); e != nil {
		return e
	}
	if e := s.initAutomations(); e != nil {
		return e
	}
	for _, spec := range c.Jobs {
		if !s.catalog.IsService(spec.Actor) {
			return errors.New("scheduled automations require a service account")
		}
	}
	return s.automations.Definitions.SetFiles(c.Jobs)
}

// StartAutomations retains the existing startup entry point.
func (s *Server) StartAutomations(ctx context.Context) error { return s.Start(ctx) }

func (s *Server) automationContext(ctx context.Context, spec automation.Spec) (context.Context, error) {
	if err := s.automationPolicy(spec); err != nil {
		return ctx, err
	}
	account, e := s.catalog.Account(spec.Actor)
	if e != nil {
		return ctx, e
	}
	if _, e = s.catalog.Access(spec.Actor, spec.Project, "read"); e != nil {
		return ctx, e
	}
	a := automation.Identity{Spec: spec, Account: account.ID, Epoch: s.catalog.Epoch(spec.Actor), ProjectEpoch: s.catalog.ProjectAccessEpoch(spec.Actor, spec.Project), Generation: s.catalog.Authority(spec.Project).Generation}
	ctx = attribution.WithSource(ctx, account.Identity(), "automation", spec.ID)
	ctx = context.WithValue(ctx, identityKey{}, spec.Actor)
	ctx = automation.WithIdentity(ctx, a)
	ctx = context.WithValue(ctx, scopeKey{}, "notes:read data:read")
	ctx = auth.WithAPI(ctx)
	ctx = workspace.WithAuthority(ctx, a.Generation)
	return ctx, nil
}
func (s *Server) checkAutomationAuth(ctx context.Context) error {
	a, ok := automation.IdentityFrom(ctx)
	if !ok {
		return nil
	}
	if err := s.automationPolicy(a.Spec); err != nil {
		return err
	}
	if fence, ok := ctx.Value(automationFenceKey{}).(automationFence); ok {
		d, exists := s.automations.Definitions.Get(fence.ID)
		if !exists || d.Spec.Disabled && !fence.Preview || automation.Hash(d.Spec) != fence.Hash {
			return errors.New("automation configuration changed")
		}
	}
	if a.Spec.Kind == "assistant" {
		if err := s.messaging.CheckMessageSpec(a.Spec); err != nil {
			return err
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if !s.catalog.ValidSession(a.Spec.Actor, a.Epoch) || s.catalog.ProjectAccessEpoch(a.Spec.Actor, a.Spec.Project) != a.ProjectEpoch || s.catalog.Authority(a.Spec.Project).Generation != a.Generation {
		return workspace.ErrForbidden
	}
	_, e := s.catalog.Access(a.Spec.Actor, a.Spec.Project, "read")
	return e
}

type automationFenceKey = automation.FenceKey
type automationFence = automation.Fence

func (s *Server) automationPolicy(spec automation.Spec) error {
	if spec.Kind == "personal" {
		if err := s.checkPersonalSpec(spec); err != nil {
			return err
		}
	}
	if policy, managed := s.catalog.AISettings(); managed && spec.UsesAI() && spec.Repair == nil && !policy.Writing {
		return errInstanceAI
	}
	return nil
}

// instanceAIWriting refuses a model call while the instance AI policy turns
// AI writing off. The automation engine asks it before each model call, a
// repair's too, which automationPolicy leaves to this check.
func (s *Server) instanceAIWriting() error {
	if policy, managed := s.catalog.AISettings(); managed && !policy.Writing {
		return errInstanceAI
	}
	return nil
}
func (s *Server) logAutomationError(stage string, err error) { s.automations.Report(stage, err) }

func (s *Server) ConfigureAutomationPython(executable string) error {
	unlock, err := s.startupConfiguration()
	if err != nil {
		return err
	}
	defer unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	runtime, err := automation.OpenPython(ctx, filepath.Join(s.data, "automations", "python"), executable, os.Getenv("AUTOMATION_PYTHON_WHEELHOUSE"))
	if err != nil {
		return err
	}
	s.python = runtime
	return nil
}
