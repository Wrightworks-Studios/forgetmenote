package chatrun

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"forgetmenote/internal/chat"
	"forgetmenote/internal/citationchecks"
	"forgetmenote/internal/decision"
	"forgetmenote/internal/knowledge"
	"forgetmenote/internal/webcore"
	"forgetmenote/internal/workspace"
)

type judgmentCitationItem = citationchecks.Item

type judgmentCitationCheck = citationchecks.Check

type judgmentCitationRecord = citationchecks.Record

func citationRecordName(user, id string) string { return citationchecks.RecordName(user, id) }

func (s *Service) citationRecord(user, project, id string) (judgmentCitationRecord, error) {
	record, _, err := s.hooks.Citations().Snapshot(user, project, id, "")
	return record, err
}

func (s *Service) saveCitationCheck(user, project, id, run string, check judgmentCitationCheck) error {
	return s.hooks.Citations().Save(user, project, id, run, check)
}

func (s *Service) judgmentCitations(w http.ResponseWriter, r *http.Request) {
	// Request-side validation also decodes a conversation. Serialize these short
	// reads independently of the bounded background preparation workers.
	releaseRead, err := s.hooks.Citations().AdmitRead(r.Context())
	if err != nil {
		webcore.Fail(w, err)
		return
	}
	defer releaseRead()

	c, err := s.chatAuthorized(r)
	if err != nil {
		webcore.Fail(w, err)
		return
	}
	run := c.Run(r.PathValue("run"))
	if run == nil {
		webcore.Fail(w, chat.ErrNotFound)
		return
	}
	if r.Method == "POST" {
		if err = s.startCitationCheck(s.hooks.RunAuth(r, c), c.ID, run.ID, true); err != nil {
			webcore.Fail(w, err)
			return
		}
	}
	fence, release := s.hooks.Chat().RetainContent(c.Owner, c.ID)
	defer release()
	fence.RLock()
	defer fence.RUnlock()
	if fence.Deleted {
		webcore.Fail(w, chat.ErrNotFound)
		return
	}
	record, active, err := s.hooks.Citations().Snapshot(c.Owner, c.Project, c.ID, run.ID)
	if err != nil {
		webcore.Fail(w, err)
		return
	}
	check, ok := record.Checks[run.ID]
	if !ok {
		webcore.JSON(w, judgmentCitationCheck{State: "disabled", Items: []judgmentCitationItem{}, Message: "Citation checks have not run for this answer."})
		return
	}
	if !active && (check.State == "running" || check.State == "pending") {
		check.State = "incomplete"
		check.Message = "The check was interrupted. Check again to continue with a fresh evaluation."
	}
	ctx, err := chat.WithRunScope(r.Context(), c, *run)
	if err != nil {
		webcore.Fail(w, err)
		return
	}
	if check.Fingerprint != "" {
		fingerprint, err := s.citationFingerprint(ctx, c.Owner, c.Project)
		if err != nil || fingerprint != check.Fingerprint {
			webcore.JSON(w, judgmentCitationCheck{State: "incomplete", Items: []judgmentCitationItem{}, Total: check.Total, Message: "The notebook changed. Check again before using these results."})
			return
		}
	}
	for _, item := range check.Items {
		for _, evidence := range item.Evidence {
			if _, err := s.citationEvidence(ctx, c.Owner, c.Project, evidence); err != nil {
				webcore.JSON(w, judgmentCitationCheck{State: "incomplete", Items: []judgmentCitationItem{}, Total: check.Total, Message: "Cited sources or access changed. Check again before using these results."})
				return
			}
		}
	}
	if _, err = s.chatAuthorized(r); err != nil {
		webcore.Fail(w, err)
		return
	}
	webcore.JSON(w, check)
}

func (s *Service) startCitationCheck(auth chat.RunAuth, id, runID string, explicit bool) error {
	judge, localOnly := s.hooks.JudgmentsConfigured()
	if judge == nil || localOnly {
		if explicit {
			return decision.ErrDisabled
		}
		return nil
	}
	// Operator configuration, default-off personal consent, and the citations
	// preference are all required before any source is prepared for remote work.
	if err := s.hooks.JudgmentsAuthorized(context.Background(), auth.User, auth.Project, "citations", judge); err != nil {
		if explicit {
			return err
		}
		return nil
	}

	fence, release := s.hooks.Chat().RetainContent(auth.User, id)
	defer release()
	fence.RLock()
	defer fence.RUnlock()
	if fence.Deleted {
		return chat.ErrNotFound
	}
	held, err := s.db.Shared(context.Background(), conversationLock(auth.User, id))
	if err != nil {
		return err
	}
	defer held()
	// A deletion through another gateway has finished; it leaves no record to
	// check against.
	if _, err = s.hooks.Chat().Store.Read(auth.User, auth.Project, id); err != nil {
		return err
	}
	return s.hooks.Citations().Start(auth.User, auth.Project, id, runID, explicit, func(ctx context.Context, checkID string) {
		s.prepareCitationCheck(ctx, auth, id, runID, checkID, judge)
	})
}

func (s *Service) prepareCitationCheck(ctx context.Context, auth chat.RunAuth, id, runID, checkID string, judge *decision.Service) {
	// Admission precedes every retained conversation, scope and claim preparation.
	if s.hooks.CheckRun(auth) != nil || s.hooks.JudgmentsAuthorized(ctx, auth.User, auth.Project, "citations", judge) != nil {
		return
	}
	fence, release := s.hooks.Chat().RetainContent(auth.User, id)
	defer release()
	fence.RLock()
	defer fence.RUnlock()
	if fence.Deleted {
		return
	}
	held, err := s.db.Shared(ctx, conversationLock(auth.User, id))
	if err != nil {
		return
	}
	defer held()
	c, err := s.hooks.Chat().Store.Read(auth.User, auth.Project, id)
	if err != nil {
		return
	}
	run := c.Run(runID)
	if run == nil {
		return
	}
	var turn chat.Turn
	for i := len(c.Turns) - 1; i >= 0; i-- {
		if c.Turns[i].RunID == runID && c.Turns[i].Role == "assistant" {
			turn = c.Turns[i]
			break
		}
	}
	if turn.ID == "" {
		return
	}
	record, err := s.citationRecord(c.Owner, c.Project, c.ID)
	if err != nil {
		return
	}
	check, ok := record.Checks[runID]
	if !ok || check.ID != checkID {
		return
	}
	check.Turn = turn.ID
	s.runCitationCheck(ctx, auth, c, *run, turn, check, judge)
}

var quotedClaim = regexp.MustCompile(`["“]([^"”]{2,})["”]`)

func normalizedQuote(text string) string {
	return strings.Join(strings.Fields(strings.NewReplacer("“", "\"", "”", "\"", "‘", "'", "’", "'").Replace(text)), " ")
}

func (s *Service) citationEvidence(ctx context.Context, user, project string, evidence knowledge.JudgmentEvidence) (workspace.Document, error) {
	if !s.catalog.Enabled(user) {
		return workspace.Document{}, workspace.ErrForbidden
	}
	if evidence.Project != "" {
		project = evidence.Project
	}
	ctx, err := s.DocumentScope(ctx, project, "notes", "", "", evidence.Path)
	if err != nil {
		return workspace.Document{}, err
	}
	store, err := s.catalog.Access(user, project, "read")
	if err != nil {
		return workspace.Document{}, err
	}
	doc, current, err := ReadNoteSnapshot(ctx, store, evidence.Path, evidence.Revision)
	if err != nil {
		return doc, err
	}
	if evidence.Hash == "" || workspace.ContentHash(doc.Content) != evidence.Hash || evidence.Text == "" || !strings.Contains(normalizedQuote(string(doc.Content)), normalizedQuote(evidence.Text)) {
		return doc, workspace.ErrConflict
	}
	// Canonical notebook evidence is pinned to its own notebook, whose changes
	// are not covered by the conversation notebook's fingerprint.
	if evidence.Project != "" {
		scope := chat.RunScopeFrom(ctx)
		if scope == nil {
			return doc, chat.ErrCorpus
		}
		attached := false
		if project == scope.Project {
			for _, attachment := range scope.Origin.Attachments {
				if attachment.Kind == "committed" && attachment.Path == evidence.Path && attachment.Revision == evidence.Revision && workspace.ContentHash([]byte(attachment.Content)) == evidence.Hash {
					attached = true
					break
				}
			}
		}
		if !scope.Run.Options.Corpus.History && !attached {
			eligible := false
			for _, file := range current.Files {
				if file.Path == evidence.Path {
					eligible = true
					if file.Hash != evidence.Hash {
						return doc, workspace.ErrConflict
					}
					break
				}
			}
			if !eligible {
				return doc, chat.ErrOutsideCorpus
			}
		}
	}
	return doc, nil
}

type proposalQuoteSource struct {
	Path, Revision, Hash string
}

// Each citation check prepares proposal edits only if a quote is absent from
// its source passage. Retained preimages are irrelevant to this comparison.
func retainedProposalQuoteMatcher(ctx context.Context, run chat.Run, project string) func(knowledge.JudgmentEvidence, string) (bool, error) {
	var edits map[proposalQuoteSource][]string
	prepared := false
	return func(evidence knowledge.JudgmentEvidence, quote string) (bool, error) {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if evidence.Project != "" && evidence.Project != project {
			return false, nil
		}
		if !prepared {
			var err error
			edits, err = prepareProposalQuotes(ctx, run, project)
			if err != nil {
				return false, err
			}
			prepared = true
		}
		normalized := normalizedQuote(quote)
		for _, content := range edits[proposalQuoteSource{evidence.Path, evidence.Revision, evidence.Hash}] {
			if err := ctx.Err(); err != nil {
				return false, err
			}
			if strings.Contains(content, normalized) {
				return true, nil
			}
		}
		return false, ctx.Err()
	}
}

func prepareProposalQuotes(ctx context.Context, run chat.Run, project string) (map[proposalQuoteSource][]string, error) {
	edits := make(map[proposalQuoteSource][]string)
	for _, operation := range run.Operations {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if operation.Name != "submit_change_group" || operation.State != "completed" || operation.Error != "" {
			continue
		}
		var proposal struct {
			ID          string              `json:"id"`
			Status      string              `json:"status"`
			RequestHash string              `json:"requestHash"`
			Group       workspace.EditGroup `json:"group"`
		}
		if json.Unmarshal(operation.Result, &proposal) != nil || proposal.ID == "" || proposal.Status != "proposed" {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var input struct {
			Project string              `json:"project"`
			Mode    string              `json:"mode"`
			Group   workspace.EditGroup `json:"group"`
		}
		if json.Unmarshal(operation.Arguments, &input) != nil || input.Project != project || input.Mode != "proposal" {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		requested, _ := json.Marshal(input.Group)
		if workspace.ContentHash(requested) != proposal.RequestHash {
			continue
		}
		for _, edit := range proposal.Group.Edits {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if !edit.Delete {
				source := proposalQuoteSource{edit.Path, edit.Revision, edit.Hash}
				edits[source] = append(edits[source], normalizedQuote(edit.Content))
			}
		}
	}
	return edits, ctx.Err()
}

func (s *Service) runCitationCheck(ctx context.Context, auth chat.RunAuth, c chat.Conversation, run chat.Run, turn chat.Turn, check judgmentCitationCheck, judge *decision.Service) {
	var err error
	ctx, err = chat.WithRunScope(ctx, c, run)
	if err != nil {
		return
	}
	account, err := s.catalog.Account(c.Owner)
	if err != nil {
		return
	}
	check.Fingerprint, err = s.citationFingerprint(ctx, c.Owner, c.Project)
	if err != nil {
		return
	}
	claims := knowledge.CitationClaims(turn.Content)
	check.Total = len(claims)
	check.State = "running"
	citations := map[string]chat.Citation{}
	for _, citation := range turn.Citations {
		citations[citation.URL] = citation
	}
	proposalQuote := retainedProposalQuoteMatcher(ctx, run, c.Project)
	for _, claim := range claims[:min(128, len(claims))] {
		if ctx.Err() != nil {
			return
		}
		item := judgmentCitationItem{Claim: claim.Claim, Status: "unchecked", Evidence: []knowledge.JudgmentEvidence{}}
		if claim.Mapped {
			if citation, ok := citations[claim.URLs[0]]; ok && (citation.Kind == "committed" || citation.Kind == "notebook" && citation.Project != "") && citation.Path != "" {
				evidence := knowledge.JudgmentEvidence{Path: citation.Path, Revision: citation.Revision, Hash: citation.Hash, Text: citation.Text, URL: citation.URL}
				if citation.Kind == "notebook" {
					evidence.Project = citation.Project
				}
				if _, err := s.citationEvidence(ctx, c.Owner, c.Project, evidence); err == nil {
					item.Evidence = []knowledge.JudgmentEvidence{evidence}
					item.Status = "pending"
					for _, quote := range quotedClaim.FindAllStringSubmatch(claim.Claim, -1) {
						if !strings.Contains(normalizedQuote(evidence.Text), normalizedQuote(quote[1])) {
							// A retained proposal can explain new quoted text without
							// making it a quotation from the cited preimage. This claim
							// remains unchecked by the note-passage verifier.
							matched, err := proposalQuote(evidence, quote[1])
							if err != nil {
								return
							}
							if matched {
								item.Status = "unchecked"
								continue
							}
							item.Status = "invalid_quote"
							check.Checked++
							break
						}
					}
				}
			}
		}
		check.Items = append(check.Items, item)
	}
	if err = s.saveCitationCheck(c.Owner, c.Project, c.ID, run.ID, check); err != nil {
		return
	}
	for i := range check.Items {
		item := &check.Items[i]
		if item.Status != "pending" {
			continue
		}
		request := knowledge.ClaimSupportRequest([]knowledge.ClaimEvidence{{Claim: item.Claim, Evidence: item.Evidence}})
		validate := func(ctx context.Context) error {
			if err := s.hooks.CheckRun(auth); err != nil {
				return err
			}
			if err := s.hooks.JudgmentsAuthorized(ctx, c.Owner, c.Project, "citations", judge); err != nil {
				return err
			}
			fingerprint, err := s.citationFingerprint(ctx, c.Owner, c.Project)
			if err != nil {
				return err
			}
			if fingerprint != check.Fingerprint {
				return workspace.ErrConflict
			}
			for _, evidence := range item.Evidence {
				if _, err := s.citationEvidence(ctx, c.Owner, c.Project, evidence); err != nil {
					return err
				}
			}
			return nil
		}
		result, callErr := judge.Evaluate(ctx, decision.Evaluation{Scope: decision.Scope{Actor: account.ID, Notebook: c.Project, Feature: "citations", Run: run.ID, Chat: c.ID, Operation: check.ID + "-" + fmt.Sprint(i)}, Request: request, Check: validate})
		if callErr != nil {
			item.Status = "unchecked"
			continue
		}
		answer := result.Answers["c0"]
		item.Status = answer.Choice
		if answer.Confidence != nil {
			item.Confidence = *answer.Confidence
			if *answer.Confidence < .6 {
				item.Status = "uncertain"
			}
		}
		check.Checked++
		if err = s.saveCitationCheck(c.Owner, c.Project, c.ID, run.ID, check); err != nil {
			return
		}
	}
	check.State = "completed"
	check.Message = "Judgments check the cited passages, not the truth or completeness of the whole answer."
	if check.Checked < check.Total {
		check.State = "incomplete"
		check.Message = "Some citation associations or sources could not be checked; unchecked text is not verified."
	}
	if s.hooks.CheckRun(auth) != nil || s.hooks.JudgmentsAuthorized(ctx, c.Owner, c.Project, "citations", judge) != nil {
		return
	}
	s.saveCitationCheck(c.Owner, c.Project, c.ID, run.ID, check)
}

// Review bookkeeping changes HEAD without changing source freshness. Bind the
// currently authorized corpus by content and visibility, not its Git commit ID.
func (s *Service) citationFingerprint(ctx context.Context, user, project string) (string, error) {
	store, err := s.catalog.Access(user, project, "read")
	if err != nil {
		return "", err
	}
	snapshot, err := ScopedSnapshot(ctx, store, "")
	if err != nil {
		return "", err
	}
	values := []string{snapshot.Visibility, snapshot.ScopeID}
	for _, file := range snapshot.Files {
		values = append(values, file.Path+"\x00"+file.Hash)
	}
	raw, _ := json.Marshal(values)
	return workspace.ContentHash(raw), nil
}
