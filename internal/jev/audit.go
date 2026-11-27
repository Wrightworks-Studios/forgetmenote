package jev

import (
	"forgetmenote/internal/auth"
	"forgetmenote/internal/knowledge"
	"sort"
)

// Missing coverage is meaningful only after every explicitly selected
// destination passage has been compared. Candidate relevance orders work but
// never drops destinations. Results merge per original source passage.
func jevJobResults(job jevJob) []jevJobResult {
	if job.Kind != "audit" {
		return job.Results
	}
	expected := map[string]int{}
	for _, work := range job.Work {
		if work.Kind == "audit" && len(work.Passages) > 0 {
			expected[work.Passages[0]]++
		}
	}
	groups := map[string][]jevJobResult{}
	order := []string{}
	for _, result := range job.Results {
		if result.SourceID == "" {
			continue
		}
		if groups[result.SourceID] == nil {
			order = append(order, result.SourceID)
		}
		groups[result.SourceID] = append(groups[result.SourceID], result)
	}
	out := []jevJobResult{}
	for _, id := range order {
		parts := groups[id]
		result := parts[0]
		result.ID = auth.Digest(job.ID + "\x00" + id)
		result.Complete = len(parts) == expected[id]
		result.Compared = 0
		result.Evidence = []knowledge.JudgmentEvidence{}
		statuses := map[string]int{}
		seen := map[string]bool{}
		for _, part := range parts {
			statuses[part.Status]++
			result.Compared += part.Compared
		}
		// Show useful support before nonmatching excerpts. Coverage retains every
		// selected destination; the browser receives at most eight original excerpts.
		ordered := append([]jevJobResult(nil), parts...)
		sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Status != "missing" && ordered[j].Status == "missing" })
		for _, part := range ordered {
			for _, evidence := range part.Evidence {
				key := evidence.Path + "\x00" + evidence.Revision + "\x00" + evidence.Text
				if !seen[key] {
					if len(result.Evidence) < 8 {
						result.Evidence = append(result.Evidence, evidence)
					}
					seen[key] = true
				}
			}
		}
		switch {
		case !result.Complete:
			result.Status = "unchecked"
		case statuses["contradicted"] > 0 && (statuses["preserved"] > 0 || statuses["superseded"] > 0):
			result.Status = "uncertain"
		case statuses["contradicted"] > 0:
			result.Status = "contradicted"
		case statuses["changed"] > 0:
			result.Status = "changed"
		case statuses["superseded"] > 0:
			result.Status = "superseded"
		case statuses["preserved"] > 0:
			result.Status = "preserved"
		case statuses["uncertain"] > 0 || statuses["partial"] > 1:
			result.Status = "uncertain"
		case statuses["partial"] > 0:
			result.Status = "partial"
		default:
			result.Status = "missing"
		}
		result.Title = "Extraction: " + result.Status
		result.Summary = "Coverage of this source passage across the explicitly selected destination passages. Distributed partial coverage or conflicting judgments remains uncertain; inspect the original evidence."
		result.Details = map[string]any{"completedBatches": len(parts), "totalBatches": expected[id], "outcomes": statuses, "destinationPassages": result.Compared, "evidenceShown": len(result.Evidence), "evidenceTotal": len(seen)}
		out = append(out, result)
	}
	return out
}
