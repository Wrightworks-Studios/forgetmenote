package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"forgetmenote/internal/automation"
	"forgetmenote/internal/automationrun"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/connectors"
	"forgetmenote/internal/messaginghttp"
	"forgetmenote/internal/workspace"
)

type learningSessionKey struct{}

func (s *Server) observeLearningUsage(ctx context.Context, in dataCall, receipt connectors.Receipt, callErr error, elapsed time.Duration) {
	if s.automations == nil {
		return
	}
	if a, automated := automation.IdentityFrom(ctx); automated {
		if a.Spec.Kind != "assistant" || a.Spec.Delivery == nil {
			return
		}
		// Interactive chat is observed usage; background repairs must never learn
		// from their own calls. Retain the human's request, not a full transcript.
		d := messaginghttp.MessageAddress(*a.Spec.Delivery)
		in.LearningContext = &automation.LearningContext{Conversation: automation.Hash([]string{d.Client, d.Workspace, d.Channel, d.Thread, d.User}), Intent: a.Spec.Instruction}
		if len(in.LearningContext.Intent) > 2000 {
			in.LearningContext.Intent = ""
		}
	}
	jobs := []automation.Spec{}
	for _, spec := range s.automations.Definitions.Specs() {
		if !spec.Disabled && spec.Kind == "learning" && slices.Contains(spec.Connections, in.Connection) {
			jobs = append(jobs, spec)
		}
	}
	if len(jobs) == 0 {
		return
	}
	op, ok := s.dataConnections.ConfiguredOperation(in.Connection, in.Operation)
	info, known := s.dataConnections.Info(in.Connection)
	if !ok || !known || !op.ReadOnly || !info.ExternalProcessing {
		return
	}
	raw, err := json.Marshal(in.Arguments)
	if err != nil || len(raw) > 12<<10 || automationrun.LearningSecret.Match(raw) || !s.dataConnections.LearningInputSafe(in.Connection, string(raw)) {
		return
	}
	var input map[string]any
	if json.Unmarshal(raw, &input) != nil {
		return
	}
	contextInput := automation.LearningContext{}
	if in.LearningContext != nil {
		contextInput = *in.LearningContext
	}
	if len(contextInput.Intent) > 2000 || len(contextInput.Conversation) > 128 || len(contextInput.RequestID) > 128 || automationrun.LearningSecret.MatchString(contextInput.Intent) || !s.dataConnections.LearningInputSafe(in.Connection, contextInput.Intent) {
		return
	}
	actor := subject(ctx)
	account, err := s.catalog.Account(actor)
	if err != nil {
		return
	}
	client, _ := ctx.Value(clientKey{}).(string)
	credential := ""
	if grant, ok := credentialFrom(ctx); ok {
		credential = grant.Hash
	}
	if lease, ok := ctx.Value(leaseKey{}).(revisionLease); ok && lease.key != digest(s.token) {
		credential = lease.key
	}
	now := time.Now().UTC()
	conversation := contextInput.Conversation
	if conversation == "" {
		conversation, _ = ctx.Value(learningSessionKey{}).(string)
		if scope := chat.RunScopeFrom(ctx); scope != nil {
			conversation = scope.Chat
		}
		// Transport sessions can contain multiple conversations. Absent explicit
		// client context this is only a bounded activity window.
		conversation = fmt.Sprintf("activity:%s:%d", conversation, now.Unix()/900)
	}
	conversation = automation.Hash([]string{account.ID, client, credential, conversation})
	id := contextInput.RequestID
	if id == "" {
		id = receipt.ID
	}
	if id == "" {
		id = RandomToken()
	}
	shapeJSON, _ := json.Marshal(automationrun.LearningShape(input, ""))
	shape := string(shapeJSON)
	outcome := "succeeded"
	if callErr != nil {
		outcome = "failed"
		if errors.Is(callErr, connectors.ErrQuery) {
			outcome = "query-error"
		}
	}
	for _, spec := range jobs {
		if !s.clientProjectAllowed(ctx, spec.Project) {
			continue
		}
		if _, err := s.clientAccess(ctx, actor, spec.Project, "read"); err != nil {
			continue
		}
		event := automation.LearningEvent{ID: automation.Hash([]string{conversation, id}), Actor: actor, Account: account.ID, Epoch: s.catalog.Epoch(actor), ProjectEpoch: s.catalog.ProjectAccessEpoch(actor, spec.Project), Credential: credential, Client: client, Conversation: conversation, Intent: contextInput.Intent, At: now, Call: automation.Call{Connection: in.Connection, Operation: in.Operation, Arguments: input, SchemaHash: receipt.SchemaHash}, Shape: shape, Outcome: outcome, DurationMS: elapsed.Milliseconds(), Receipt: receipt.ID, Complete: callErr == nil && receipt.Complete}
		if _, err := s.automations.Queue.ObserveLearning(spec, event, now); err != nil {
			_ = s.catalog.RecordActivity(actor, "learning.capture", spec.Project, "failed", map[string]string{"job": spec.ID})
		}
	}
}

func (s *Server) learningEventAllowed(ctx context.Context, r *automation.Run, e automation.LearningEvent) error {
	a, err := s.catalog.Account(e.Actor)
	if err != nil || a.ID != e.Account || !s.catalog.ValidSession(e.Actor, e.Epoch) || s.catalog.ProjectAccessEpoch(e.Actor, r.Spec.Project) != e.ProjectEpoch || !s.catalog.ConnectionAllowed(e.Actor, e.Call.Connection, e.Call.Operation) {
		return workspace.ErrForbidden
	}
	if _, err = s.catalog.Access(e.Actor, r.Spec.Project, "read"); err != nil {
		return err
	}
	if e.Credential != "" {
		g, ok := s.oauth.Credential(e.Credential)
		if ok {
			if g.Subject != e.Actor || g.Expires <= time.Now().Unix() || !s.catalog.ValidSession(g.Subject, g.Epoch) || !slices.Contains(g.Scopes, "data:read") || !slices.Contains(g.Connections, e.Call.Connection) || !g.AllProjects && !slices.Contains(g.Projects, r.Spec.Project) {
				return workspace.ErrForbidden
			}
		} else {
			access, oauth := s.oauthAuthorization(e.Credential)
			if !oauth || access.Subject != e.Actor || access.Expires <= time.Now().Unix() || access.Client != e.Client || !s.catalog.ValidSession(access.Subject, access.Epoch) || !slices.Contains(strings.Fields(access.Scope), "data:read") || (access.Resource != s.origin+"/mcp" && access.Resource != s.origin+apiPrefix) {
				return workspace.ErrForbidden
			}
		}
	}
	if !s.dataAllowed(ctx, e.Call.Connection, e.Call.Operation) {
		return workspace.ErrForbidden
	}
	return nil
}
