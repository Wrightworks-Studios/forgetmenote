package automationrun

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
	"github.com/google/jsonschema-go/jsonschema"
)

// PackageQuery names a script package: its repository notebook, its
// directory and the revision to read, the downloaded one when empty.
type PackageQuery struct {
	Project  string `json:"project"`
	Path     string `json:"path,omitempty"`
	Revision string `json:"revision,omitempty"`
}

// PackageInfo is a package's manifest, where it was read and its digest.
type PackageInfo struct {
	Manifest automation.Manifest `json:"manifest"`
	Project  string              `json:"project"`
	Source   string              `json:"source"`
	Path     string              `json:"path"`
	Revision string              `json:"revision"`
	Latest   string              `json:"latest"`
	Digest   string              `json:"digest"`
}

// Package reads a package's manifest and files from its repository notebook,
// which actor must be able to read. retain keeps the files as source evidence
// after the repository's cache drops their commit.
func (s *Service) Package(ctx context.Context, actor string, q PackageQuery, retain bool) (PackageInfo, map[string]string, error) {
	var info PackageInfo
	repositories, err := s.repos()
	if err != nil {
		return info, nil, err
	}
	if !automation.PackagePath(q.Path) {
		return info, nil, webcore.Invalid("repository notebook and package path required")
	}
	if _, err := s.hooks.ClientAccess(ctx, actor, q.Project, "read"); err != nil {
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
	info = PackageInfo{Manifest: m, Project: q.Project, Source: state.ID, Path: q.Path, Revision: q.Revision, Latest: state.Downloaded, Digest: automation.PackageDigest(relative)}
	return info, relative, nil
}

// ScriptPackage reads an installed script's package and checks it against
// the installation: the approved source, digest, runtime and entrypoint, and
// the capabilities, connections, notebooks and parameters it grants.
func (s *Service) ScriptPackage(ctx context.Context, spec automation.Spec, retain bool) (PackageInfo, map[string]string, error) {
	cfg := spec.Script
	if cfg == nil {
		return PackageInfo{}, nil, errors.New("missing script installation")
	}
	info, files, err := s.Package(ctx, spec.Actor, PackageQuery{cfg.SourceProject, cfg.PackagePath, cfg.Revision}, retain)
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
		if _, err = s.hooks.ClientAccess(ctx, spec.Actor, p, role); err != nil {
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
func (s *Service) refreshAutomationPackages(ctx context.Context) {
	if s.hooks.Repositories() == nil || s.hooks.Python() == nil {
		return
	}
	for _, definition := range s.runtime.Definitions.List() {
		if ctx.Err() != nil {
			return
		}
		if definition.ManagedBy != "instance" || definition.Spec.Script == nil {
			continue
		}
		if err := s.refreshAutomationPackage(ctx, definition); err != nil {
			s.runtime.Report("package update "+definition.Spec.ID, err)
		}
	}
}

func (s *Service) refreshAutomationPackage(ctx context.Context, definition automation.Definition) error {
	spec := definition.Spec
	cfg := *spec.Script
	if !cfg.FollowPrimaryBranch {
		return nil
	}
	state, err := s.hooks.Repositories().Status(cfg.SourceProject)
	if err != nil {
		return err
	}
	if !state.Connected || state.Paused || state.ID != cfg.SourceID || state.Downloaded == "" || state.Downloaded == cfg.Revision {
		return nil
	}
	changes, err := s.hooks.Repositories().Changes(ctx, cfg.SourceProject, cfg.SourceID, cfg.Revision, state.Downloaded)
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
	previous, oldFiles, err := s.ScriptPackage(ctx, spec, false)
	if err != nil {
		return err
	}
	latest, files, err := s.Package(ctx, spec.Actor, PackageQuery{cfg.SourceProject, cfg.PackagePath, state.Downloaded}, true)
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
	if err = s.hooks.ValidateDefinition(ctx, paused); err != nil {
		return err
	}
	prepareCtx, cancel := context.WithTimeout(ctx, time.Duration(spec.TimeoutSeconds)*time.Second)
	defer cancel()
	cfg.Environment, err = s.hooks.Python().Prepare(prepareCtx, files, latest.Digest)
	if err != nil {
		return err
	}
	current, _, err := s.ScriptPackage(ctx, spec, false)
	if err != nil {
		return err
	}
	if current.Latest != cfg.Revision || current.Source != cfg.SourceID {
		return errors.New("package source advanced during preparation; update deferred")
	}
	updated, err := s.runtime.Queue.UpdateIdleDefinition(s.runtime.Definitions, spec, definition.Revision)
	if err == nil && updated {
		err = s.catalog.RecordActivity(spec.Actor, "automation.package-update", spec.ID, "succeeded", map[string]string{"revision": cfg.Revision})
	}
	if err != nil {
		return fmt.Errorf("automatic package update: %w", err)
	}
	return nil
}
