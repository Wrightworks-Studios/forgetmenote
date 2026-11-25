package web

import (
	"time"

	"forgetmenote/internal/workspace"
)

type recipeCheckHealth struct {
	State     string            `json:"state"`
	CheckedAt string            `json:"checkedAt,omitempty"`
	Checks    map[string]string `json:"checks,omitempty"`
}

type recipeEntry struct {
	workspace.Entry
	Health *recipeCheckHealth `json:"health,omitempty"`
}

// Called only after notebook/content authorization. Readers see states, never
// another connector's rows, error bodies or the private repair prompt.
func (s *Server) recipeHealth(project, path, hash string) (*recipeCheckHealth, error) {
	if s.automations == nil {
		return nil, nil
	}
	var health *recipeCheckHealth
	for _, d := range s.automations.Definitions.List() {
		spec := d.Spec
		if spec.Repair == nil || spec.Recipe != path || spec.Project != project {
			continue
		}
		memo, err := s.automations.Queue.RepairMemo(spec)
		if err != nil {
			return nil, err
		}
		if health == nil {
			health = &recipeCheckHealth{State: "healthy", Checks: map[string]string{}}
		}
		stale := spec.Disabled || memo.Updated.IsZero() || memo.Report.RecipeHash != hash
		if spec.IntervalSeconds > 0 && time.Since(memo.Updated) > time.Duration(spec.IntervalSeconds*2)*time.Second {
			stale = true
		}
		state := memo.Report.State
		if stale {
			state = "unverified"
		}
		if state != "healthy" && state != "repaired" && state != "rolled_back" {
			health.State = state
		}
		if !memo.Updated.IsZero() {
			health.CheckedAt = memo.Updated.UTC().Format(time.RFC3339)
		}
		for _, c := range memo.Report.Checks {
			value := c.State
			if stale {
				value = "unverified"
			}
			health.Checks[spec.ID+"/"+c.Name] = value
		}
	}
	return health, nil
}
