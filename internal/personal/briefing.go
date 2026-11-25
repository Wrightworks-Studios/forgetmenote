package personal

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"forgetmenote/internal/automation"
)

func personalCalendarReminder(o Observation, zone string) string {
	var event map[string]any
	if json.Unmarshal(o.Content, &event) != nil {
		return ""
	}
	start := Object(event["start"])
	at, err := time.Parse(time.RFC3339, String(start["dateTime"]))
	if err != nil {
		location, e := time.LoadLocation(zone)
		if e != nil {
			return ""
		}
		at, err = time.ParseInLocation("2006-01-02", String(start["date"]), location)
		if err != nil {
			return ""
		}
		at = at.Add(9 * time.Hour)
	}
	return at.Add(-30 * time.Minute).UTC().Format(time.RFC3339)
}

// A daily digest reuses grounded suggestions. Summarizing their summaries again
// would buy another model call without adding evidence.
func (s *Service) personalBriefing(ctx context.Context, r *automation.Run, state *State) error {
	settings, err := s.catalog.Assistant(r.Spec.Actor)
	if err != nil {
		return err
	}
	if !slices.Contains(settings.Features, "briefing") {
		return nil
	}
	location, err := time.LoadLocation(settings.TimeZone)
	if err != nil {
		return err
	}
	now := time.Now().In(location)
	if now.Format("15:04") < "08:30" {
		return nil
	}
	day := now.Format("2006-01-02")
	key := "briefing:" + day
	if state.Seen[key] != "" {
		return nil
	}
	drafts := []Draft{}
	for _, d := range state.Drafts {
		if d.State == "ready" && d.Source.Kind != "briefing" {
			drafts = append(drafts, d)
		}
	}
	if len(drafts) == 0 {
		return nil
	}
	sort.Slice(drafts, func(i, j int) bool { return drafts[i].ID < drafts[j].ID })
	var summary strings.Builder
	for _, d := range drafts {
		fmt.Fprintf(&summary, "- [%s](%s): %s", strings.ReplaceAll(d.Title, "\n", " "), assistantNoteURL(s.hooks.Origin(), r.Spec.Project, d.Path), strings.ReplaceAll(d.Summary, "\n", " "))
		if d.Due != "" {
			fmt.Fprintf(&summary, " (due %s)", d.Due)
		}
		summary.WriteString("\n")
	}
	content, _ := json.Marshal(map[string]string{"day": day, "summary": summary.String()})
	o := Observation{Key: key, Kind: "briefing", Revision: automation.Hash(content), URL: assistantNoteURL(s.hooks.Origin(), r.Spec.Project, ""), Observed: now.UTC(), Content: content}
	id := personalFingerprint(o)
	state.Seen[key] = o.Revision
	state.SeenAt[key] = now.UTC()
	state.Drafts[id] = Draft{ID: id, Source: o, Path: "briefings/" + day + ".md", Title: "Briefing · " + day, Summary: summary.String(), State: "ready"}
	return nil
}
