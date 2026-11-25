package web

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"forgetmenote/internal/automation"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/decision"
	"forgetmenote/internal/messaging"
	"forgetmenote/internal/sources"
	"forgetmenote/internal/workspace"
	"github.com/google/jsonschema-go/jsonschema"
)

type scriptCall struct {
	Connection string         `json:"connection"`
	Operation  string         `json:"operation"`
	Arguments  map[string]any `json:"arguments"`
}
type scriptGuard struct {
	Read      scriptCall     `json:"read"`
	Equals    map[string]any `json:"equals"`
	ExpiresAt time.Time      `json:"expiresAt"`
}
type scriptSubmit struct {
	scriptCall
	Guard          scriptGuard    `json:"guard"`
	Acknowledgment map[string]any `json:"acknowledgment"`
}

func (s *Server) boundScriptCall(e *scriptExecution, c scriptCall, write bool) (automation.Call, error) {
	spec := e.run.Spec
	id := spec.Script.Connections[c.Connection]
	if id == "" || !e.info.Manifest.RequestsOperation(c.Connection, c.Operation) || !slices.Contains(spec.Connections, id) || !slices.Contains(spec.Operations[id], c.Operation) || !s.dataAllowed(e.ctx, id, c.Operation) {
		return automation.Call{}, workspace.ErrForbidden
	}
	op, ok := s.dataConnections.ConfiguredOperation(id, c.Operation)
	if !ok || op.ReadOnly == write {
		return automation.Call{}, errors.New("operation does not match the requested read/write capability")
	}
	return automation.Call{Connection: id, Operation: c.Operation, Arguments: c.Arguments, SchemaHash: op.SchemaHash}, nil
}
func (s *Server) scriptCallAuthority(e *scriptExecution, in scriptRequest) error {
	if in.Operation == "ai.structured" {
		var v struct {
			Research *struct {
				MaxSearches int `json:"maxSearches"`
			} `json:"research"`
			CacheKey string   `json:"cacheKey"`
			Images   []string `json:"images"`
		}
		if err := json.Unmarshal(in.Arguments, &v); err != nil {
			return err
		}
		if v.Research != nil {
			if _, err := s.scriptResearchConfiguration(); err != nil {
				return err
			}
			if !slices.Contains(e.run.Spec.Script.Capabilities, "web.search") {
				return workspace.ErrForbidden
			}
			if v.CacheKey != "" {
				return errors.New("required research cannot reuse an analysis cache")
			}
			if len(v.Images) > 0 && !slices.Contains(e.run.Spec.Script.Capabilities, "ai.images") {
				return workspace.ErrForbidden
			}
		}
	}
	switch in.Operation {
	case "web.fetch":
		var v struct {
			Render bool `json:"render"`
			Image  bool `json:"image"`
		}
		if err := json.Unmarshal(in.Arguments, &v); err != nil {
			return err
		}
		if v.Render && (v.Image || !slices.Contains(e.run.Spec.Script.Capabilities, "ai.images")) {
			return workspace.ErrForbidden
		}
	case "connectors.read", "connectors.submit":
		var c scriptCall
		if json.Unmarshal(in.Arguments, &c) != nil {
			return errors.New("invalid connector call")
		}
		_, err := s.boundScriptCall(e, c, in.Operation == "connectors.submit")
		if err != nil {
			return err
		}
		if in.Operation == "connectors.submit" {
			var v scriptSubmit
			_ = json.Unmarshal(in.Arguments, &v)
			managed := s.dataConnections.ManagedWrites(e.run.Spec.Script.Connections[c.Connection])
			if !managed && (len(v.Guard.Equals) == 0 || v.Guard.ExpiresAt.IsZero() || len(v.Acknowledgment) == 0) {
				return errors.New("submission needs a read guard, original expiry and acknowledgment assertions")
			}
			if !managed {
				if _, err = s.boundScriptCall(e, v.Guard.Read, false); err != nil {
					return err
				}
			}
			for p := range v.Guard.Equals {
				if !automation.ValidPointer(p) {
					return errors.New("invalid guard pointer")
				}
			}
			for p := range v.Acknowledgment {
				if !automation.ValidPointer(p) {
					return errors.New("invalid acknowledgment pointer")
				}
			}
		}
	case "messages.post":
		if _, _, err := s.scriptMessageDestination(e, in.Arguments); err != nil {
			return err
		}
	case "notebooks.read", "notebooks.publish", "notebooks.map", "repositories.changes":
		var v struct {
			Notebook string `json:"notebook"`
			Origin   string `json:"origin"`
			Path     string `json:"path"`
			Revision string `json:"revision"`
		}
		if json.Unmarshal(in.Arguments, &v) != nil {
			return errors.New("invalid notebook call")
		}
		p := e.run.Spec.Script.Notebooks[v.Notebook]
		role := e.info.Manifest.Notebooks[v.Notebook]
		if p == "" || role == "" || in.Operation == "notebooks.publish" && (role != "edit" || p != e.run.Spec.Project) {
			return workspace.ErrForbidden
		}
		if _, err := s.clientAccess(e.ctx, e.run.Spec.Actor, p, role); err != nil {
			return err
		}
		if err := s.captureScriptSources(e); err != nil {
			return err
		}
		if v.Origin == "repository" || in.Operation == "notebooks.map" || in.Operation == "repositories.changes" {
			source, ok := e.run.Script.Sources[v.Notebook]
			if !ok || v.Revision != "" && v.Revision != source.Revision {
				return workspace.ErrForbidden
			}
			var paths []string
			if in.Operation == "notebooks.read" {
				paths = []string{v.Path}
			}
			if err := s.repositories.AuthorizeIndexedRead(e.ctx, p, source.Source, source.Revision, paths); err != nil {
				return err
			}
		}
	}
	external := slices.Contains([]string{"ai.structured", "ai.decisions", "decision.evaluate", "web.fetch", "web.search"}, in.Operation)
	if external {
		if !e.run.Spec.ExternalProcessing {
			return workspace.ErrForbidden
		}
		state, err := s.repositories.Status(e.run.Spec.Script.SourceProject)
		if err != nil {
			return err
		}
		if !state.IndexConfig.ExternalProcessing {
			return errors.New("package notebook does not consent to external processing")
		}
		config := s.chat.Snapshot().Config
		if config != nil && config.LocalOnly && in.Operation != "ai.structured" {
			return workspace.ErrForbidden
		}
	}
	return nil
}
func (s *Server) scriptOperation(e *scriptExecution, in scriptRequest, started time.Time) (any, error) {
	r := e.run
	switch in.Operation {
	case "jobstate.get":
		if r.DryRun && r.Script.PreviewJobState != nil {
			return *r.Script.PreviewJobState, nil
		}
		return s.automations.Queue.JobState(r.Spec)
	case "jobstate.set":
		return s.scriptSetJobState(e, in)
	case "checkpoint.set":
		var v struct {
			State json.RawMessage `json:"state"`
		}
		if err := json.Unmarshal(in.Arguments, &v); err != nil {
			return nil, err
		}
		r.Script.State = v.State
		return map[string]bool{"saved": true}, nil
	case "run.complete":
		var v struct {
			Result any `json:"result"`
		}
		if err := json.Unmarshal(in.Arguments, &v); err != nil {
			return nil, err
		}
		b, _ := json.Marshal(v.Result)
		r.Result = string(b)
		return v.Result, nil
	case "incidents.report":
		return s.scriptReportIncident(e, in)
	case "connectors.read":
		return s.scriptReadConnector(e, in, started)
	case "connectors.submit":
		return s.scriptSubmit(e, in)
	case "notebooks.read":
		return s.scriptReadDocument(e, in)
	case "notebooks.map", "repositories.changes":
		return s.scriptRepositoryNavigation(e, in)
	case "notebooks.publish":
		return s.scriptPublish(e, in)
	case "messages.post":
		return s.scriptPostMessage(e, in)
	case "ai.structured":
		return s.scriptStructured(e, in)
	case "ai.decisions":
		return s.scriptDecisions(e, in)
	case "decision.evaluate":
		return s.scriptEvaluateDecision(e, in)
	case "web.fetch":
		return s.scriptFetch(e, in)
	case "documents.extract":
		return s.scriptExtractDocument(e, in)
	case "web.search":
		return s.scriptSearch(e, in)
	default:
		return nil, errors.New("unsupported host operation")
	}
}
func (s *Server) scriptSetJobState(e *scriptExecution, in scriptRequest) (any, error) {
	r := e.run
	var v struct {
		Revision string          `json:"revision"`
		Data     json.RawMessage `json:"data"`
	}
	if err := automation.DecodeScriptRequest(in.Arguments, &v); err != nil {
		return nil, err
	}
	if r.DryRun && r.Script.PreviewJobState != nil {
		next, err := automation.NextJobState(*r.Script.PreviewJobState, v.Revision, v.Data)
		if err == nil {
			r.Script.PreviewJobState = &next
		}
		return next, err
	}

	next, err := s.automations.Queue.SaveJobState(r.Spec, v.Revision, v.Data, r.DryRun)
	if err == nil && r.DryRun {
		r.Script.PreviewJobState = &next
	}
	return next, err
}
func (s *Server) scriptReportIncident(e *scriptExecution, in scriptRequest) (any, error) {
	r := e.run
	var fields map[string]json.RawMessage
	if json.Unmarshal(in.Arguments, &fields) != nil || fields["active"] == nil || string(fields["active"]) == "null" {
		return nil, errors.New("finding requires an explicit active boolean")
	}
	var finding automation.Incident
	if err := automation.DecodeScriptRequest(in.Arguments, &finding); err != nil {
		return nil, err
	}
	if strings.TrimSpace(finding.Summary) == "" || len(finding.Summary) > 16000 || len(finding.Key) > 512 || !slices.Contains([]string{"warning", "critical"}, finding.Severity) {
		return nil, errors.New("finding requires a summary and warning or critical severity")
	}
	for id, previous := range r.Script.Steps {
		if id != in.Step && previous.Operation == "incidents.report" {
			return nil, errors.New("one incident finding per run")
		}
	}
	if err := s.recordAutomationIncident(r, finding, e.save); err != nil {
		return nil, err
	}
	return r.Alert, nil
}
func (s *Server) scriptReadConnector(e *scriptExecution, in scriptRequest, started time.Time) (any, error) {
	var c scriptCall
	_ = json.Unmarshal(in.Arguments, &c)
	call, err := s.boundScriptCall(e, c, false)
	if err != nil {
		return nil, err
	}
	result, err := s.automationCall(e.ctx, e.run, call)
	if err != nil {
		return nil, err
	}
	if !result.Receipt.Complete {
		return nil, errors.New("incomplete connector response")
	}
	return map[string]any{"data": result.Data, "receipt": result.Receipt, "receivedAt": started}, nil
}
func (s *Server) scriptSubmit(e *scriptExecution, in scriptRequest) (any, error) {
	ctx, r := e.ctx, e.run
	var v scriptSubmit
	if err := json.Unmarshal(in.Arguments, &v); err != nil {
		return nil, err
	}
	managed := s.dataConnections.ManagedWrites(r.Spec.Script.Connections[v.Connection])
	if !managed {
		if !time.Now().Before(v.Guard.ExpiresAt) {
			return nil, errors.New("guard expired; no submission sent")
		}
		read, err := s.boundScriptCall(e, v.Guard.Read, false)
		if err != nil {
			return nil, err
		}
		fresh, err := s.automationCall(ctx, r, read)
		if err != nil {
			return nil, err
		}
		if !fresh.Receipt.Complete {
			return nil, errors.New("incomplete guard response")
		}
		if err = scriptAssertions(fresh.Data, v.Guard.Equals); err != nil {
			return nil, fmt.Errorf("guard changed: %w", err)
		}
		if err = s.scriptAuthority(e); err != nil {
			return nil, err
		}
		if !time.Now().Before(v.Guard.ExpiresAt) {
			return nil, errors.New("guard expired during refresh")
		}
	}
	if r.DryRun {
		return map[string]any{"dryRun": true, "submitted": false}, nil
	}
	call, err := s.boundScriptCall(e, v.scriptCall, true)
	if err != nil {
		return nil, err
	}
	writeCtx := context.WithValue(ctx, chatScopeKey{}, (*chatRunScope)(nil))
	writeCtx = context.WithValue(writeCtx, scopeKey{}, "notes:read data:read data:write")
	result, err := s.automationCall(writeCtx, r, call)
	if err != nil {
		return nil, err
	}
	if !result.Receipt.Complete {
		return nil, errors.New("incomplete submission response; outcome unknown")
	}
	if !managed {
		if err = scriptAssertions(result.Data, v.Acknowledgment); err != nil {
			return nil, fmt.Errorf("submission acknowledgment mismatch; outcome unknown: %w", err)
		}
	}
	return map[string]any{"data": result.Data, "receipt": result.Receipt}, nil
}
func (s *Server) scriptPublish(e *scriptExecution, in scriptRequest) (any, error) {
	ctx, r := e.ctx, e.run
	var v struct{ Notebook, Path, Content, Revision, Hash string }
	if err := json.Unmarshal(in.Arguments, &v); err != nil {
		return nil, err
	}
	if r.DryRun {
		return map[string]any{"dryRun": true, "content": v.Content}, nil
	}
	a := ctx.Value(automationKey{}).(automationAuth)
	var result workspace.GroupResult
	err := s.catalog.WithProjectAuthority(a.Spec.Actor, a.Spec.Project, a.Account, "edit", a.Epoch, a.ProjectEpoch, func(store *workspace.Store) error {
		var err error
		result, err = store.ApplyGroup(ctx, workspace.EditGroup{ID: "script-" + automation.Hash([]string{r.ID, in.Step}), Summary: "Automation: " + r.Spec.ID, Edits: []workspace.NoteEdit{{Path: v.Path, Content: v.Content, Revision: v.Revision, Hash: v.Hash}}})
		return err
	})
	if err == nil {
		// The optimistic write checked the old output. Subsequent steps must
		// not reject the run because its own publication replaced that note.
		r.Citations = slices.DeleteFunc(r.Citations, func(c chat.Citation) bool {
			return c.Kind == "notebook" && c.Project == r.Spec.Project && c.Path == v.Path
		})
	}
	return result, err
}

type scriptMessage struct {
	Text   string            `json:"text"`
	Thread string            `json:"thread,omitempty"`
	Image  *messaging.Image  `json:"image,omitempty"`
	Images []messaging.Image `json:"images,omitempty"`
	Cards  []messaging.Card  `json:"cards,omitempty"`
}

// scriptMessageDestination resolves a package post to the job's shared
// notification channel. The package cannot choose another channel, and the
// channel's approved audience must cover everything the run has read so far.
func (s *Server) scriptMessageDestination(e *scriptExecution, raw json.RawMessage) (string, messaging.Post, error) {
	var v scriptMessage
	if err := automation.DecodeScriptRequest(raw, &v); err != nil {
		return "", messaging.Post{}, err
	}
	post := messaging.Post{Text: v.Text, Thread: v.Thread, Image: v.Image, Images: v.Images, Cards: v.Cards}
	if err := post.Validate(); err != nil {
		return "", post, err
	}
	channel := e.run.Spec.NotifyChannel
	if channel == "" {
		return "", post, errors.New("messages.post requires the job's shared notifyChannel")
	}
	policy, _, ok := s.messageClient("slack")
	if !ok || s.slack == nil {
		return "", post, workspace.ErrForbidden
	}
	return channel, post, sharedAudience(policy, channel, e.run)
}

// scriptPostMessage returns the posted message's Slack ID so later posts can
// reply in its thread. A preview checks the destination and sends nothing.
func (s *Server) scriptPostMessage(e *scriptExecution, in scriptRequest) (any, error) {
	channel, post, err := s.scriptMessageDestination(e, in.Arguments)
	if err != nil {
		return nil, messaging.NotPosted(err)
	}
	if e.run.DryRun {
		return map[string]any{"dryRun": true, "posted": false}, nil
	}
	ts, err := s.slack.Post(e.ctx, channel, post)
	if err != nil {
		return nil, err
	}
	return map[string]any{"channel": channel, "ts": ts}, nil
}
func (s *Server) scriptStructured(e *scriptExecution, in scriptRequest) (any, error) {
	var v struct {
		Prompt   string          `json:"prompt"`
		Schema   json.RawMessage `json:"schema"`
		Images   []string        `json:"images"`
		Research *struct {
			MaxSearches int `json:"maxSearches"`
		} `json:"research"`
	}
	if err := json.Unmarshal(in.Arguments, &v); err != nil {
		return nil, err
	}
	if len(v.Images) > 0 && !slices.Contains(e.run.Spec.Script.Capabilities, "ai.images") {
		return nil, workspace.ErrForbidden
	}
	for _, img := range v.Images {
		if !strings.HasPrefix(img, "data:image/") {
			return nil, errors.New("AI images must be inline host-fetched evidence")
		}
		_, encoded, ok := strings.Cut(img, ",")
		if !ok {
			return nil, errors.New("invalid inline image")
		}
		b, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, err
		}
		if _, err = chat.InlineImage(b); err != nil {
			return nil, err
		}
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(v.Schema, &schema); err != nil {
		return nil, err
	}
	if _, err := schema.Resolve(nil); err != nil {
		return nil, err
	}
	if v.Research != nil {
		return s.scriptStructuredResearch(e, chat.StructuredResearchRequest{Prompt: v.Prompt, Schema: v.Schema, Images: v.Images, MaxSearches: v.Research.MaxSearches})
	}
	return s.scriptStructuredAnswer(e, v.Prompt, v.Schema, v.Images)
}
func (s *Server) scriptDecisions(e *scriptExecution, in scriptRequest) (any, error) {
	r := e.run
	var v struct {
		chat.DecisionsRequest
		CacheKey string `json:"cacheKey,omitempty"`
	}
	if err := automation.DecodeScriptRequest(in.Arguments, &v); err != nil {
		return nil, err
	}
	if len(v.Images) > 0 && !slices.Contains(r.Spec.Script.Capabilities, "ai.images") {
		return nil, workspace.ErrForbidden
	}
	v.Model = r.Spec.Model
	ctx, model, err := s.automationModelCall(e.ctx, r, func(model automationModel) error {
		estimate, err := model.provider.EstimateDecisions(v.DecisionsRequest)
		if err != nil {
			return err
		}
		return automationTokenBudget(r, estimate)
	})
	if err != nil {
		return nil, err
	}
	defer model.release()
	if err = s.checkAutomationEvidence(ctx, r, true); err != nil {
		return nil, err
	}
	v.Check = func(ctx context.Context) error {
		if err := s.checkAutomationEvidence(ctx, r, true); err != nil {
			return err
		}
		return s.scriptAuthority(e)
	}
	result, err := model.provider.Decide(ctx, v.DecisionsRequest)
	r.InputTokens += int(result.Usage.InputTokens)
	r.OutputTokens += int(result.Usage.OutputTokens)
	if err != nil {
		return nil, err
	}
	if err = s.checkAutomationEvidence(ctx, r, true); err != nil {
		return nil, err
	}
	return result, nil
}
func (s *Server) scriptEvaluateDecision(e *scriptExecution, in scriptRequest) (any, error) {
	r := e.run
	var request decision.Request
	if err := json.Unmarshal(in.Arguments, &request); err != nil {
		return nil, err
	}
	judge, local := s.jevConfigured()
	if judge == nil {
		return nil, errors.New("decision provider unavailable")
	}
	return judge.Evaluate(e.ctx, decision.Evaluation{Scope: decision.Scope{Actor: r.Spec.Actor, Notebook: r.Spec.Project, Feature: "automation-script", Run: r.ID, Operation: automation.Hash(in.Step)}, Request: request, LocalOnly: local, Background: true, Check: func(context.Context) error { return s.scriptAuthority(e) }})
}
func (s *Server) scriptFetch(e *scriptExecution, in scriptRequest) (any, error) {
	ctx, r := e.ctx, e.run
	var v struct {
		URL               string `json:"url"`
		Image             bool   `json:"image"`
		Render            bool   `json:"render"`
		Detail            bool   `json:"detail"`
		DismissPromotions bool   `json:"dismissPromotions"`
	}
	if err := json.Unmarshal(in.Arguments, &v); err != nil {
		return nil, err
	}
	if err := sources.ValidateURL(v.URL); err != nil {
		if v.Render {
			return nil, errors.Join(chat.ErrWebsiteCaptureNotAttempted, err)
		}
		return nil, err
	}
	if v.Image {
		return s.scriptFetchImage(ctx, v.URL, v.Detail)
	}
	cfg := s.chat.Snapshot().Config
	maxFetches := 5
	if cfg != nil && cfg.Research != nil && cfg.Research.MaxFetches > 0 {
		maxFetches = cfg.Research.MaxFetches
	}
	fetches := 0
	for _, step := range r.Script.Steps {
		if step.Operation == "web.fetch" {
			var a struct {
				Image bool `json:"image"`
			}
			_ = json.Unmarshal(step.Input, &a)
			if !a.Image {
				fetches++
			}
		}
	}
	if fetches > maxFetches {
		if v.Render {
			return nil, errors.Join(chat.ErrWebsiteCaptureNotAttempted, errors.New("configured research fetch budget exhausted"))
		}
		return nil, errors.New("configured research fetch budget exhausted")
	}
	release, err := s.acquireSourceWork(ctx)
	if err != nil {
		if v.Render {
			return nil, errors.Join(chat.ErrWebsiteCaptureNotAttempted, err)
		}
		return nil, err
	}
	defer release()
	if v.Render {
		if s.scriptWebsiteCapture != nil {
			return s.scriptWebsiteCapture(ctx, v.URL)
		}
		if v.DismissPromotions {
			return s.chat.Snapshot().WebsiteCapture.CaptureAfterDismissingPromotions(ctx, v.URL)
		}
		return s.chat.Snapshot().WebsiteCapture.Capture(ctx, v.URL)
	}
	prepare := s.chat.Snapshot().PrepareSource
	capture, err := prepare(ctx, "", v.URL, nil)
	if err != nil {
		return nil, err
	}
	return map[string]any{"text": capture.Text, "title": capture.Title, "record": capture.Record}, nil
}
func (s *Server) scriptFetchImage(ctx context.Context, url string, detail bool) (any, error) {
	fetch := sources.Fetch
	if s.scriptImageFetch != nil {
		fetch = s.scriptImageFetch
	}
	b, _, final, err := fetch(ctx, url)
	if err != nil {
		return nil, err
	}
	img, err := chat.InlineImage(b)
	if err != nil {
		return nil, err
	}
	result := map[string]any{"image": img, "url": final}
	if !detail {
		return result, nil
	}
	release, err := s.acquireSourceWork(ctx)
	if err != nil {
		return nil, err
	}
	regions, err := chat.ImageDetails(ctx, b)
	if err != nil {
		release()
		return nil, err
	}
	result["regions"] = regions
	edgeRegions, err := chat.ImageEdgeDetails(ctx, b)
	if err != nil {
		release()
		return nil, err
	}
	result["edgeRegions"] = edgeRegions
	observations, err := chat.ObserveImage(ctx, b)
	release()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err == nil && observations.Version > 0 {
		result["pixelObservations"] = observations
	}
	return result, nil
}
func (s *Server) scriptExtractDocument(e *scriptExecution, in scriptRequest) (any, error) {
	var v struct {
		Name string `json:"name"`
		Data string `json:"data"`
	}
	if err := json.Unmarshal(in.Arguments, &v); err != nil {
		return nil, err
	}
	b, err := base64.StdEncoding.DecodeString(v.Data)
	if err != nil {
		return nil, err
	}
	release, err := s.acquireSourceWork(e.ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	capture, err := sources.Prepare(e.ctx, v.Name, "", b)
	if err != nil {
		return nil, err
	}
	return map[string]any{"text": capture.Text, "title": capture.Title, "record": capture.Record}, nil
}
func (s *Server) scriptSearch(e *scriptExecution, in scriptRequest) (any, error) {
	ctx, r := e.ctx, e.run
	var v struct {
		Query       string `json:"query"`
		Limit       int    `json:"limit"`
		Diagnostics bool   `json:"diagnostics"`
	}
	if err := json.Unmarshal(in.Arguments, &v); err != nil {
		return nil, err
	}
	snapshot := s.chat.Snapshot()
	cfg, research, search := snapshot.Config, snapshot.Research, snapshot.SearchResearch
	if cfg == nil || cfg.LocalOnly || cfg.Research == nil || !cfg.Research.Enabled || research == nil {
		return nil, errors.New("public research is not configured")
	}
	if v.Limit < 1 || v.Limit > cfg.Research.MaxFetches {
		return nil, errors.New("search result count exceeds configured research budget")
	}
	if scriptResearchCalls(r) > cfg.Research.MaxSearches {
		return nil, errors.New("configured research search budget exhausted")
	}
	selected, selectedErr := cfg.Resolve(r.Spec.AIConnection, r.Spec.Model)
	if r.Spec.AIConnection != "" && selectedErr == nil && selected.Cloudflare != nil {
		_, _, provider, err := s.automationProvider(r.Spec)
		if err != nil {
			return nil, err
		}
		if err := s.checkAutomationEvidence(ctx, r, true); err != nil {
			return nil, err
		}
		found, diagnostics, err := provider.CloudflareSearchDiagnostics(ctx, v.Query, v.Limit)
		if err != nil {
			return nil, err
		}
		if err := s.checkAutomationEvidence(ctx, r, true); err != nil {
			return nil, err
		}
		if v.Diagnostics {
			return map[string]any{"results": found, "diagnostics": diagnostics}, nil
		}
		return found, nil
	}
	if r.Spec.AIConnection != "" && selectedErr != nil {
		return nil, selectedErr
	}
	reservation := research.SearchReservation(v.Query)
	if r.Spec.MaxTokens > 0 && r.InputTokens+r.OutputTokens+reservation > r.Spec.MaxTokens {
		return nil, errors.New("automation token budget reached")
	}
	if connection, paid := research.ModelConnection(); paid {
		if policy, managed := s.catalog.AISettings(); managed && !policy.Writing {
			return nil, errInstanceAI
		}
		var release func()
		var err error
		ctx, release, err = s.reserveAutomationModel(ctx, cfg, connection)
		if err != nil {
			return nil, err
		}
		defer release()
	}
	found, usage, err := search(ctx, research, v.Query, v.Limit)
	r.InputTokens += int(usage.InputTokens)
	r.OutputTokens += int(usage.OutputTokens)
	return found, err
}
func scriptAssertions(data any, assertions map[string]any) error {
	for p, want := range assertions {
		got, ok := automation.Pointer(data, p)
		if !ok || automation.Hash(got) != automation.Hash(want) {
			return fmt.Errorf("value at %s does not match", p)
		}
	}
	return nil
}
