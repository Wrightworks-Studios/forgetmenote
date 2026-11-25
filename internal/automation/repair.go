package automation

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"time"
)

// RepairPolicy belongs to a check suite. Evidence calls observe schema/data
// versions, never a clock or request ID; unchanged evidence must not buy AI again.
type RepairPolicy struct {
	CooldownSeconds int    `json:"cooldownSeconds"`
	Evidence        []Call `json:"evidence,omitempty"`
}

type CheckResult struct {
	Name        string          `json:"name"`
	Block       string          `json:"block,omitempty"`
	State       string          `json:"state"`
	Error       string          `json:"error,omitempty"`
	Fingerprint string          `json:"fingerprint,omitempty"`
	Actual      json.RawMessage `json:"actual,omitempty"`
	Reference   json.RawMessage `json:"reference,omitempty"`
}

type CheckReport struct {
	Candidate  []CheckResult `json:"candidate,omitempty"`
	State      string        `json:"state"`
	Summary    string        `json:"summary,omitempty"`
	RecipeHash string        `json:"recipeHash,omitempty"`
	Checks     []CheckResult `json:"checks"`
	Original   []CheckResult `json:"original,omitempty"`
	Repaired   []string      `json:"repaired,omitempty"`
}

// RepairMemo survives run retention. One record per job owns paid-attempt
// suppression and the prior content of its last validated publication.
type RepairMemo struct {
	Version            int         `json:"version"`
	Definition         string      `json:"definition"`
	AttemptFingerprint string      `json:"attemptFingerprint,omitempty"`
	AttemptedAt        time.Time   `json:"attemptedAt,omitempty"`
	RunID              string      `json:"runId,omitempty"`
	Updated            time.Time   `json:"updated"`
	Report             CheckReport `json:"report"`
	Previous           string      `json:"previous,omitempty"`
	PublishedHash      string      `json:"publishedHash,omitempty"`
	EvidenceHash       string      `json:"evidenceHash,omitempty"`
}

func (s Spec) validateRepair() error {
	names := map[string]Check{}
	for _, c := range s.Checks {
		if _, ok := names[c.Name]; ok {
			return errors.New("duplicate check name")
		}
		names[c.Name] = c
		if s.Repair == nil && (len(c.Evidence) > 0 || c.SQLTemplate != "" || len(c.DependsOn) > 0 || s.Kind == "checks" && c.Block != "") {
			return errors.New("SQL blocks, templates and dependencies require repair policy")
		}
	}
	if s.Repair == nil {
		return nil
	}
	if s.Kind != "checks" || s.Output != "" || s.Repair.CooldownSeconds < 60 || s.Repair.CooldownSeconds > 86400 || len(s.Repair.Evidence) > 8 {
		return errors.New("repair requires a check suite, retained run reports and a 60–86400 second cooldown")
	}
	for _, c := range s.Checks {
		if len(c.Evidence) > 4 {
			return errors.New("at most four evidence calls per check")
		}
		for _, call := range c.Evidence {
			if _, err := call.At(time.Now()); err != nil {
				return err
			}
		}
		if c.Block != "" && (s.Recipe == "" || !identifier.MatchString(c.Block) || c.Call.Operation != "query") {
			return errors.New("repairable checks require a recipe, named SQL block and query operation")
		}
		if c.SQLTemplate != "" && (c.Block == "" || strings.Count(c.SQLTemplate, "{{sql}}") != 1 || len(c.SQLTemplate) > 64000) {
			return errors.New("SQL template requires one {{sql}} placeholder and a recipe block")
		}
		for _, name := range c.DependsOn {
			guard, ok := names[name]
			if !ok || guard.Block != "" || name == c.Name || c.Block == "" {
				return errors.New("repair dependencies must name fixed upstream checks in the suite")
			}
		}
	}
	for _, call := range s.Repair.Evidence {
		if _, err := call.At(time.Now()); err != nil {
			return err
		}
	}
	return nil
}

// RepairMemo only reads, so it takes no cross-gateway lock.
func (q *Queue) RepairMemo(spec Spec) (RepairMemo, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	var m RepairMemo
	b, _, err := q.records.read(Hash([]string{spec.ID, "repair"})+".state", 1<<20)
	if errors.Is(err, os.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return m, err
	}
	if json.Unmarshal(b, &m) != nil || m.Version != 1 {
		return m, errors.New("invalid repair checkpoint")
	}
	if m.Definition != Hash(spec) {
		return RepairMemo{}, nil
	}
	return m, nil
}

func (q *Queue) SaveRepairMemo(spec Spec, m RepairMemo) error {
	q.lock()
	defer q.unlock()
	m.Version, m.Definition = 1, Hash(spec)
	// Diagnostic rows follow ordinary run retention. The durable cursor needs
	// only health, identities and the prior note required for safe rollback.
	m.Report.Original = nil
	m.Report.Candidate = nil
	m.Report.Checks = append([]CheckResult(nil), m.Report.Checks...)
	for i := range m.Report.Checks {
		m.Report.Checks[i].Actual = nil
		m.Report.Checks[i].Reference = nil
	}
	b, err := json.Marshal(m)
	if err != nil || len(b) > 1<<20 {
		return errors.New("repair checkpoint exceeds limit")
	}
	return q.records.replace(Hash([]string{spec.ID, "repair"})+".state", b)
}
