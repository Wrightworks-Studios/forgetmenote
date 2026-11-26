package chatrun

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"forgetmenote/internal/chat"
	"forgetmenote/internal/sources"
)

type researchRequest struct {
	run          *chat.Run
	conversation chat.Conversation
	snapshot     chat.Configuration
	query, url   string
	count        int
}

func (s *Service) researchRequest(a chat.RunAuth, id, runID string, call chat.ToolCall) (researchRequest, error) {
	c, err := s.hooks.Chat().Store.Read(a.User, a.Project, id)
	if err != nil {
		return researchRequest{}, err
	}
	in := researchRequest{run: c.Run(runID), conversation: c, snapshot: s.hooks.Chat().Snapshot(), count: 5}
	if in.run == nil {
		return researchRequest{}, chat.ErrNotFound
	}
	cfg := in.snapshot.Config
	if !in.run.Options.Research || cfg == nil || cfg.LocalOnly || cfg.Research == nil || !cfg.Research.Enabled || in.snapshot.Research == nil {
		return researchRequest{}, errors.New("external research is disabled for this run")
	}
	if call.Name == "research_search" {
		var args struct {
			Query string `json:"query"`
			Count int    `json:"count"`
		}
		if StrictArguments(call.Arguments, &args) != nil || args.Query == "" || len(args.Query) > 2000 || args.Count < 0 || args.Count > 10 {
			return researchRequest{}, errors.New("invalid research search arguments")
		}
		in.query = args.Query
		if args.Count != 0 {
			in.count = args.Count
		}
		return in, nil
	}
	var args struct {
		URL string `json:"url"`
	}
	if StrictArguments(call.Arguments, &args) != nil || sources.ValidateURL(args.URL) != nil {
		return researchRequest{}, errors.New("invalid public source arguments")
	}
	in.url = args.URL
	return in, nil
}

// researchURLProvided reports whether a fetch target came from the person's own
// turn text or from this run's search results. Any other URL may have been
// chosen by injected content and can carry private context in its path or
// query, so it needs the person's explicit approval of the exact URL.
func researchURLProvided(c chat.Conversation, run chat.Run, target string) bool {
	for _, turn := range c.Turns {
		if turn.Role == "user" && turn.RunID == run.ID && strings.Contains(turn.Content, target) {
			return true
		}
	}
	for _, op := range run.Operations {
		if op.Name != "research_search" || op.State != "completed" {
			continue
		}
		var result struct {
			Sources []chat.ResearchResult `json:"sources"`
		}
		if json.Unmarshal(op.Result, &result) != nil {
			continue
		}
		for _, source := range result.Sources {
			if source.URL == target {
				return true
			}
		}
	}
	return false
}

func (s *Service) ExecuteResearchTool(ctx context.Context, a chat.RunAuth, id, runID string, call chat.ToolCall, connection chat.Connection) error {
	in, err := s.researchRequest(a, id, runID, call)
	if err != nil {
		return err
	}
	key := OperationID(runID, in.run.Rounds, call.ID)
	for _, op := range in.run.Operations {
		if op.ID == key {
			if op.State == "completed" {
				return s.RunUpdate(a, id, runID, func(c *chat.Conversation, r *chat.Run) error {
					RecordToolResult(c, r, call.ID, researchToolContext(op.Result), op.Citations, true)
					return nil
				})
			}
			if op.State == "awaiting_confirmation" {
				return ErrConfirmation
			}
			return errors.New("interrupted research request has unknown external usage; start a new explicit research request")
		}
	}
	if err = s.hooks.CheckRun(a); err != nil {
		return err
	}
	if in.url != "" && !researchURLProvided(in.conversation, *in.run, in.url) {
		return s.hooks.PrepareOperation(ctx, a, id, runID, call, call.Arguments)
	}
	return s.performResearch(ctx, a, id, runID, key, call, in, connection, false)
}

// ExecuteApprovedResearch fetches the exact URL the person approved. Like an
// approved notebook control, the run then waits for explicit continuation.
func (s *Service) ExecuteApprovedResearch(ctx context.Context, a chat.RunAuth, id, runID string, op chat.Operation, connection chat.Connection) error {
	call := chat.ToolCall{ID: op.ToolCallID, Name: op.Name, Arguments: op.Arguments}
	in, err := s.researchRequest(a, id, runID, call)
	if err != nil {
		return err
	}
	if in.url == "" {
		return errors.New("only a research fetch can be approved")
	}
	return s.performResearch(ctx, a, id, runID, op.ID, call, in, connection, true)
}

func (s *Service) performResearch(ctx context.Context, a chat.RunAuth, id, runID, key string, call chat.ToolCall, in researchRequest, connection chat.Connection, approved bool) error {
	cfg, research, query, sourceURL := in.snapshot.Config, in.snapshot.Research, in.query, in.url
	// Persist attempts before crossing the network, including interrupted requests.
	err := s.RunUpdate(a, id, runID, func(c *chat.Conversation, r *chat.Run) error {
		if call.Name == "research_search" {
			if r.ResearchSearches >= cfg.Research.MaxSearches {
				return FailureError(chatResearchBudget, "research search budget reached")
			}
			r.ResearchSearches++
			if research.SearchReservation(query) > 0 {
				r.InFlightTokens = int64(research.SearchReservation(query))
				r.InFlightDay = chat.Day()
			}
		} else {
			if r.ResearchFetches >= cfg.Research.MaxFetches {
				return FailureError(chatResearchBudget, "research fetch budget reached")
			}
			r.ResearchFetches++
		}
		if approved {
			for i := range r.Operations {
				if o := &r.Operations[i]; o.ID == key {
					if !o.Approved || o.State != "running" || len(o.Result) > 0 {
						return chat.ErrConflict
					}
					c.AddEvent(runID, "tool_started", map[string]string{"id": key, "name": call.Name})
					return nil
				}
			}
			return chat.ErrNotFound
		}
		r.ToolCount++
		r.Operations = append(r.Operations, chat.Operation{ID: key, ToolCallID: call.ID, Round: r.Rounds, Name: call.Name, Arguments: call.Arguments, Hash: chat.Hash(call.Arguments), State: "running"})
		c.AddEvent(runID, "tool_started", map[string]string{"id": key, "name": call.Name})
		return nil
	})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	release, err := s.hooks.AcquireSourceWork(ctx)
	if err != nil {
		return err
	}
	defer release()
	var result any
	var usage chat.Usage
	var citations []chat.Citation
	if sourceURL == "" {
		var found []chat.ResearchResult
		found, usage, err = in.snapshot.SearchResearch(ctx, research, query, in.count)
		result = map[string]any{"kind": "external-search", "capturedAt": chat.Now(), "verified": false, "sources": found, "message": "Search snippets are candidates; fetch original sources before claims."}
	} else {
		capture, e := in.snapshot.PrepareSource(ctx, "", sourceURL, nil)
		err = e
		if err == nil {
			citation := chat.Citation{ID: "external-" + key, URL: capture.Record.FinalURL, Title: capture.Title, Text: TextLimit(capture.Text, min(12000, connection.Limits.EvidenceTokens*3)), Hash: capture.Record.SHA256, CapturedAt: capture.Record.CapturedAt, Kind: "external"}
			if citation.URL == "" {
				citation.URL = sourceURL
			}
			citations = []chat.Citation{citation}
			result = map[string]any{"source": citation, "provenance": capture.Record, "archived": false, "extraction": capture.Record.Extraction, "warning": capture.Record.Warning}
		}
	}
	if research.SearchReservation(query) > 0 && sourceURL == "" {
		if settleErr := s.hooks.Chat().Store.SettleUsage(a.User, a.Project, id, runID, usage, 0); settleErr != nil {
			return settleErr
		}
	}
	if s.hooks.CheckRun(a) != nil {
		return FailureError(AccessRevoked, "notebook access was revoked")
	}
	if err != nil {
		result = map[string]string{"error": "External research failed or was interrupted; no source was archived."}
	}
	raw, _ := json.Marshal(result)
	return s.RunUpdate(a, id, runID, func(c *chat.Conversation, r *chat.Run) error {
		for i := range r.Operations {
			if r.Operations[i].ID == key {
				o := &r.Operations[i]
				o.State = "completed"
				o.Result = raw
				if err != nil {
					o.Error = "External research failed or was interrupted"
				}
				c.AddEvent(runID, "tool_result", *o)
			}
		}
		if err == nil {
			p := chat.Provenance{ExternalAllowance: reviewExternalAllowance(r)}
			for _, citation := range citations {
				p.ExternalSources = append(p.ExternalSources, externalCapture(citation))
			}
			inheritExternalEvidence(&r.Provenance, p)
		}
		RecordToolResult(c, r, call.ID, researchToolContext(raw), citations, true)
		return nil
	})
}

// researchToolContext is the model's view of a research result: the bounded
// receipt, whose sources are untrusted external captures.
func researchToolContext(result json.RawMessage) string { return TextLimit(string(result), 16000) }
