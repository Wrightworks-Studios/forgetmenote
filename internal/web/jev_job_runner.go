package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"forgetmenote/internal/attribution"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/decision"
	"forgetmenote/internal/knowledge"
	"forgetmenote/internal/workspace"
	"sort"
	"strings"
)

func (s *Server) checkJevJob(ctx context.Context, job jevJob, judge *decision.Service) error {
	if job.Organization != nil {
		return s.checkOrganizationJob(ctx, job, judge)
	}
	if err := s.jevAuthorized(ctx, job.Owner, job.Project, job.Kind, judge); err != nil {
		return err
	}
	if job.Model != judge.Model() {
		return decision.ErrDisabled
	}
	account, err := s.catalog.Account(job.Owner)
	if err != nil || account.ID != job.AccountID {
		return workspace.ErrForbidden
	}
	role := "comment"
	if job.Automatic {
		role = "owner"
	}
	store, err := s.catalog.Access(job.Owner, job.Project, role)
	if err != nil {
		return err
	}
	if job.Automatic {
		// A review an earlier policy started on a private notebook stops here.
		if _, managed := s.catalog.AISettings(); managed && !s.catalog.OrdinaryNotebook(job.Project) {
			return workspace.ErrForbidden
		}
		settings, err := s.loadJevNotebook(job.Project)
		if err != nil {
			return err
		}
		if !settings.AutoChecks || settings.AccountID != job.AccountID || settings.EnabledBy != job.Owner {
			return workspace.ErrForbidden
		}
	}
	if err = validateJevSources(ctx, store, job); err != nil {
		return err
	}
	s.jevState.mu.Lock()
	current, err := s.jevState.readJob(job.ID)
	s.jevState.mu.Unlock()
	if err == nil && current.Cancelled {
		return context.Canceled
	}
	if err != nil && !errors.Is(err, chat.ErrNotFound) {
		return err
	}
	return nil
}

func (s *Server) runJevJob(ctx context.Context, job jevJob) {
	if job.Organization != nil {
		s.runOrganizationJob(ctx, job)
		return
	}
	pause := func(message string) {
		job.State = "paused"
		job.Message = message
		s.settleJevJob(&job)
	}
	judge, _ := s.jevConfigured()
	if err := s.checkJevJob(ctx, job, judge); err != nil {
		pause("Access, consent, settings or source content changed. Review current sources before continuing.")
		return
	}
	store, err := s.catalog.Access(job.Owner, job.Project, "comment")
	if err != nil {
		pause("Notebook permission is unavailable.")
		return
	}
	account, err := s.catalog.Account(job.Owner)
	if err != nil {
		pause("Enabling account is unavailable.")
		return
	}
	ctx = attribution.WithSource(ctx, account.Identity(), "jev", "embedded")
	passages, err := loadJevPassages(ctx, store, job)
	if err != nil {
		pause("Pinned sources are unavailable; start a fresh review.")
		return
	}
	for job.Cursor < len(job.Work) {
		if ctx.Err() != nil {
			job.State = "queued"
			s.settleJevJob(&job)
			return
		}
		if err = s.checkJevJob(ctx, job, judge); err != nil {
			pause("The batch stopped. Continue only with current access, consent and unchanged sources.")
			return
		}
		work := &job.Work[job.Cursor]
		evidence, err := workEvidence(*work, passages)
		if err != nil {
			pause("Review checkpoint is unavailable; restore its durable record.")
			return
		}
		if work.Verdict == "" {
			var request decision.Request
			feature := job.Kind
			switch work.Kind {
			case "consistency":
				request = knowledge.ConsistencyRequest(evidence)
			case "audit":
				feature = "extraction"
				request = knowledge.ExtractionRequest(evidence[0], evidence[1:])
			case "organization":
				request = knowledge.OrganizationRequest(evidence)
			case "tags":
				feature = "organization"
				request = knowledge.TagRequest(evidence[0], work.Tags)
			default:
				pause("Unknown durable review task.")
				return
			}
			result, callErr := judge.Evaluate(ctx, decision.Evaluation{Scope: decision.Scope{Actor: job.AccountID, Notebook: job.Project, Feature: feature, Run: job.ID, Operation: fmt.Sprintf("task-%d-%d", job.Cursor, work.Attempt)}, Background: true, Request: request, Check: func(checkCtx context.Context) error { return s.checkJevJob(checkCtx, job, judge) }})
			if callErr != nil {
				pause("A judgment was not completed. Usage is retained, including unknown usage. Continue retries this task with a new recorded attempt.")
				return
			}
			if work.Kind == "tags" {
				work.Verdict = "tags"
				work.Scores = map[string]float64{}
				for key, answer := range result.Answers {
					if answer.Probability != nil {
						work.Scores[key] = *answer.Probability
					}
				}
			} else {
				answer := result.Answers["assessment"]
				work.Verdict = answer.Choice
				if answer.Confidence == nil || *answer.Confidence < .6 {
					work.Verdict = "uncertain"
				}
				if work.Verdict == "contradiction" && (answer.Confidence == nil || *answer.Confidence < .8 || answer.Probabilities["contradiction"] < .9) {
					work.Verdict = "uncertain"
				}
			}
			// Keep the judgment before filing a shared observation; crash recovery can
			// replay its exact mutation intent without buying the same judgment again.
			if !s.checkpointJevJob(&job) {
				return
			}
		}
		if err = s.checkJevJob(ctx, job, judge); err != nil {
			pause("Sources changed before publication; start a fresh review.")
			return
		}
		result := jevJobResult{ID: digest(job.ID + fmt.Sprint(job.Cursor)), Kind: work.Kind, Status: work.Verdict, Evidence: evidence}
		switch work.Kind {
		case "consistency":
			result.Title = "Consistency: " + work.Verdict
			result.Summary = "Judgment compares the recorded original excerpts with their stated context. It does not establish which statement is correct."
			if work.Verdict == "contradiction" {
				result.Title = "Possible contradiction"
				finding, err := s.publishJevFinding(ctx, &job, work, evidence, judge)
				if err != nil {
					pause("The finding could not be saved. Continue reconciles its recorded retry intent.")
					return
				}
				result.FindingID = finding.Comment.ID
				result.FindingPath = finding.Path
			}
		case "audit":
			result.SourceID = work.Passages[0]
			result.Compared = len(evidence) - 1
			result.Complete = true
			result.Title = "Extraction: " + work.Verdict
			result.Summary = "Coverage of this source passage against the selected original destination evidence; inspect qualifiers and exceptions."
		case "organization":
			result.Title = "Organization: " + work.Verdict
			result.Summary = "Suggestion only. Inspect the full notes before accepting a link or proposing consolidation."
			result.ProposalID, err = s.jevOrganizationProposal(ctx, &job, work, evidence, nil, judge)
			if err != nil {
				pause("The guarded proposal could not be saved. Continue reconciles its recorded intent.")
				return
			}
		case "tags":
			selected := []string{}
			if work.Scores["none"] < .2 {
				for i, tag := range work.Tags {
					if work.Scores[fmt.Sprintf("tag%d", i)] >= .85 {
						selected = append(selected, tag)
					}
				}
			}
			result.Details = map[string]any{"tags": selected, "probabilities": work.Scores}
			result.Title = "Tag suggestions"
			result.Status = "suggested"
			if len(selected) == 0 {
				result.Status = "none"
			}
			result.Summary = "Only existing or explicitly supplied tags are considered; more than one can fit."
			result.ProposalID, err = s.jevOrganizationProposal(ctx, &job, work, evidence, selected, judge)
			if err != nil {
				pause("The guarded tag proposal could not be saved. Continue reconciles its recorded intent.")
				return
			}
		}
		if err = s.writeJevResult(job, result); err != nil {
			// ErrInvalid: cancelled, or another run moved this job on.
			if !errors.Is(err, decision.ErrInvalid) {
				pause("The task's result could not be saved. Continue records it again.")
			}
			return
		}
		work.Proposal = nil
		work.Finding = nil
		work.Scores = nil
		job.Cursor++
		if !s.checkpointJevJob(&job) {
			return
		}
	}
	if err = s.checkJevJob(ctx, job, judge); err != nil {
		pause("Sources changed before completion; start a fresh review.")
		return
	}
	job.State = "completed"
	job.Message = "Finished the recorded candidate comparisons. This is not exhaustive verification of all notebook facts."
	s.settleJevJob(&job)
}

func (s *Server) publishJevFinding(ctx context.Context, job *jevJob, work *jevJobWork, evidence []knowledge.JudgmentEvidence, judge *decision.Service) (workspace.ReviewFinding, error) {
	store, err := s.catalog.Access(job.Owner, job.Project, "comment")
	if err != nil {
		return workspace.ReviewFinding{}, err
	}
	ids := make([]string, len(work.Passages))
	for i, passage := range work.Passages {
		ids[i] = passage + "\x00" + evidence[i].Hash
	}
	sort.Strings(ids)
	raw, _ := json.Marshal(ids)
	key := "jev-consistency-" + digest(string(raw))
	findings, err := store.Findings(ctx)
	if err != nil {
		return workspace.ReviewFinding{}, err
	}
	for _, finding := range findings {
		if finding.Comment.Finding.Key == key && !finding.Comment.Finding.Stale {
			return finding, nil
		}
	}
	if work.Finding == nil {
		state, err := store.Status(ctx)
		if err != nil {
			return workspace.ReviewFinding{}, err
		}
		input := workspace.FindingInput{Key: key, Path: evidence[0].Path, Revision: state.Revision, Hash: evidence[0].Hash, Title: "Possible contradiction", Text: "Jev found potentially incompatible assertions about the same entity, time and scope. Inspect the original excerpts; no statement is assumed correct. This observation does not authorize a correction.", Evidence: []workspace.SourceEvidence{}}
		for _, source := range evidence {
			input.Evidence = append(input.Evidence, workspace.SourceEvidence{Path: source.Path, Revision: source.Revision, Hash: source.Hash, Explanation: jevFindingExcerpt(source.Text)})
			input.Text += "\n\n" + source.Path + ":\n> " + strings.ReplaceAll(jevFindingExcerpt(source.Text), "\n", "\n> ")
		}
		work.Finding = &input
		if err = s.saveJevJob(job); err != nil {
			return workspace.ReviewFinding{}, err
		}
	}
	if err = s.checkJevJob(ctx, *job, judge); err != nil {
		return workspace.ReviewFinding{}, err
	}
	scope := workspace.MutationScope{Version: 1, Content: jevActiveScope(), Target: work.Finding.Path}
	for _, source := range evidence {
		scope.Sources = append(scope.Sources, workspace.MutationSource{Path: source.Path, Revision: source.Revision, Hash: source.Hash})
	}
	return store.AddFinding(workspace.WithMutationScope(ctx, scope), *work.Finding, job.Owner)
}

func jevFindingExcerpt(text string) string {
	if len(text) <= 1900 {
		return text
	}
	runes := []rune(text)
	for len(string(runes)) > 1900 {
		runes = runes[:len(runes)-1]
	}
	return string(runes) + "…"
}
