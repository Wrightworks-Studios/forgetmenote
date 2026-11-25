package catalog

import (
	"slices"
	"time"

	"forgetmenote/internal/workspace"
)

// AutomationActivity returns aggregate metadata only, scoped to the notebook
// and the service account's current operation grants. Inputs and people are not
// copied into a digest, and retained coverage is explicit.
func (c *Catalog) AutomationActivity(actor, project string, connections []string, since time.Time) (map[string]any, error) {
	if _, err := c.Access(actor, project, "read"); err != nil {
		return nil, err
	}
	allowed := map[string]bool{}
	for _, id := range connections {
		if !c.ConnectionAllowed(actor, id, "") {
			return nil, workspace.ErrForbidden
		}
		allowed[id] = true
	}
	c.lock()
	events := slices.Clone(c.activity)
	c.mu.Unlock()
	counts := map[string]int{}
	total := 0
	oldest := time.Time{}
	for _, e := range events {
		if oldest.IsZero() || e.Time.Before(oldest) {
			oldest = e.Time
		}
		if e.Time.Before(since) || e.Outcome == "pending" {
			continue
		}
		if e.Action == "data.call" {
			if !allowed[e.Target] || !c.ConnectionAllowed(actor, e.Target, e.Details["operation"]) {
				continue
			}
		} else if e.Target != project {
			continue
		}
		key := e.Action + " / " + e.Outcome
		if e.Action == "data.call" {
			key += " / " + e.Target + "." + e.Details["operation"]
		}
		counts[key]++
		total++
	}
	return map[string]any{"counts": counts, "total": total, "since": since, "retainedSince": oldest, "partial": len(events) >= activityLimit && oldest.After(since)}, nil
}
