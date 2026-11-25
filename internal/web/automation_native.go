package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"forgetmenote/internal/automation"
	"forgetmenote/internal/knowledge"
	"forgetmenote/internal/workspace"
)

func (s *Server) runNativeAutomation(ctx context.Context, r *automation.Run, save func() error) error {
	n := r.Spec.Native
	if err := r.Spec.Validate(); err != nil {
		return err
	}
	store, err := s.clientAccess(ctx, r.Spec.Actor, r.Spec.Project, "read")
	if err != nil {
		return err
	}
	data := map[string]any{}
	incident := automation.Incident{Severity: "warning"}
	switch n.Task {
	case "storage-health":
		var stat syscall.Statfs_t
		if err := syscall.Statfs(s.data, &stat); err != nil {
			return errors.New("cannot inspect instance storage")
		}
		if stat.Blocks == 0 {
			return errors.New("storage capacity unavailable")
		}
		used := 100 * (1 - float64(stat.Bavail)/float64(stat.Blocks))
		warning, critical := n.WarningPercent, n.CriticalPercent
		if warning == 0 {
			warning = 70
		}
		if critical == 0 {
			critical = 90
		}
		data = map[string]any{"usedPercent": used, "availableBytes": uint64(stat.Bavail) * uint64(stat.Bsize)}
		incident.Active = used >= warning
		if used >= critical {
			incident.Severity = "critical"
		}
		incident.Summary = fmt.Sprintf("Instance storage %.1f%% used", used)
	case "notebook-health":
		status, e := store.Status(ctx)
		if e != nil {
			return e
		}
		data["revision"] = status.Revision
		data["dirty"] = status.Dirty
		incident.Active = status.Dirty
		if s.repositories != nil && s.catalog.Kind(r.Spec.Project) == "repository" {
			state, e := s.repositories.Status(r.Spec.Project)
			if e != nil {
				return e
			}
			data["repository"] = state
			incident.Active = incident.Active || state.Error != "" || state.Failures > 0
		} else {
			pending, e := withEngine(s, ctx, store, func(engine *knowledge.Engine) (knowledge.Pending, error) {
				return engine.PendingScope(ctx, 1, "")
			})
			if e != nil {
				return e
			}
			data["index"] = pending.Status
		}
		if s.backups != nil {
			b, e := s.backups.manager.Status(r.Spec.Project)
			if e != nil {
				return e
			}
			data["backup"] = map[string]any{"connected": b.Connected, "enabled": b.Enabled, "lastSuccess": b.LastSuccess, "revision": b.LastRevision, "blocked": b.Blocked, "error": b.Error}
			incident.Active = incident.Active || b.Blocked || b.Error != ""
			if b.Enabled && status.Revision != b.LastRevision && time.Since(b.LastSuccess) > time.Duration(max(60, n.MaxAgeMinutes))*time.Minute {
				incident.Active = true
			}
		}
		incident.Summary = "Notebook health: " + r.Spec.Project
	case "automation-health", "profile-health":
		runs, e := s.automations.Queue.List()
		if e != nil {
			return e
		}
		latest := map[string]automation.Run{}
		pending := map[string]string{}
		for _, run := range runs {
			if run.Spec.Project != r.Spec.Project || run.Spec.ID == r.Spec.ID {
				continue
			}
			if n.Task == "profile-health" && run.Spec.Kind != "learning" && run.Spec.Kind != "recipe" && run.Spec.Kind != "checks" {
				continue
			}
			if run.DryRun {
				continue
			}
			if run.State == "queued" || run.State == "running" {
				pending[run.Spec.ID] = run.State
				if time.Since(run.Created) > time.Duration(max(60, n.MaxAgeMinutes))*time.Minute {
					incident.Active = true
				}
				continue
			}
			if old := latest[run.Spec.ID]; old.Updated.Before(run.Updated) {
				latest[run.Spec.ID] = run
			}
		}
		states := map[string]string{}
		for job, run := range latest {
			states[job] = run.State
			if run.State == "failed" || run.State == "action-required" {
				incident.Active = true
			}
		}
		data["jobs"] = states
		data["pending"] = pending
		data["retainedRuns"] = len(runs)
		incident.Summary = "Automation health: " + r.Spec.Project
	case "activity-summary":
		data, err = s.catalog.AutomationActivity(r.Spec.Actor, r.Spec.Project, r.Spec.Connections, time.Now().Add(-time.Duration(max(1, n.LookbackHours))*time.Hour))
		if err != nil {
			return err
		}
	case "repository-refresh":
		repos, e := s.repos()
		if e != nil {
			return e
		}
		if _, err = s.clientAccess(ctx, r.Spec.Actor, r.Spec.Project, "owner"); err != nil {
			return err
		}
		state, e := repos.Status(r.Spec.Project)
		if e != nil {
			return e
		}
		if !state.Connected {
			return errors.New("notebook has no connected repository")
		}
		if !r.DryRun {
			if err = s.checkAutomationAuth(ctx); err != nil {
				return err
			}
			state, e = repos.Refresh(ctx, r.Spec.Project)
			if e != nil {
				return e
			}
		}
		data["repository"] = state
	case "email-pairing":
		if data, incident, err = s.runEmailPairing(ctx, r); err != nil {
			return err
		}
	case "task-reminder":
		if data, incident, err = s.runTaskReminder(ctx, r); err != nil {
			return err
		}
	case "profile-compaction":
		count, e := s.compactAutomationProfiles(ctx, r)
		if e != nil {
			return e
		}
		data["exactDuplicates"] = count
	}
	raw, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	report := "# " + r.Spec.ID + "\n\n```json\n" + string(raw) + "\n```\n"
	if len(report) > 256<<10 {
		return errors.New("native report exceeds limit")
	}
	r.Result = report
	if incident.Summary != "" {
		decision, e := s.automations.Queue.Incident(r.Spec, r.ID, incident, time.Now(), r.DryRun)
		if e != nil {
			return e
		}
		r.Alert = &decision
		if decision.Notify && !r.DryRun {
			r.Answer = incident.Summary + "\n\n" + report
			if decision.Recovered {
				r.Answer = "Recovered: " + r.Answer
			}
			if e = save(); e != nil {
				return e
			}
			return s.notifyAutomation(ctx, r, save)
		}
		return save()
	}
	return s.automationReport(ctx, r, report, save)
}

func (s *Server) automationReport(ctx context.Context, r *automation.Run, report string, save func() error) error {
	r.Result = report
	if r.Spec.Output != "" {
		if r.Publication == nil {
			return errors.New("report has no pinned output revision")
		}
		r.Publication.Content = report
		if err := save(); err != nil {
			return err
		}
		if err := s.publishAutomation(ctx, r); err != nil {
			return err
		}
	} else {
		r.Answer = report
		if err := save(); err != nil {
			return err
		}
	}
	return s.notifyAutomation(ctx, r, save)
}

func (s *Server) compactAutomationProfiles(ctx context.Context, r *automation.Run) (int, error) {
	store, err := s.clientAccess(ctx, r.Spec.Actor, r.Spec.Project, "edit")
	if err != nil {
		return 0, err
	}
	scope, err := s.documentContentScope(ctx, store, knowledge.ContainerSelection{NotesOnly: true})
	if err != nil {
		return 0, err
	}
	snapshot, err := store.SnapshotScoped(ctx, "", scope)
	if err != nil {
		return 0, err
	}
	if len(snapshot.Files) > 256 {
		return 0, errors.New("compaction scope exceeds 256 notes")
	}
	sort.Slice(snapshot.Files, func(i, j int) bool { return snapshot.Files[i].Path < snapshot.Files[j].Path })
	allowed := []string{}
	expected := []workspace.MutationSource{}
	seen := map[string]workspace.SnapshotFile{}
	count := 0
	for _, file := range snapshot.Files {
		allowed = append(allowed, file.Path)
		expected = append(expected, workspace.MutationSource{Path: file.Path, Revision: snapshot.Revision, Hash: file.Hash})
	}
	for _, file := range snapshot.Files {
		if !strings.HasPrefix(file.Path, r.Spec.Native.Folder+"/") || !slices.Contains(file.Metadata.Tags, "recipe") {
			continue
		}
		if old, ok := seen[file.Hash]; ok {
			action := workspace.OrganizationAction{ID: "automation-" + r.ID + "-" + automation.Hash(file.Path)[:16], Kind: "duplicate", Path: file.Path, Target: old.Path, Allowed: allowed, Expected: expected, Sources: []workspace.MutationSource{{Path: file.Path, Revision: snapshot.Revision, Hash: file.Hash}, {Path: old.Path, Revision: snapshot.Revision, Hash: old.Hash}}}
			if err = s.checkAutomationAuth(ctx); err != nil {
				return count, err
			}
			a := ctx.Value(automationKey{}).(automationAuth)
			err = s.catalog.WithProjectAuthority(a.Spec.Actor, a.Spec.Project, a.Account, "edit", a.Epoch, a.ProjectEpoch, func(st *workspace.Store) error { _, e := st.Organize(ctx, action, !r.DryRun); return e })
			if err != nil {
				return count, err
			}
			count++
			// Each successful operation rewrites links and revisions. Rescan on the
			// next scheduled pass rather than operating on stale dependencies.
			return count, nil
		}
		seen[file.Hash] = file
	}
	return count, nil
}

func (s *Server) runAutomationChecks(ctx context.Context, r *automation.Run, save func() error) error {
	checks, err := s.evaluateChecks(ctx, r, nil)
	r.CheckReport = &automation.CheckReport{State: "healthy", Checks: checks}
	if err != nil {
		r.CheckReport.State = "failed"
		r.Result = checkReportText(r.CheckReport) + "\n" + receiptSummary(r.Receipts)
		if e := save(); e != nil {
			return e
		}
		return err
	}
	return s.automationReport(ctx, r, checkReportText(r.CheckReport)+"\n"+receiptSummary(r.Receipts), save)
}

func checkAutomationAssertions(check automation.Check, data any, duration int64) error {
	for _, a := range check.Assertions {
		v, ok := jsonPointer(data, a.Pointer)
		var expected any
		if json.Unmarshal(a.Equals, &expected) != nil || !ok || automation.Hash(v) != automation.Hash(expected) {
			return recipeFailure{fmt.Errorf("check %s failed at %s", check.Name, a.Pointer)}
		}
	}
	if check.MaxDurationMS > 0 && duration > check.MaxDurationMS {
		return recipeFailure{fmt.Errorf("check %s exceeded %d ms", check.Name, check.MaxDurationMS)}
	}
	return nil
}
