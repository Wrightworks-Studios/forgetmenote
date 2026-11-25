package automation

import (
	"encoding/json"
	"errors"
	"io"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"
)

const PackageAPI = "forgetmenote.automation/v1"
const PythonRuntime = "python-v1"

var commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
var capabilities = []string{"checkpoint", "jobstate", "connectors.read", "connectors.submit", "notebooks.read", "notebooks.map", "repositories.changes", "incidents.report", "notebooks.publish", "messages.post", "documents.extract", "web.fetch", "web.search", "ai.structured", "ai.decisions", "ai.images", "decision.evaluate"}

// Script is the administrator's installation grant, not package-supplied authority.
type Script struct {
	RetentionHours int    `json:"retentionHours,omitempty"`
	SourceProject  string `json:"sourceProject"`
	SourceID       string `json:"sourceID"`
	PackagePath    string `json:"packagePath"`
	Revision       string `json:"revision"`
	Digest         string `json:"digest"`
	Runtime        string `json:"runtime"`
	Entrypoint     string `json:"entrypoint"`
	Environment    string `json:"environment,omitempty"`
	// FollowPrimaryBranch opts an instance-managed installation into adopting
	// changed package code with an unchanged contract. Absent (the default) pins
	// the approved digest; a changed digest is then only an available update.
	FollowPrimaryBranch bool              `json:"followPrimaryBranch,omitempty"`
	Capabilities        []string          `json:"capabilities"`
	Connections         map[string]string `json:"connections,omitempty"`
	Notebooks           map[string]string `json:"notebooks,omitempty"`
	Parameters          map[string]any    `json:"parameters,omitempty"`
	// Sandbox confines runs to their package and scratch directory, without
	// sockets, and refuses to run where Linux Landlock is unavailable. Absent,
	// as in definitions saved by earlier versions, means sandboxed: only an
	// explicit false runs package code as unconfined server code.
	Sandbox *bool `json:"sandbox,omitempty"`
}

// Sandboxed reports whether runs use the package sandbox, which is the default
// unless the administrator's grant opts out.
func (s *Script) Sandboxed() bool { return s.Sandbox == nil || *s.Sandbox }

type Manifest struct {
	OptionalCapabilities []string            `json:"optionalCapabilities,omitempty"`
	OptionalConnections  map[string][]string `json:"optionalConnections,omitempty"`
	APIVersion           string              `json:"apiVersion"`
	ID                   string              `json:"id"`
	Name                 string              `json:"name"`
	Entrypoint           string              `json:"entrypoint"`
	Runtime              string              `json:"runtime"`
	Requirements         string              `json:"requirements,omitempty"`
	Files                []string            `json:"files"`
	Capabilities         []string            `json:"capabilities"`
	Connections          map[string][]string `json:"connections,omitempty"`
	Notebooks            map[string]string   `json:"notebooks,omitempty"`
	ParametersSchema     json.RawMessage     `json:"parametersSchema,omitempty"`
}
type ScriptStep struct {
	Operation string          `json:"operation"`
	Input     json.RawMessage `json:"input,omitempty"`
	Hash      string          `json:"hash"`
	State     string          `json:"state"`
	Effect    bool            `json:"effect,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     string          `json:"error,omitempty"`
	Started   time.Time       `json:"started"`
}
type ScriptSource struct {
	Project          string `json:"project"`
	Source           string `json:"source"`
	Revision         string `json:"revision"`
	PreviousRevision string `json:"previousRevision,omitempty"`
	Policy           string `json:"policy"`
}

type ScriptCheckpoint struct {
	Expired         bool                    `json:"expired,omitempty"`
	PreviewJobState *JobState               `json:"previewJobState,omitempty"`
	Sources         map[string]ScriptSource `json:"sources,omitempty"`
	Digest          string                  `json:"digest"`
	Revision        string                  `json:"revision"`
	Environment     string                  `json:"environment"`
	Contract        string                  `json:"contract"`
	Steps           map[string]*ScriptStep  `json:"steps"`
	State           json.RawMessage         `json:"state,omitempty"`
	Complete        bool                    `json:"complete,omitempty"`
}

func (c *ScriptCheckpoint) Uncertain() bool {
	if c == nil {
		return false
	}
	for _, s := range c.Steps {
		if s.Effect && s.State != "completed" {
			return true
		}
	}
	return false
}
func (s *Script) HasExternalEffects() bool {
	return s.UsesAI() || s != nil && (slices.Contains(s.Capabilities, "web.search") || slices.Contains(s.Capabilities, "notebooks.publish") || slices.Contains(s.Capabilities, "connectors.submit") || slices.Contains(s.Capabilities, "messages.post"))
}
func (s *Script) UsesAI() bool {
	return s != nil && (slices.Contains(s.Capabilities, "ai.structured") || slices.Contains(s.Capabilities, "ai.decisions") || slices.Contains(s.Capabilities, "decision.evaluate"))
}
func PackagePath(p string) bool {
	return p != "" && p != "." && !strings.Contains(p, "\\") && !strings.ContainsRune(p, 0) && !strings.HasPrefix(p, "/") && path.Clean(p) == p && p != ".." && !strings.HasPrefix(p, "../")
}
func (s *Script) validate() error {
	if s == nil || s.SourceProject == "" || s.SourceID == "" || !PackagePath(s.PackagePath) || !commitPattern.MatchString(s.Revision) || !digestPattern.MatchString(s.Digest) || s.Runtime != PythonRuntime || !PackagePath(s.Entrypoint) || s.Environment != "" && !digestPattern.MatchString(s.Environment) {
		return errors.New("script requires a source notebook, safe package path, exact commit, content digest and approved Python entrypoint")
	}
	if s.RetentionHours < 0 || s.RetentionHours > 168 {
		return errors.New("private script journal retention must be 1–168 hours, or zero for existing run-history policy")
	}
	if err := validateCapabilities(s.Capabilities); err != nil {
		return err
	}
	for k, v := range s.Connections {
		if !identifier.MatchString(k) || v == "" {
			return errors.New("invalid script connection binding")
		}
	}
	for k, v := range s.Notebooks {
		if !identifier.MatchString(k) || v == "" {
			return errors.New("invalid script notebook binding")
		}
	}
	return nil
}
func validateCapabilities(v []string) error {
	seen := map[string]bool{}
	for _, c := range v {
		if !slices.Contains(capabilities, c) || seen[c] {
			return errors.New("unknown or duplicate script capability: " + c)
		}
		seen[c] = true
	}
	return nil
}
func (m Manifest) Validate() error {
	if m.APIVersion != PackageAPI || !identifier.MatchString(m.ID) || m.Name == "" || m.Runtime != PythonRuntime || !PackagePath(m.Entrypoint) || !strings.HasSuffix(m.Entrypoint, ".py") || !slices.Contains(m.Files, m.Entrypoint) {
		return errors.New("invalid automation manifest or Python entrypoint")
	}
	if err := validateCapabilities(append(slices.Clone(m.Capabilities), m.OptionalCapabilities...)); err != nil {
		return err
	}
	seen := map[string]bool{"automation.json": true}
	for _, f := range m.Files {
		if !PackagePath(f) || seen[f] {
			return errors.New("unsafe or duplicate package file")
		}
		seen[f] = true
	}
	if m.Requirements != "" && (!PackagePath(m.Requirements) || !slices.Contains(m.Files, m.Requirements)) {
		return errors.New("requirements lock must be a declared package file")
	}
	requests := []map[string][]string{m.Connections, m.OptionalConnections}
	for _, request := range requests {
		for k, ops := range request {
			if !identifier.MatchString(k) || len(ops) == 0 {
				return errors.New("invalid requested connection")
			}
			for _, op := range ops {
				if !identifier.MatchString(op) {
					return errors.New("invalid requested operation")
				}
			}
		}
	}
	for k, role := range m.Notebooks {
		if !identifier.MatchString(k) || (role != "read" && role != "edit") {
			return errors.New("invalid requested notebook")
		}
	}
	return nil
}
func ParseManifest(raw []byte) (Manifest, error) {
	var m Manifest
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.DisallowUnknownFields()
	if e := d.Decode(&m); e != nil {
		return m, e
	}
	if e := d.Decode(new(any)); e != io.EOF {
		return m, errors.New("manifest must contain exactly one JSON object")
	}
	return m, m.Validate()
}

// PackageDigest binds every file and its relative name, independent of map order.
func PackageDigest(files map[string]string) string { return Hash(files) }

// Optional requests permit one package to run without write or AI grants. Only
// explicit administrator installation grants activate them.
func (m Manifest) RequestsCapability(cap string) bool {
	return slices.Contains(m.Capabilities, cap) || slices.Contains(m.OptionalCapabilities, cap)
}
func (m Manifest) RequestsOperation(alias, op string) bool {
	return slices.Contains(m.Connections[alias], op) || slices.Contains(m.OptionalConnections[alias], op)
}
