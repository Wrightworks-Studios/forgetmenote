package web

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"forgetmenote/internal/automation"
	"forgetmenote/internal/chatrun"
	"forgetmenote/internal/knowledge"
	"forgetmenote/internal/workspace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type automationJobStatus struct {
	ID              string          `json:"id"`
	Kind            string          `json:"kind"`
	Actor           string          `json:"actor"`
	Project         string          `json:"project"`
	IntervalSeconds int             `json:"intervalSeconds"`
	Spec            automation.Spec `json:"spec"`
	Revision        string          `json:"revision"`
	ManagedBy       string          `json:"managedBy"`
	NextRun         string          `json:"nextRun,omitempty"`
}
type automationRunStatus struct {
	DryRun       bool                    `json:"dryRun,omitempty"`
	Checks       *checkRunStatus         `json:"checks,omitempty"`
	ID           string                  `json:"id"`
	Job          string                  `json:"job"`
	Project      string                  `json:"project"`
	State        string                  `json:"state"`
	Updated      string                  `json:"updated"`
	Error        string                  `json:"error,omitempty"`
	Attempts     int                     `json:"attempts"`
	InputTokens  int                     `json:"inputTokens"`
	OutputTokens int                     `json:"outputTokens"`
	Receipts     int                     `json:"receipts"`
	Observation  *automation.Observation `json:"observation,omitempty"`
	Delivery     string                  `json:"delivery,omitempty"`
	Learning     *learningRunStatus      `json:"learning,omitempty"`
}
type learningRunStatus struct {
	State        string   `json:"state"`
	Summary      string   `json:"summary"`
	Observations int      `json:"observations"`
	Patterns     int      `json:"patterns"`
	Deferred     int      `json:"deferred"`
	Published    []string `json:"published"`
	ReadyAt      string   `json:"readyAt"`
	Expires      string   `json:"expires"`
}

type checkRunStatus struct {
	State    string   `json:"state"`
	Summary  string   `json:"summary,omitempty"`
	Passed   int      `json:"passed"`
	Failed   int      `json:"failed"`
	Repaired []string `json:"repaired,omitempty"`
}

func runStatus(r automation.Run) automationRunStatus {
	status := automationRunStatus{ID: r.ID, Job: r.Spec.ID, Project: r.Spec.Project, State: r.State, Updated: r.Updated.Format("2006-01-02T15:04:05Z07:00"), Error: r.Error, Attempts: r.Attempts, InputTokens: r.InputTokens, OutputTokens: r.OutputTokens, Receipts: len(r.Receipts), Observation: r.Observation, Delivery: r.DeliveryState}
	status.DryRun = r.DryRun
	if c := r.CheckReport; c != nil {
		status.Checks = &checkRunStatus{State: c.State, Summary: c.Summary, Repaired: c.Repaired}
		for _, check := range c.Checks {
			if check.State == "passed" {
				status.Checks.Passed++
			} else {
				status.Checks.Failed++
			}
		}
	}
	if b := r.Learning; b != nil {
		patterns := map[string]bool{}
		for _, e := range b.Events {
			patterns[learningGroup(e)] = true
		}
		learning := &learningRunStatus{State: r.State, Observations: len(b.Events), Patterns: len(patterns), Deferred: b.Deferred, Published: b.Published, ReadyAt: b.ReadyAt.Format(time.RFC3339), Expires: b.Expires.Format(time.RFC3339)}
		if r.State == "completed" {
			switch {
			case len(b.Published) > 0:
				learning.State = "published"
				learning.Summary = "Profiles published."
			case b.Deferred > 0:
				learning.State = "deferred"
				learning.Summary = "No profile published; evidence or validation was insufficient."
			case r.DryRun:
				learning.State = "previewed"
				learning.Summary = "Preview finished; publication was suppressed."
			default:
				learning.State = "unchanged"
				learning.Summary = "No profile changes."
			}
		}
		if b.Summary != "" {
			learning.Summary = chatrun.TextLimit(strings.TrimSpace(b.Summary), 1400)
		}
		status.Learning = learning
	}
	return status
}
func (s *Server) automationTools(server *mcp.Server) {
	s.packageTools(server)
	addManagementTool(s, server, "test_automation", "Run a configured automation with publication and delivery suppressed. Reads and model calls still execute.", false, func(ctx context.Context, in struct {
		ID          string `json:"id"`
		RequestID   string `json:"requestId"`
		EvidenceRun string `json:"evidenceRun,omitempty"`
	}) (automationRunStatus, error) {
		if !s.catalog.IsAdmin(subject(ctx)) {
			return automationRunStatus{}, workspace.ErrForbidden
		}
		if err := s.outgoing.Check(); err != nil {
			return automationRunStatus{}, err
		}
		d, ok := s.automations.Definitions.Get(in.ID)
		if !ok || in.RequestID == "" || len(in.RequestID) > 128 {
			return automationRunStatus{}, errors.New("job and request ID required")
		}
		var r automation.Run
		var err error
		if d.Spec.Kind == "learning" || d.Spec.Trigger != nil {
			r, err = s.automations.Queue.PreviewEvidence(d.Spec, in.RequestID, in.EvidenceRun)
		} else {
			r, err = s.automations.Queue.EnqueuePreview(d.Spec, "preview-"+in.RequestID)
		}
		return runStatus(r), err
	})
	addManagementTool(s, server, "automation_run", "Inspect one run as an administrator, including retained test output and execution receipts.", true, func(ctx context.Context, in struct {
		ID string `json:"id"`
	}) (map[string]any, error) {
		if !s.catalog.IsAdmin(subject(ctx)) {
			return nil, workspace.ErrForbidden
		}
		r, err := s.automations.Queue.Get(in.ID)
		if err != nil {
			return nil, err
		}
		if r.Spec.Kind == "assistant" {
			return map[string]any{"status": runStatus(r), "spec": map[string]string{"id": r.Spec.ID, "kind": "assistant", "project": r.Spec.Project}, "privateConversation": true}, nil
		}
		return map[string]any{"status": runStatus(r), "result": r.Result, "receipts": r.Receipts, "citations": r.Citations, "spec": r.Spec, "dryRun": r.DryRun, "retryErrors": r.RetryErrors, "retryAt": r.RetryAt, "alert": r.Alert, "checks": r.CheckReport, "script": r.Script}, nil
	})
	addManagementTool(s, server, "save_automation", "Create or update an instance automation as an administrator. Supply the current revision for updates; mounted definitions are read-only.", false, func(ctx context.Context, in struct {
		Spec     automation.Spec `json:"spec"`
		Revision string          `json:"revision"`
	}) (automation.Definition, error) {
		if !s.catalog.IsAdmin(subject(ctx)) {
			return automation.Definition{}, workspace.ErrForbidden
		}
		if in.Spec.Script != nil {
			if in.Spec.Script.Environment == "" {
				return automation.Definition{}, errors.New("prepare or install this package before saving its definition")
			}
			if _, _, err := s.scriptPackage(ctx, in.Spec, true); err != nil {
				return automation.Definition{}, err
			}
		}
		if err := s.validateAutomationDefinition(ctx, in.Spec); err != nil {
			return automation.Definition{}, err
		}
		result, err := s.automations.Definitions.Save(in.Spec, in.Revision)
		if err == nil {
			err = s.automations.Queue.CancelSuperseded(result.Spec.ID, automation.Hash(result.Spec))
		}
		if err == nil {
			err = s.catalog.RecordActivity(subject(ctx), "automation.save", in.Spec.ID, "succeeded", nil)
		}
		return result, err
	})
	addManagementTool(s, server, "delete_automation", "Delete an instance automation definition with its expected revision. Run history remains available.", false, func(ctx context.Context, in struct {
		ID       string `json:"id"`
		Revision string `json:"revision"`
	}) (actionResult, error) {
		if !s.catalog.IsAdmin(subject(ctx)) {
			return actionResult{}, workspace.ErrForbidden
		}
		err := s.automations.Definitions.Delete(in.ID, in.Revision)
		if err == nil {
			err = s.automations.Queue.CancelSuperseded(in.ID, "")
		}
		if err == nil {
			err = s.catalog.RecordActivity(subject(ctx), "automation.delete", in.ID, "succeeded", nil)
		}
		return actionResult{err == nil}, err
	})
	addManagementTool(s, server, "validate_automation", "Validate an automation and preview its configured reads, output and model use without executing it.", true, func(ctx context.Context, in struct {
		Spec automation.Spec `json:"spec"`
	}) (map[string]any, error) {
		if !s.catalog.IsAdmin(subject(ctx)) {
			return nil, workspace.ErrForbidden
		}
		if err := s.validateAutomationDefinition(ctx, in.Spec); err != nil {
			return nil, err
		}
		return map[string]any{"valid": true, "actor": in.Spec.Actor, "project": in.Spec.Project, "connections": in.Spec.Connections, "calls": in.Spec.Calls, "output": in.Spec.Output, "recipe": in.Spec.Recipe, "aiConnection": in.Spec.AIConnection}, nil
	})
	addManagementTool(s, server, "automation_status", "Inspect definitions, the 100 newest runs and every older run that still blocks its job, as an instance administrator. Run results require automation_run.", true, func(ctx context.Context, in struct{}) (map[string]any, error) {
		if !s.catalog.IsAdmin(subject(ctx)) {
			return nil, workspace.ErrForbidden
		}
		jobs := []automationJobStatus{}
		runs := []automationRunStatus{}
		if s.automations != nil {
			for _, definition := range s.automations.Definitions.List() {
				spec := definition.Spec
				jobs = append(jobs, automationJobStatus{NextRun: spec.Next(time.Now()), ID: spec.ID, Kind: spec.Kind, Actor: spec.Actor, Project: spec.Project, IntervalSeconds: spec.IntervalSeconds, Spec: spec, Revision: definition.Revision, ManagedBy: definition.ManagedBy})
			}
			items, e := s.automations.Queue.List()
			if e != nil {
				return nil, e
			}
			// Recent history is capped, but a run that still blocks its job
			// (queued, running or awaiting reconciliation) stays listed however
			// old it is: the scheduler skips that job until an administrator
			// cancels the run here.
			for i := len(items) - 1; i >= 0; i-- {
				if len(items)-i <= 100 || items[i].Unresolved() {
					runs = append(runs, runStatus(items[i]))
				}
			}
		}
		slices.SortFunc(jobs, func(a, b automationJobStatus) int { return strings.Compare(a.ID, b.ID) })
		return map[string]any{"jobs": jobs, "runs": runs, "slackConfigured": s.slack != nil, "pythonConfigured": s.python != nil}, nil
	})
	addManagementTool(s, server, "run_automation", "Queue a configured job as an instance administrator. Supply a stable requestId to make retries idempotent.", false, func(ctx context.Context, in struct {
		ID        string `json:"id"`
		RequestID string `json:"requestId"`
	}) (automationRunStatus, error) {
		if !s.catalog.IsAdmin(subject(ctx)) || s.automations == nil {
			return automationRunStatus{}, workspace.ErrForbidden
		}
		if err := s.outgoing.Check(); err != nil {
			return automationRunStatus{}, err
		}
		definition, ok := s.automations.Definitions.Get(in.ID)
		if !ok || definition.Spec.Disabled || in.RequestID == "" || len(in.RequestID) > 128 {
			return automationRunStatus{}, errors.New("job ID and requestId required")
		}
		if definition.Spec.Trigger != nil || definition.Spec.Kind == "learning" {
			return automationRunStatus{}, errors.New("event jobs require captured input; test with an evidence run or wait for the next event")
		}
		r, e := s.automations.Queue.Enqueue(definition.Spec, "manual-"+in.RequestID)
		if e == nil {
			e = s.catalog.RecordActivity(subject(ctx), "automation.enqueue", r.ID, "succeeded", nil)
		}
		return runStatus(r), e
	})
	addManagementTool(s, server, "cancel_automation", "Cancel one queued or active run as an instance administrator.", false, func(ctx context.Context, in struct {
		ID string `json:"id"`
	}) (actionResult, error) {
		if !s.catalog.IsAdmin(subject(ctx)) || s.automations == nil {
			return actionResult{}, workspace.ErrForbidden
		}
		e := s.automations.Queue.Cancel(in.ID)
		return actionResult{e == nil}, e
	})
	addProjectTool(s, server, "find_recipes", "Find Markdown recipe notes tagged recipe in an approved notebook. Query is a literal substring: use a short phrase or omit it to list recipes. Read a recipe and current operation schemas before executing its instructions.", "read", func(ctx context.Context, store *workspace.Store, in struct {
		projectArg
		Query string `json:"query,omitempty"`
	}) ([]recipeEntry, error) {
		ctx, err := s.chatrun.DocumentScope(ctx, in.Project, "notes", "", "", "")
		if err != nil {
			return nil, err
		}
		scope, err := s.documentContentScope(ctx, store, knowledge.ContainerSelection{NotesOnly: true})
		if err != nil {
			return nil, err
		}
		found, err := store.FindScoped(ctx, in.Query, "recipe", "", "", false, 100, scope)
		if err != nil {
			return nil, err
		}
		entries := make([]recipeEntry, 0, len(found.Entries))
		for _, entry := range found.Entries {
			doc, err := store.Read(ctx, entry.Path, "")
			if err != nil {
				return nil, err
			}
			health, err := s.recipeHealth(in.Project, entry.Path, workspace.ContentHash(doc.Content))
			if err != nil {
				return nil, err
			}
			entries = append(entries, recipeEntry{entry, health})
		}
		return entries, nil
	})
}

func (s *Server) validateAutomationDefinition(ctx context.Context, spec automation.Spec) error {
	if err := spec.ValidateConfigured(); err != nil {
		return err
	}
	if !s.catalog.IsService(spec.Actor) {
		return errors.New("select an active service account")
	}
	role := "read"
	if spec.Output != "" || spec.Repair != nil && spec.Recipe != "" || spec.Kind == "recipe" || spec.Kind == "learning" || spec.Kind == "drive-sync" || spec.Native != nil && spec.Native.Task == "profile-compaction" {
		role = "edit"
	}
	if spec.Native != nil && spec.Native.Task == "repository-refresh" {
		role = "owner"
	}
	if spec.Native != nil && spec.Native.Task == "email-pairing" {
		if _, _, err := s.emailPairing(spec); err != nil {
			return err
		}
	}
	if spec.Native != nil && spec.Native.Task == "task-reminder" {
		if _, err := s.tasks.Reminder(spec); err != nil {
			return err
		}
	}
	if _, err := s.catalog.Access(spec.Actor, spec.Project, role); err != nil {
		return err
	}
	for _, project := range spec.ReadProjects {
		if _, err := s.catalog.Access(spec.Actor, project, "read"); err != nil {
			return err
		}
	}
	if spec.Script != nil {
		_, files, err := s.scriptPackage(ctx, spec, false)
		if err != nil {
			return err
		}
		if s.python == nil {
			return errors.New("configure AUTOMATION_PYTHON before installing packages")
		}
		// A prepared definition must also prepare on this process and Python.
		if spec.Script.Environment != "" {
			prepareCtx, cancel := context.WithTimeout(ctx, time.Duration(spec.TimeoutSeconds)*time.Second)
			_, err = s.python.Ensure(prepareCtx, files, spec.Script.Digest)
			cancel()
			if err != nil {
				return err
			}
		}
	}
	// Disabled drafts may be prepared before an operator mounts their connections.
	if spec.Disabled {
		return nil
	}
	for _, id := range spec.Connections {
		if _, ok := s.dataConnections.Info(id); !ok {
			return errors.New("automation connection is not configured")
		}
	}
	calls := append([]automation.Call{}, spec.Calls...)
	if spec.Repair != nil {
		calls = append(calls, spec.Repair.Evidence...)
	}
	for _, check := range spec.Checks {
		calls = append(calls, check.Call)
		calls = append(calls, check.Evidence...)
		if check.Reference != nil {
			calls = append(calls, *check.Reference)
		}
	}
	if spec.Drive != nil {
		for _, op := range []string{"list", "download", "export"} {
			calls = append(calls, automation.Call{Connection: spec.Drive.Connection, Operation: op})
		}
	}

	for _, call := range calls {
		if !slices.Contains(spec.Connections, call.Connection) || !s.catalog.ConnectionAllowed(spec.Actor, call.Connection, call.Operation) || spec.Operations != nil && !slices.Contains(spec.Operations[call.Connection], call.Operation) {
			return workspace.ErrForbidden
		}
		if op, ok := s.dataConnections.ConfiguredOperation(call.Connection, call.Operation); !ok || !op.ReadOnly {
			return errors.New("automation requires a configured read-only operation")
		}
	}
	for id, ops := range spec.Operations {
		for _, op := range ops {
			if !slices.Contains(spec.Connections, id) || !s.catalog.ConnectionAllowed(spec.Actor, id, op) {
				return workspace.ErrForbidden
			}
			if _, ok := s.dataConnections.ConfiguredOperation(id, op); !ok {
				return errors.New("operation is not configured")
			}
		}
	}
	if spec.UsesAI() {
		if _, _, _, err := s.automationrun.Provider(spec); err != nil {
			return err
		}
	}
	if spec.Trigger != nil && spec.Trigger.Kind == "slack.knowledge" {
		if s.slack == nil || s.slack.config.KnowledgeChannels[spec.Trigger.Channel] != spec.Project {
			return errors.New("Slack knowledge channel is not configured for this notebook")
		}
	}

	if spec.NotifyChannel != "" {
		policy, _, enabled := s.messaging.MessageClient("slack")
		if !enabled {
			return errors.New("Slack is not configured")
		}
		b, ok := policy.Broadcasts[spec.NotifyChannel]
		if !ok || !b.AudienceApproved || b.Project != spec.Project || !slices.Contains(b.Actors, spec.Actor) {
			return errors.New("Slack destination is not approved for this automation")
		}
	}
	return nil
}
