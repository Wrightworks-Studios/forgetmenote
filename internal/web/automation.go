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
	if !s.modules.Enabled("automations") {
		return nil
	}
	if err := s.privateRuntimeFile(file); err != nil {
		return err
	}
	cfg, err := automation.Load(file)
	if err != nil {
		return err
	}
	return s.ConfigureAutomationsConfig(cfg)
}

// ConfigureAutomationsConfig installs the instance's file-defined jobs. With
// automations off they are left unread, and the definitions stored while the
// module ran stay as they were.
func (s *Server) ConfigureAutomationsConfig(c automation.Config) error {
	unlock, err := s.startupConfiguration()
	if err != nil {
		return err
	}
	defer unlock()
	if !s.modules.Enabled("automations") {
		return nil
	}

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

// automationModules names the modules a run needs: automations, the module
// whose work it does, messaging for a Slack trigger or delivery, and chat,
// whose engine runs every model call.
func automationModules(spec automation.Spec) []string {
	out := []string{"automations"}
	switch {
	case spec.Kind == "personal":
		out = append(out, "personal")
	case spec.Native != nil && spec.Native.Task == "task-reminder":
		out = append(out, "tasks")
	}
	if spec.Kind == "assistant" || spec.Native != nil && spec.Native.Task == "email-pairing" || spec.Delivery != nil || spec.NotifyChannel != "" || spec.NotifySlackUser != "" || spec.Trigger != nil && spec.Trigger.Kind == "slack.knowledge" {
		out = append(out, "messaging")
	}
	if spec.UsesAI() {
		out = append(out, "chat")
	}
	return out
}

// automationModulesOn refuses an automation that needs a module the instance
// turns off.
func (s *Server) automationModulesOn(spec automation.Spec) error {
	for _, module := range automationModules(spec) {
		if err := s.modules.Check(module); err != nil {
			return err
		}
	}
	return nil
}

// automationPolicy decides whether a run may be scheduled, claimed and go on
// running. A run that needs a module the instance turns off is neither
// scheduled nor claimed: one queued while the module ran stays queued, and a
// running one stops at its next check.
func (s *Server) automationPolicy(spec automation.Spec) error {
	if err := s.automationModulesOn(spec); err != nil {
		return err
	}
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

// ConfigureAutomationPython prepares the runtime that script automations run
// in. With automations off nothing runs a script, so it is not prepared.
func (s *Server) ConfigureAutomationPython(executable string) error {
	unlock, err := s.startupConfiguration()
	if err != nil {
		return err
	}
	defer unlock()
	if !s.modules.Enabled("automations") {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	runtime, err := automation.OpenPython(ctx, filepath.Join(s.data, "automations", "python"), executable, os.Getenv("AUTOMATION_PYTHON_WHEELHOUSE"))
	if err != nil {
		return err
	}
	s.python = runtime
	return nil
}
