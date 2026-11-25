package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"time"

	"forgetmenote/internal/automation"
	"forgetmenote/internal/webcore"
	"forgetmenote/internal/workspace"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type packageQuery struct {
	Project  string `json:"project"`
	Path     string `json:"path,omitempty"`
	Revision string `json:"revision,omitempty"`
}
type packageInfo struct {
	Manifest automation.Manifest `json:"manifest"`
	Project  string              `json:"project"`
	Source   string              `json:"source"`
	Path     string              `json:"path"`
	Revision string              `json:"revision"`
	Latest   string              `json:"latest"`
	Digest   string              `json:"digest"`
}

func (s *Server) automationPackage(ctx context.Context, actor string, q packageQuery, retain bool) (packageInfo, map[string]string, error) {
	var info packageInfo
	repositories, err := s.repos()
	if err != nil {
		return info, nil, err
	}
	if !automation.PackagePath(q.Path) {
		return info, nil, webcore.Invalid("repository notebook and package path required")
	}
	if _, err := s.clientAccess(ctx, actor, q.Project, "read"); err != nil {
		return info, nil, err
	}
	state, err := repositories.Status(q.Project)
	if err != nil {
		return info, nil, err
	}
	if !state.Connected || state.Paused {
		return info, nil, errors.New("source unavailable or paused")
	}
	if q.Revision == "" {
		q.Revision = state.Downloaded
	}
	manifestPath := path.Join(q.Path, "automation.json")
	files, err := repositories.PackageFiles(ctx, q.Project, state.ID, q.Revision, []string{manifestPath}, retain)
	if err != nil {
		return info, nil, err
	}
	m, err := automation.ParseManifest([]byte(files[manifestPath]))
	if err != nil {
		return info, nil, err
	}
	paths := []string{manifestPath}
	for _, f := range m.Files {
		paths = append(paths, path.Join(q.Path, f))
	}
	files, err = repositories.PackageFiles(ctx, q.Project, state.ID, q.Revision, paths, retain)
	if err != nil {
		return info, nil, err
	}
	relative := map[string]string{}
	for p, v := range files {
		relative[strings.TrimPrefix(p, q.Path+"/")] = v
	}
	info = packageInfo{Manifest: m, Project: q.Project, Source: state.ID, Path: q.Path, Revision: q.Revision, Latest: state.Downloaded, Digest: automation.PackageDigest(relative)}
	return info, relative, nil
}
func (s *Server) scriptPackage(ctx context.Context, spec automation.Spec, retain bool) (packageInfo, map[string]string, error) {
	cfg := spec.Script
	if cfg == nil {
		return packageInfo{}, nil, errors.New("missing script installation")
	}
	info, files, err := s.automationPackage(ctx, spec.Actor, packageQuery{cfg.SourceProject, cfg.PackagePath, cfg.Revision}, retain)
	if err != nil {
		return info, nil, err
	}
	if info.Source != cfg.SourceID || info.Digest != cfg.Digest || info.Manifest.Runtime != cfg.Runtime || info.Manifest.Entrypoint != cfg.Entrypoint {
		return info, nil, errors.New("installed package identity does not match approved source")
	}
	if !slices.Contains(spec.ReadProjects, cfg.SourceProject) && spec.Project != cfg.SourceProject {
		return info, nil, errors.New("source notebook must be in job readProjects")
	}
	for _, cap := range info.Manifest.Capabilities {
		if !slices.Contains(cfg.Capabilities, cap) {
			return info, nil, errors.New("package capability not granted: " + cap)
		}
	}
	for _, cap := range cfg.Capabilities {
		if !info.Manifest.RequestsCapability(cap) {
			return info, nil, errors.New("grant is not requested by package: " + cap)
		}
	}
	for alias, ops := range info.Manifest.Connections {
		id := cfg.Connections[alias]
		if !slices.Contains(spec.Connections, id) {
			return info, nil, errors.New("connection binding missing: " + alias)
		}
		for _, op := range ops {
			if !slices.Contains(spec.Operations[id], op) {
				return info, nil, errors.New("operation grant missing: " + op)
			}
		}
	}
	for alias := range cfg.Connections {
		if _, ok := info.Manifest.Connections[alias]; !ok && len(info.Manifest.OptionalConnections[alias]) == 0 {
			return info, nil, errors.New("unrequested connection binding")
		}
	}
	for alias, role := range info.Manifest.Notebooks {
		p := cfg.Notebooks[alias]
		if p == "" || p != spec.Project && !slices.Contains(spec.ReadProjects, p) {
			return info, nil, errors.New("notebook binding missing: " + alias)
		}
		if role == "edit" && p != spec.Project {
			return info, nil, errors.New("notebook publication is limited to the job project")
		}
		if _, err = s.clientAccess(ctx, spec.Actor, p, role); err != nil {
			return info, nil, err
		}
	}
	for alias := range cfg.Notebooks {
		if _, ok := info.Manifest.Notebooks[alias]; !ok {
			return info, nil, errors.New("unrequested notebook binding")
		}
	}
	if len(info.Manifest.ParametersSchema) > 0 {
		if _, err = validateStructuredJSON(info.Manifest.ParametersSchema, cfg.Parameters); err != nil {
			return info, nil, err
		}
	}
	return info, files, nil
}
func validateStructuredJSON(schemaRaw json.RawMessage, value any) (json.RawMessage, error) {
	var schema jsonschema.Schema
	if json.Unmarshal(schemaRaw, &schema) != nil {
		return nil, errors.New("invalid JSON schema")
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		return nil, err
	}
	if err = resolved.Validate(value); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

// Installations are pinned to the digest an administrator approved: write access
// to the source repository is not review. A newer digest stays an available
// update in discover_automation_packages until an explicit install/update. Only
// installations that set script.followPrimaryBranch adopt compatible changes here.
func (s *Server) refreshAutomationPackages(ctx context.Context) {
	if s.repositories == nil || s.python == nil {
		return
	}
	for _, definition := range s.automations.Definitions.List() {
		if ctx.Err() != nil {
			return
		}
		if definition.ManagedBy != "instance" || definition.Spec.Script == nil {
			continue
		}
		if err := s.refreshAutomationPackage(ctx, definition); err != nil {
			s.automations.Report("package update "+definition.Spec.ID, err)
		}
	}
}

func (s *Server) refreshAutomationPackage(ctx context.Context, definition automation.Definition) error {
	spec := definition.Spec
	cfg := *spec.Script
	if !cfg.FollowPrimaryBranch {
		return nil
	}
	state, err := s.repositories.Status(cfg.SourceProject)
	if err != nil {
		return err
	}
	if !state.Connected || state.Paused || state.ID != cfg.SourceID || state.Downloaded == "" || state.Downloaded == cfg.Revision {
		return nil
	}
	changes, err := s.repositories.Changes(ctx, cfg.SourceProject, cfg.SourceID, cfg.Revision, state.Downloaded)
	if err != nil {
		return err
	}
	if changes.BaselineRequired {
		return errors.New("package history changed; prepare an explicit administrator update")
	}
	changed := false
	for _, p := range append(changes.Paths, changes.Removed...) {
		if strings.HasPrefix(p, cfg.PackagePath+"/") {
			changed = true
			break
		}
	}
	if !changed {
		return nil
	}
	previous, oldFiles, err := s.scriptPackage(ctx, spec, false)
	if err != nil {
		return err
	}
	latest, files, err := s.automationPackage(ctx, spec.Actor, packageQuery{cfg.SourceProject, cfg.PackagePath, state.Downloaded}, true)
	if err != nil {
		return err
	}
	if latest.Digest == cfg.Digest {
		return nil
	}
	oldContract, newContract := previous.Manifest, latest.Manifest
	oldContract.Files, newContract.Files = nil, nil
	oldContract.Name, newContract.Name = "", ""
	if automation.Hash(oldContract) != automation.Hash(newContract) || oldFiles[previous.Manifest.Requirements] != files[latest.Manifest.Requirements] {
		return errors.New("package grants, entrypoint or dependencies changed; prepare an explicit administrator update")
	}
	cfg.Revision, cfg.Digest, cfg.Environment = latest.Revision, latest.Digest, ""
	spec.Script = &cfg
	// Preparation validates the existing service authority without requiring the
	// global AI switch or enabling a paused job. No package code runs here.
	paused := spec
	paused.Disabled = true
	if err = s.validateAutomationDefinition(ctx, paused); err != nil {
		return err
	}
	prepareCtx, cancel := context.WithTimeout(ctx, time.Duration(spec.TimeoutSeconds)*time.Second)
	defer cancel()
	cfg.Environment, err = s.python.Prepare(prepareCtx, files, latest.Digest)
	if err != nil {
		return err
	}
	current, _, err := s.scriptPackage(ctx, spec, false)
	if err != nil {
		return err
	}
	if current.Latest != cfg.Revision || current.Source != cfg.SourceID {
		return errors.New("package source advanced during preparation; update deferred")
	}
	updated, err := s.automations.Queue.UpdateIdleDefinition(s.automations.Definitions, spec, definition.Revision)
	if err == nil && updated {
		err = s.catalog.RecordActivity(spec.Actor, "automation.package-update", spec.ID, "succeeded", map[string]string{"revision": cfg.Revision})
	}
	if err != nil {
		return fmt.Errorf("automatic package update: %w", err)
	}
	return nil
}
func (s *Server) packageTools(server *mcp.Server) {
	addManagementTool(s, server, "discover_automation_packages", "Inspect repository manifests as an administrator. Uninstalled packages remain inert. Installed packages stay pinned to their approved commit and digest: a different digest here is an available update to review and apply with install_automation_package, unless that installation set script.followPrimaryBranch.", true, func(ctx context.Context, q packageQuery) ([]packageInfo, error) {
		if !s.catalog.IsAdmin(subject(ctx)) {
			return nil, workspace.ErrForbidden
		}
		if q.Path != "" {
			info, _, err := s.automationPackage(ctx, subject(ctx), q, false)
			return []packageInfo{info}, err
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
		out := []packageInfo{}
		for _, entry := range entries {
			if entry.Supported && path.Base(entry.Path) == "automation.json" {
				query := q
				query.Path = path.Dir(entry.Path)
				info, _, err := s.automationPackage(ctx, subject(ctx), query, false)
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
	info, files, err := s.scriptPackage(ctx, spec, true)
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
	_, _, err = s.scriptPackage(ctx, spec, false)
	return spec, err
}
