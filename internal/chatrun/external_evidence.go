package chatrun

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"forgetmenote/internal/chat"
	"forgetmenote/internal/sources"
)

// A review can finish the current explicitly bounded allowance using the dated
// originals it fetched. Resume/new turns cannot silently certify web freshness.
func reviewExternalAllowance(run *chat.Run) string {
	return fmt.Sprintf("%s:%d", run.ID, run.ReviewAllowance)
}
func externalCapture(c chat.Citation) chat.ExternalCapture {
	return chat.ExternalCapture{URL: c.URL, CapturedAt: c.CapturedAt, Hash: c.Hash}
}
func validExternalCapture(c chat.ExternalCapture) bool {
	hash, err := hex.DecodeString(c.Hash)
	_, dateErr := time.Parse(time.RFC3339Nano, c.CapturedAt)
	return err == nil && len(hash) == 32 && dateErr == nil && sources.ValidateURL(c.URL) == nil
}
func inheritExternalEvidence(target *chat.Provenance, source chat.Provenance) {
	if source.ExternalAllowance == "" && len(source.ExternalSources) == 0 {
		return
	}
	if source.ExternalAllowance == "" || target.ExternalAllowance != "" && target.ExternalAllowance != source.ExternalAllowance {
		target.Complete = false
		return
	}
	target.ExternalAllowance = source.ExternalAllowance
	for _, capture := range source.ExternalSources {
		if !validExternalCapture(capture) {
			target.Complete = false
			continue
		}
		found := false
		for _, old := range target.ExternalSources {
			if old == capture {
				found = true
				break
			}
		}
		if found {
			continue
		}
		candidate := append(append([]chat.ExternalCapture(nil), target.ExternalSources...), capture)
		raw, _ := json.Marshal(candidate)
		if len(candidate) > 32 || len(raw) > 16<<10 {
			target.Complete = false
			continue
		}
		target.ExternalSources = candidate
	}
}
func reviewExternalEvidenceValid(p chat.Provenance, citations []chat.Citation, allowance string) bool {
	if p.ExternalAllowance != "" && p.ExternalAllowance != allowance || len(p.ExternalSources) > 0 && p.ExternalAllowance == "" || len(p.ExternalSources) > 32 {
		return false
	}
	raw, _ := json.Marshal(p.ExternalSources)
	if len(raw) > 16<<10 {
		return false
	}
	for _, capture := range p.ExternalSources {
		if !validExternalCapture(capture) {
			return false
		}
	}
	for _, citation := range citations {
		if citation.Kind != "external" {
			continue
		}
		if citation.Path != "" || citation.Revision != "" {
			return false
		}
		found := false
		capture := externalCapture(citation)
		for _, saved := range p.ExternalSources {
			if saved == capture {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
