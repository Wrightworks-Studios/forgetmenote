package jev

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"forgetmenote/internal/attribution"
	"forgetmenote/internal/auth"
	"forgetmenote/internal/decision"
	"forgetmenote/internal/knowledge"
	"forgetmenote/internal/workspace"
	"path"
	"strconv"
	"strings"
)

// Conservative initial policy, not model-quality guarantees. Duplicate removal
// additionally requires complete notes and deterministic dependency validation.
const organizationTagThreshold = .95
const organizationLinkThreshold = .95
const organizationMoveThreshold = .98
const organizationDuplicateThreshold = .995

func organizationChoice(a decision.Answer, choice string, minimum float64) bool {
	if a.Choice != choice || a.Confidence == nil || *a.Confidence < .9 || a.Probabilities[choice] < minimum {
		return false
	}
	for key, prob := range a.Probabilities {
		if key != choice && a.Probabilities[choice]-prob < .5 {
			return false
		}
	}
	return true
}
func organizationCurrentPath(job jevJob, p string) string {
	if current, ok := job.Organization.Paths[p]; ok {
		return current
	}
	return p
}
func (s *Service) organizationEvidence(ctx context.Context, store *workspace.Store, job jevJob, p string) (knowledge.JudgmentEvidence, error) {
	p = organizationCurrentPath(job, p)
	for _, note := range job.Notes {
		if note.Path == p {
			source := workspace.MutationSource{Path: p, Revision: note.Revision, Hash: note.Hash}
			if err := store.OrganizationSources(ctx, []workspace.MutationSource{source}); err != nil {
				return knowledge.JudgmentEvidence{}, err
			}
			doc, err := store.KnowledgeRead(ctx, p, note.Revision)
			if err != nil {
				return knowledge.JudgmentEvidence{}, err
			}
			if len(doc.Content) > 24<<10 {
				return knowledge.JudgmentEvidence{}, errors.New("complete note exceeds 24 KiB verification input; review manually")
			}
			return knowledge.JudgmentEvidence{Path: p, Revision: note.Revision, Hash: note.Hash, Text: string(doc.Content), URL: evidenceURL(p, note.Revision)}, nil
		}
	}
	return knowledge.JudgmentEvidence{}, errors.New("note was removed by an earlier action")
}
func (s *Service) prepareOrganizationAction(ctx context.Context, store *workspace.Store, job jevJob, action *organizationAction) (decision.Request, []knowledge.JudgmentEvidence, error) {
	question, evidence, err := organizationRequest(job, action, func(p string) (knowledge.JudgmentEvidence, error) {
		return s.organizationEvidence(ctx, store, job, p)
	})
	if err != nil {
		return question, evidence, err
	}
	request := action.Request
	request.Allowed = append([]string{}, job.Organization.Allowed...)
	allowed := make(map[string]bool, len(request.Allowed))
	for _, p := range request.Allowed {
		allowed[p] = true
	}
	for _, note := range job.Notes {
		if allowed[note.Path] {
			request.Expected = append(request.Expected, workspace.MutationSource{Path: note.Path, Revision: note.Revision, Hash: note.Hash})
		}
	}
	return question, evidence, nil
}

// One builder owns evidence selection and fingerprints. Actual model work uses
// freshly authorized reads; Undo only needs fingerprints from its pinned snapshot.
func organizationRequest(job jevJob, action *organizationAction, read func(string) (knowledge.JudgmentEvidence, error)) (decision.Request, []knowledge.JudgmentEvidence, error) {
	source, err := read(action.Path)
	if err != nil {
		return decision.Request{}, nil, err
	}
	evidence := []knowledge.JudgmentEvidence{source}
	policy := job.Organization.Input.Policy
	request := workspace.OrganizationAction{ID: job.ID + ":" + strconv.Itoa(job.Cursor), Kind: action.Kind, Path: source.Path, Folders: policy.Folders}
	var question decision.Request
	switch action.Kind {
	case "tags":
		question = knowledge.TagRequest(source, action.Vocabulary)
	case "links", "duplicate":
		target, err := read(action.Target)
		if err != nil {
			return question, evidence, err
		}
		evidence = append(evidence, target)
		request.Target = target.Path
		if action.Kind == "links" {
			question = knowledge.OrganizationRequest(evidence)
		} else {
			question = knowledge.DuplicateNoteRequest(source, target)
		}
	case "move":
		destinations := []knowledge.OrganizationDestination{}
		for _, folder := range policy.Folders {
			d := knowledge.OrganizationDestination{Folder: folder, Context: []knowledge.JudgmentEvidence{}}
			for _, note := range job.Notes {
				dir := path.Dir(note.Path)
				if dir == "." {
					dir = ""
				}
				if dir != folder || note.Path == source.Path {
					continue
				}
				candidate, err := read(note.Path)
				if err != nil {
					continue
				}
				d.Context = append(d.Context, candidate)
				evidence = append(evidence, candidate)
				break
			}
			destinations = append(destinations, d)
		}
		question = knowledge.OrganizationFolderRequest(source, destinations)
	default:
		return question, evidence, decision.ErrInvalid
	}
	total := 0
	for _, e := range evidence {
		total += len(e.Text)
		request.Sources = append(request.Sources, workspace.MutationSource{Path: e.Path, Revision: e.Revision, Hash: e.Hash})
	}
	if total > 48<<10 {
		return question, evidence, errors.New("complete verification exceeds 48 KiB; no changes applied")
	}
	keySources := []string{}
	for _, source := range request.Sources {
		keySources = append(keySources, source.Path+"\x00"+source.Hash)
	}
	key, _ := json.Marshal([]any{action.Kind, source.Path, request.Target, keySources, action.Vocabulary, policy.Folders})
	action.Key = auth.Digest(string(key))
	action.Request = &request
	return question, evidence, nil
}
func organizationEvidenceForAction(a *organizationAction) []knowledge.JudgmentEvidence {
	evidence := []knowledge.JudgmentEvidence{}
	if a.Request != nil {
		for _, source := range a.Request.Sources {
			evidence = append(evidence, knowledge.JudgmentEvidence{Path: source.Path, Revision: source.Revision, Hash: source.Hash, URL: evidenceURL(source.Path, source.Revision)})
		}
	}
	if len(evidence) == 0 {
		evidence = append(evidence, knowledge.JudgmentEvidence{Path: a.Path})
	}
	return evidence
}

// runOrganizationJob advances job by a bounded batch. Every return leaves it
// settled with a message (paused, queued for the next batch or completed) or
// cancelled through another path; a save that fails pauses it instead.
func (s *Service) runOrganizationJob(ctx context.Context, job jevJob) {
	pause := func(message string) { job.State = "paused"; job.Message = message; s.settleJevJob(&job) }
	judge, _ := s.hooks.Judge()
	account, err := s.catalog.Account(job.Owner)
	if err != nil {
		pause("The account that started this organization is unavailable.")
		return
	}
	ctx = attribution.WithSource(ctx, account.Identity(), "jev", "organizer")
	store, err := s.organizationNotebook(job.Owner, job.Project, "comment")
	if err != nil {
		pause("Notebook permission is unavailable.")
		return
	}
	for batch := 0; job.Cursor < len(job.Work) && batch < 4; batch++ {
		if err = s.checkOrganizationJob(ctx, job, judge); err != nil {
			pause("Stopped: access, consent, policy or configuration changed.")
			return
		}
		work := &job.Work[job.Cursor]
		a := work.Organization
		if a == nil {
			pause("Organization record is invalid.")
			return
		}
		if a.Status == "" {
			var question decision.Request
			if a.Request != nil && job.Organization.Input.Mode == "apply" {
				// Reconcile a successful commit before checking now-obsolete source guards.
				result, e := store.OrganizationReceipt(ctx, *a.Request, job.AccountID)
				if e != nil {
					pause("Commit receipt is unavailable; retry after recovery.")
					return
				}
				if result.Replayed {
					a.Status = "applied"
					a.Result = &result
					if summary := organizationTrashSummary(result.TrashLinks, organizationTrashUpdated, organizationTrashLeft); summary != "" {
						a.Reason = organizationExplanation(a.Kind, a.Status) + summary
					}
				}
			}
			if a.Status == "" && a.Answers == nil {
				if a.Attempted {
					pause("The prior judgment outcome is unknown. Continue explicitly starts a recorded retry.")
					return
				}
				question, _, err = s.prepareOrganizationAction(ctx, store, job, a)
				if err != nil {
					a.Status = "suggested"
					a.Reason = err.Error()
				}
				if a.Status == "" {
					settings, e := s.loadJevNotebook(job.Project)
					if e != nil {
						pause("Organization policy is unavailable.")
						return
					}
					if settings.Organization != nil && settings.Organization.Suppressed[a.Key] != "" && !job.Organization.Input.Reconsider {
						a.Status = "skipped"
						a.Reason = "Suppressed after Undo or dismissal. Start a run with Reconsider suppressed work to retry."
					}
				}
				if a.Status == "" && (a.Kind == "duplicate" || a.Kind == "links") {
					preflight, e := store.Organize(ctx, *a.Request, false)
					if e != nil {
						a.Status = "suggested"
						a.Reason = e.Error()
					} else if len(preflight.Files) == 0 {
						a.Status = "skipped"
						a.Reason = "The related link is already present."
					}
				}
				if a.Status == "" {
					if e := judge.ValidateRequest(question); e != nil {
						a.Status = "suggested"
						a.Reason = "Complete verification exceeds the configured request or question limits; review manually."
					}
				}
				if a.Status == "" {
					a.Attempted = true
					if !s.checkpointJevJob(&job) {
						return
					}
					result, e := judge.Evaluate(ctx, decision.Evaluation{Scope: decision.Scope{Actor: job.AccountID, Notebook: job.Project, Feature: "organization", Run: job.ID, Operation: fmt.Sprintf("organize-%d-%d", job.Cursor, work.Attempt)}, Background: true, Request: question, Check: func(check context.Context) error {
						if e := s.checkOrganizationJob(check, job, judge); e != nil {
							return e
						}
						return store.OrganizationSources(check, a.Request.Sources)
					}})
					if e != nil {
						pause("Judgment was not completed; retained usage may be unknown. Continue explicitly retries this action.")
						return
					}
					a.Answers = result.Answers
					if !s.checkpointJevJob(&job) {
						return
					}
				}
			}
			if a.Status == "" {
				s.selectOrganizationAction(&job, a)
			}
		}
		// Policy disable, consent withdrawal and cancellation wait for this bounded
		// transaction and its durable receipt publication; no next action can pass.
		s.jevState.organizationMu.Lock()
		if err = s.checkOrganizationJob(ctx, job, judge); err != nil {
			s.jevState.organizationMu.Unlock()
			pause("Stopped before publication: authority or policy changed.")
			return
		}
		if a.Status == "eligible" {
			if !s.checkpointJevJob(&job) {
				s.jevState.organizationMu.Unlock()
				return
			}
			var result workspace.OrganizationResult
			apply := job.Organization.Input.Mode == "apply"
			role := "comment"
			if apply {
				role = "owner"
			}
			err = s.catalog.WithProjectAuthority(job.Owner, job.Project, job.AccountID, role, job.Organization.AccessEpoch, job.Organization.ProjectEpoch, func(authorized *workspace.Store) error {
				var e error
				result, e = authorized.Organize(ctx, *a.Request, apply)
				return e
			})
			if err != nil {
				if errors.Is(err, workspace.ErrOrganizationOutcomeUnknown) || errors.Is(err, workspace.ErrOwnerUnavailable) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					s.jevState.organizationMu.Unlock()
					pause("Commit outcome is unknown; recover its durable receipt before continuing.")
					return
				}
				a.Status = "conflicted"
				a.Reason = err.Error()
			} else {
				a.Result = &result
				a.Status = "suggested"
				a.Reason = "Preview only; no notes were changed." + organizationTrashSummary(result.TrashLinks, "Notes in the Trash whose links to it will be updated", "Notes in the Trash that will be left as they are")
				if len(result.Files) == 0 {
					a.Status = "skipped"
					a.Reason = "This change is already present."
				} else if apply {
					a.Status = "applied"
					a.Reason = "Applied under the selected organization policy." + organizationTrashSummary(result.TrashLinks, organizationTrashUpdated, organizationTrashLeft)
				}
			}
		}
		if a.Status == "applied" && a.Result != nil {
			if err = s.advanceOrganization(ctx, &job, a); err != nil {
				s.jevState.organizationMu.Unlock()
				pause("Committed; receipt recovery is required before continuing.")
				return
			}
		}
		if a.Reason == "" {
			a.Reason = organizationExplanation(a.Kind, a.Status)
		}
		record := jevJobResult{ID: auth.Digest(job.ID + fmt.Sprint(job.Cursor)), Kind: work.Kind, Title: strings.Title(a.Kind) + ": " + a.Path, Summary: a.Reason, Status: a.Status, Evidence: organizationEvidenceForAction(a), Details: organizationDetails(*a)}
		if err = s.writeJevResult(job, record); err != nil {
			s.jevState.organizationMu.Unlock()
			// ErrInvalid: cancelled, or another run moved this job on.
			if !errors.Is(err, decision.ErrInvalid) {
				pause("The action's result could not be saved. Continue records it again.")
			}
			return
		}
		// Completed bodies live in the existing paged result records, keeping the job
		// envelope bounded and avoiding the 200-entry interactive proposal queue.
		work.Organization = &organizationAction{Kind: a.Kind, Path: a.Path, Target: a.Target, Status: a.Status, Key: a.Key}
		work.Verdict = a.Status
		job.Cursor++
		if !s.checkpointJevJob(&job) {
			s.jevState.organizationMu.Unlock()
			return
		}
		s.jevState.organizationMu.Unlock()
	}
	s.jevState.organizationMu.Lock()
	defer s.jevState.organizationMu.Unlock()
	if job.Cursor == len(job.Work) {
		if err := s.recordOrganizationCompletion(job); err != nil {
			pause("Could not checkpoint automatic organization coverage; continue to recover.")
			return
		}
		job.State = "completed"
		job.Message = "Finished the selected actions and retrieved candidates. Suggested or blocked actions need review; candidate coverage is not exhaustive."
	} else {
		job.State = "queued"
		job.Message = "Continuing in bounded batches; partial applied changes have individual receipts and Undo."
	}
	s.settleJevJob(&job)
}
func (s *Service) selectOrganizationAction(job *jevJob, a *organizationAction) {
	policy := job.Organization.Input.Policy
	a.Status = "suggested"
	a.Reason = "Uncertain judgment or action threshold not met. No change applied."
	switch a.Kind {
	case "tags":
		none := a.Answers["none"].Probability
		if none == nil || *none >= .05 {
			return
		}
		for i, tag := range a.Vocabulary {
			score := a.Answers[fmt.Sprintf("tag%d", i)].Probability
			if score != nil && *score >= organizationTagThreshold {
				a.Request.Tags = append(a.Request.Tags, tag)
			}
		}
		if len(a.Request.Tags) == 0 {
			return
		}
	case "links":
		choice := a.Answers["assessment"].Choice
		if choice != "related" && choice != "complementary" || !organizationChoice(a.Answers["assessment"], choice, organizationLinkThreshold) {
			return
		}
	case "duplicate":
		if !organizationChoice(a.Answers["assessment"], "redundant", organizationDuplicateThreshold) {
			return
		}
	case "move":
		answer := a.Answers["folder"]
		if answer.Choice == "keep" {
			a.Status = "skipped"
			a.Reason = "Keep the current folder."
			return
		}
		if !strings.HasPrefix(answer.Choice, "folder") || !organizationChoice(answer, answer.Choice, organizationMoveThreshold) {
			return
		}
		index, err := strconv.Atoi(strings.TrimPrefix(answer.Choice, "folder"))
		if err != nil || index < 0 || index >= len(policy.Folders) {
			return
		}
		a.Request.Target = path.Join(policy.Folders[index], path.Base(a.Request.Path))
		if a.Request.Target == a.Request.Path {
			a.Status = "skipped"
			a.Reason = "Already in the selected folder."
			return
		}
	default:
		return
	}
	a.Status = "eligible"
	a.Reason = ""
}
func (s *Service) advanceOrganization(ctx context.Context, job *jevJob, a *organizationAction) error {
	// Consume the exact committed output, never refresh a stale judgment from HEAD.
	for _, f := range a.Result.Files {
		if !strings.HasSuffix(f.Path, ".md") || workspace.IsTrash(f.Path) || strings.HasPrefix(f.Path, "comments/") {
			continue
		}
		found := -1
		for i := range job.Notes {
			if job.Notes[i].Path == f.Path {
				found = i
				break
			}
		}
		if f.After == nil {
			if found >= 0 {
				job.Notes = append(job.Notes[:found], job.Notes[found+1:]...)
			}
			continue
		}
		hash := workspace.ContentHash([]byte(*f.After))
		note := jevJobNote{Path: f.Path, Revision: a.Result.Revision, Hash: hash, CurrentHash: hash}
		if found >= 0 {
			job.Notes[found] = note
		} else {
			job.Notes = append(job.Notes, note)
		}
	}
	if a.Kind == "move" {
		job.Organization.Paths[a.Path] = a.Request.Target
		for i, p := range job.Organization.Allowed {
			if p == a.Request.Path {
				job.Organization.Allowed[i] = a.Request.Target
			}
		}
	}
	if a.Kind == "duplicate" {
		job.Organization.Paths[a.Path] = ""
	}
	return s.recordOrganizationBaseline(job.Project, a.Result)
}

func organizationDetails(a organizationAction) any {
	raw, _ := json.Marshal(a)
	var value any
	json.Unmarshal(raw, &value)
	return value
}
