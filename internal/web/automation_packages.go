package web

import (
	"context"
	"errors"
	"path"
	"time"

	"forgetmenote/internal/automation"
	"forgetmenote/internal/automationrun"
	"forgetmenote/internal/workspace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func (s *Server) packageTools(server *mcp.Server) {
	addManagementTool(s, server, "discover_automation_packages", "Inspect repository manifests as an administrator. Uninstalled packages remain inert. Installed packages stay pinned to their approved commit and digest: a different digest here is an available update to review and apply with install_automation_package, unless that installation set script.followPrimaryBranch.", true, func(ctx context.Context, q automationrun.PackageQuery) ([]automationrun.PackageInfo, error) {
		if !s.catalog.IsAdmin(subject(ctx)) {
			return nil, workspace.ErrForbidden
		}
		if q.Path != "" {
			info, _, err := s.automationrun.Package(ctx, subject(ctx), q, false)
			return []automationrun.PackageInfo{info}, err
		}
		repositories, err := s.repos()
		if err != nil {
			return nil, err
		}
		if _, err := s.clientAccess(ctx, subject(ctx), q.Project, "read"); err != nil {
			return nil, err
		}
		state, err := repositories.Status(q.Project)
		if err != nil {
			return nil, err
		}
		if q.Revision == "" {
			q.Revision = state.Downloaded
		}
		entries, err := repositories.Tree(ctx, q.Project, state.ID, q.Revision)
		if err != nil {
			return nil, err
		}
		out := []automationrun.PackageInfo{}
		for _, entry := range entries {
			if entry.Supported && path.Base(entry.Path) == "automation.json" {
				query := q
				query.Path = path.Dir(entry.Path)
				info, _, err := s.automationrun.Package(ctx, subject(ctx), query, false)
				if err == nil {
					out = append(out, info)
				}
			}
		}
		return out, nil
	})
	addManagementTool(s, server, "prepare_automation_package", "Prepare a pinned Python package without changing its job definition. For operator-managed installation; returns a paused spec with its immutable environment identity.", false, func(ctx context.Context, in struct {
		Spec automation.Spec `json:"spec"`
	}) (automation.Spec, error) {
		return s.prepareScriptPackage(ctx, in.Spec)
	})

	addManagementTool(s, server, "install_automation_package", "Create a paused installation or explicitly update/roll back one with its current definition revision. Source and runtime must be pinned; activation uses save_automation. The installation stays on this commit unless script.followPrimaryBranch is true, which opts it into adopting compatible primary-branch package changes without review.", false, func(ctx context.Context, in struct {
		Spec     automation.Spec `json:"spec"`
		Revision string          `json:"revision"`
	}) (automation.Definition, error) {
		if !s.catalog.IsAdmin(subject(ctx)) {
			return automation.Definition{}, workspace.ErrForbidden
		}
		in.Spec.Disabled = true
		if in.Spec.Kind != "script" {
			return automation.Definition{}, errors.New("script job required")
		}
		var err error
		in.Spec, err = s.prepareScriptPackage(ctx, in.Spec)
		if err != nil {
			return automation.Definition{}, err
		}
		d, err := s.automations.Definitions.Save(in.Spec, in.Revision)
		if err == nil {
			err = s.automations.Queue.CancelSuperseded(d.Spec.ID, automation.Hash(d.Spec))
		}
		if err == nil {
			err = s.catalog.RecordActivity(subject(ctx), "automation.install", d.Spec.ID, "succeeded", nil)
		}
		return d, err
	})
}

func (s *Server) prepareScriptPackage(ctx context.Context, spec automation.Spec) (automation.Spec, error) {
	if !s.catalog.IsAdmin(subject(ctx)) {
		return spec, workspace.ErrForbidden
	}
	if spec.Kind != "script" || spec.Script == nil {
		return spec, errors.New("script job required")
	}
	cfg := *spec.Script
	cfg.Environment = ""
	spec.Script = &cfg
	spec.Disabled = true
	if err := s.validateAutomationDefinition(ctx, spec); err != nil {
		return spec, err
	}
	info, files, err := s.automationrun.ScriptPackage(ctx, spec, true)
	if err != nil {
		return spec, err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(spec.TimeoutSeconds)*time.Second)
	defer cancel()
	spec.Script.Environment, err = s.python.Prepare(ctx, files, info.Digest)
	if err != nil {
		return spec, err
	}
	if !s.catalog.IsAdmin(subject(ctx)) {
		return spec, workspace.ErrForbidden
	}
	_, _, err = s.automationrun.ScriptPackage(ctx, spec, false)
	return spec, err
}
