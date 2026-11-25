package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/knowledge"
	"forgetmenote/internal/workspace"
	"net/url"
	"sort"
	"strings"
)

const reviewChunkRunes = 1200

func reviewChunks(content string) []string {
	text := []rune(content)
	parts := []string{}
	for len(text) > 0 {
		n := min(reviewChunkRunes, len(text))
		parts = append(parts, string(text[:n]))
		text = text[n:]
	}
	if len(parts) == 0 {
		parts = append(parts, "[Empty committed note]")
	}
	return parts
}
func reviewComplete(r *chat.Coverage) bool {
	for _, n := range r.Notes {
		if n.State != "examined" || n.Cursor != n.Total || !n.Provenance.Complete || !n.Provenance.AttachmentsTracked {
			return false
		}
	}
	return true
}
func reviewProgress(r *chat.Coverage) map[string]any {
	examined, parts, total := 0, 0, 0
	for _, n := range r.Notes {
		if n.State == "examined" {
			examined++
		}
		parts += n.Cursor
		total += n.Total
	}
	return map[string]any{"corpusId": r.CorpusID, "historyLimited": r.HistoryLimited, "coverageMeaning": "All eligible current notes only. Historical comparisons are bounded and never exhaustive historical coverage.", "revision": r.Revision, "examined": examined, "total": len(r.Notes), "passagesExamined": parts, "passagesTotal": total, "complete": r.Complete, "message": fmt.Sprintf("Examined %d of %d notes (%d of %d original passages)", examined, len(r.Notes), parts, total)}
}

// prepareChatReview inventories authorized committed content. Every batch contains
// one original note passage; extra comparison passages never count as coverage.
// A resumed or concurrently changed notebook requeues obsolete analysis.
func (s *Server) prepareChatReview(ctx context.Context, a chat.RunAuth, id, runID string, connection chat.Connection) (bool, error) {
	if err := s.checkRun(a); err != nil {
		return false, err
	}
	store, err := s.catalog.Access(a.User, a.Project, "read")
	if err != nil {
		return false, err
	}
	c, err := s.chat.Store.Read(a.User, a.Project, id)
	if err != nil {
		return false, err
	}
	run := c.Run(runID)
	if run == nil {
		return false, chat.ErrNotFound
	}
	ctx, err = chat.WithRunScope(ctx, c, *run)
	if err != nil {
		return false, err
	}
	snapshot, err := chatCorpusSnapshot(ctx, store, *run, "")
	if err != nil {
		return false, err
	}
	guidance, instructionsHash, err := s.chatInstructions(ctx, store, snapshot.Revision)
	if err != nil {
		return false, err
	}
	visible := snapshot.Files[:0]
	for _, f := range snapshot.Files {
		if !workspace.IsTrash(f.Path) {
			visible = append(visible, f)
		}
	}
	snapshot.Files = visible
	sort.Slice(snapshot.Files, func(i, j int) bool { return snapshot.Files[i].Path < snapshot.Files[j].Path })
	references := knowledge.ReviewFingerprints(snapshot)

	var original chat.Turn
	for _, t := range c.Turns {
		if t.RunID == runID && t.Role == "user" {
			original = t
			break
		}
	}
	if original.ID == "" {
		return false, errors.New("review is missing its original user turn")
	}
	purpose, attachments := original.Content, original.Attachments
	validCommitted := chatEvidenceValidator(snapshot, instructionsHash, []chat.Turn{original}, run.Options.Corpus.ID(), chatHistoricalValidator(ctx, store, *run, snapshot))
	validEvidence := func(provenance chat.Provenance, citations []chat.Citation) bool {
		if !reviewExternalEvidenceValid(provenance, citations, reviewExternalAllowance(run)) {
			return false
		}
		provenance.ExternalAllowance, provenance.ExternalSources = "", nil
		committed := []chat.Citation{}
		for _, source := range citations {
			if source.Kind != "external" {
				committed = append(committed, source)
			}
		}
		return validCommitted(provenance, committed)
	}
	inputs, _ := json.Marshal(struct {
		Mode         string
		Actions      []string
		Research     bool
		Attachments  []chat.Attachment
		System       string
		Instructions string
		CorpusID     string
	}{run.Options.Mode, run.Options.Actions, run.Options.Research, attachments, chatSystem, instructionsHash, run.Options.Corpus.ID()})
	old := run.Review
	coverage := chat.Coverage{CorpusID: run.Options.Corpus.ID(), HistoryLimited: run.Options.Corpus.History, InputHash: chat.Hash(inputs), Revision: snapshot.Revision, Visibility: snapshot.Visibility, Purpose: purpose, Model: connection.ID + ":" + run.Options.Model, Notes: []chat.ReviewNote{}}
	reusable := map[string]chat.ReviewNote{}
	compatible := old != nil && old.Model == coverage.Model && old.Purpose == purpose && old.InputHash == coverage.InputHash
	if compatible {
		for _, n := range old.Notes {
			// Stop this allowance after an unverifiable fresh result. Explicit
			// resume returns queued and permits a new bounded attempt.
			if n.State == "unverified" && run.State == "running" {
				return false, errors.New("review evidence dependencies could not be verified within their storage limits; progress remains incomplete; inspect evidence before continuing")
			}
			reusable[n.Path] = n
		}
	}
	// Previous completed analyses can nominate comparison candidates in this chat;
	// coverage itself is reused only for the identical user purpose and model.
	if old == nil {
		for _, previous := range c.Runs {
			if previous.ID == runID || previous.Review == nil || previous.Review.Purpose != purpose || previous.Review.Model != coverage.Model || previous.Review.InputHash != coverage.InputHash {
				continue
			}
			for _, n := range previous.Review.Notes {
				if n.State == "examined" {
					reusable[n.Path] = n
				}
			}
		}
	}
	files := map[string]workspace.SnapshotFile{}
	for _, f := range snapshot.Files {
		files[f.Path] = f
		parts := reviewChunks(f.Content)
		n := chat.ReviewNote{Path: f.Path, Revision: snapshot.Revision, Hash: f.Hash, References: references[f.Path], State: "pending", Total: len(parts)}
		if cached, ok := reusable[f.Path]; ok && cached.Hash == f.Hash && cached.References == n.References && cached.Total == n.Total {
			untouched := cached.State == "pending" && cached.Cursor == 0 && cached.Summary == ""
			analyzed := cached.Cursor > 0 && cached.Cursor <= cached.Total && (cached.State == "pending" && cached.Cursor < cached.Total || cached.State == "examined" && cached.Cursor == cached.Total)
			if untouched || analyzed && validReviewSources(cached.Sources) && validEvidence(cached.Provenance, cached.Sources) {
				n = cached
			}
		}
		coverage.Notes = append(coverage.Notes, n)
	}
	// Preserve a prepared batch across a normal tool continuation, provided its
	// exact content, source fingerprint and notebook guidance remain current.
	_, preparedSourcesComplete := mergeReviewSources(run.Citations)
	if compatible && old.BatchPath != "" && preparedSourcesComplete && validEvidence(run.Provenance, run.Citations) {
		for _, n := range coverage.Notes {
			if n.Path == old.BatchPath && n.Cursor == old.BatchCursor && n.State == "pending" {
				prior := reusable[n.Path]
				if prior.Hash == n.Hash && prior.References == n.References {
					coverage.BatchPath = old.BatchPath
					coverage.BatchCursor = old.BatchCursor
				}
			}
		}
	}
	if coverage.BatchPath != "" {
		return false, s.chatRunUpdate(a, id, runID, func(c *chat.Conversation, r *chat.Run) error {
			r.Review = &coverage
			r.Provenance.InstructionsHash = instructionsHash
			return nil
		})
	}
	coverage.Complete = reviewComplete(&coverage)
	var messages []chat.Message
	var citations []chat.Citation
	provenance := chat.Provenance{CorpusID: run.Options.Corpus.ID(), Complete: true, Revision: snapshot.Revision, Visibility: snapshot.Visibility, InstructionsHash: instructionsHash}
	attachmentSources, err := s.chatAttachmentProvenance(ctx, store, original, &provenance)
	if err != nil {
		return false, err
	}
	if !coverage.Complete {
		var selected *chat.ReviewNote
		for i := range coverage.Notes {
			if coverage.Notes[i].State != "examined" {
				selected = &coverage.Notes[i]
				break
			}
		}
		parts := reviewChunks(files[selected.Path].Content)
		text := parts[selected.Cursor]
		link := url.URL{Path: "/" + selected.Path, RawQuery: url.Values{"fmn-rev": {snapshot.Revision}}.Encode()}
		primary := chat.Citation{ID: "review-original", Path: selected.Path, URL: link.String(), Title: selected.Path, Text: text, Revision: snapshot.Revision, Hash: selected.Hash, Kind: "committed"}
		citations = append(citations, primary)
		cfg := s.chat.Snapshot().Config
		query := textLimit(purpose+" "+text, 1800)
		related, err := withEngine(s, ctx, store, func(e *knowledge.Engine) (chatRetrieval, error) {
			return s.chatRetrieve(ctx, id, *run, e, knowledge.RetrievalQuery{PerNotePassages: connection.Limits.PerNotePassages, Text: query, Limit: 3, TokenBudget: min(1500, max(256, connection.Limits.EvidenceTokens/3)), LocalOnly: cfg.LocalOnly && !cfg.EmbeddingsLocal}, connection, false)
		})
		if err != nil {
			return false, err
		}
		for _, p := range related.Passages {
			if p.Path != selected.Path {
				citations = append(citations, chat.Citation{ID: fmt.Sprintf("compare-%d", len(citations)), Path: p.Path, URL: p.Citation, Title: p.Heading, Text: p.Text, Revision: p.Revision, Hash: p.NoteHash, PassageHash: p.Hash, Kind: "committed"})
			}
		}
		if err := pinChatCitationReferences(ctx, store, citations); err != nil {
			return false, err
		}
		summaries := []struct {
			Path     string `json:"path"`
			Revision string `json:"revision"`
			Summary  string `json:"summary"`
		}{}
		for _, n := range reviewNavigationNotes(&coverage) {
			inheritChatProvenance(&provenance, chat.Turn{Provenance: n.Provenance, Citations: n.Sources})
			summaries = append(summaries, struct {
				Path     string `json:"path"`
				Revision string `json:"revision"`
				Summary  string `json:"summary"`
			}{n.Path, n.Revision, n.Summary})
		}
		payload, _ := json.Marshal(map[string]any{"scope": "bounded original-source review batch, not full notebook coverage", "note": selected.Path, "part": selected.Cursor + 1, "parts": selected.Total, "progress": reviewProgress(&coverage), "originalSources": citations, "analysisForNavigationOnly": summaries, "explicitAttachments": attachments, "attachmentSources": attachmentSources, "notebookGuidance": guidance})
		content := purpose + "\nExamine this original passage for the review purpose. Compare only original passages supplied or fetched with tools; summaries are navigation hints and cannot establish claims. Explicit live/private attachments are separately labelled evidence and never part of committed notebook coverage. Cite exact sources. State what remains unexamined; the server controls completion. Do not label the full notebook reviewed from this batch.\n<untrusted_review_batch>" + string(payload) + "</untrusted_review_batch>"
		messages = []chat.Message{{Role: "user", Content: content}}
		if estimateTokens(messages)+estimateTokens(s.runTools(*run, connection))+estimateTokens(chatSystem)+connection.Limits.OutputTokens > connection.Limits.ContextTokens {
			return false, errors.New("review batch exceeds configured context; increase operator context or reduce output budget")
		}
		coverage.BatchPath = selected.Path
		coverage.BatchCursor = selected.Cursor
		// Original attachment identities are durable evidence, separately labelled
		// from the comparison passages in the model payload above.
		citations = append(citations, attachmentSources...)
	}
	if coverage.Complete && reviewFinalizationEnabled(run, connection) {
		coverage.Finalizing = true
		if compatible && old.Finalizing && old.Revision == coverage.Revision && old.Visibility == coverage.Visibility && reviewExternalEvidenceValid(run.Provenance, run.Citations, reviewExternalAllowance(run)) {
			messages, citations, provenance = run.Messages, run.Citations, run.Provenance
		} else {
			messages = reviewFinalizationMessages(*run, purpose, guidance, &coverage)
			citations = attachmentSources
			provenance.SnapshotBound = true
		}
	}
	if err = s.checkRun(a); err != nil {
		return false, err
	}
	err = s.chatRunUpdate(a, id, runID, func(c *chat.Conversation, r *chat.Run) error {
		r.Review = &coverage
		r.Messages = messages
		r.Citations = citations
		r.Provenance = provenance
		c.AddEvent(runID, "review", reviewProgress(&coverage))
		c.AddEvent(runID, "citations", citations)
		c.Memory.Revisions = map[string]string{}
		for _, n := range coverage.Notes {
			if n.State == "examined" {
				c.Memory.Revisions[n.Path] = n.Hash + ":" + n.References
			}
		}
		return nil
	})
	return coverage.Complete, err
}
func finishChatReviewBatch(r *chat.Run, summary string) {
	if r.Review == nil || r.Review.BatchPath == "" {
		return
	}
	// Use the same bounded navigation selection that populated this batch.
	// Preserve its original source revisions without exposing dependency records
	// as navigation text or replacing the batch's displayed citations.
	var navigationSources []chat.Citation
	for _, n := range reviewNavigationNotes(r.Review) {
		navigationSources = append(navigationSources, n.Sources...)
	}
	for i := range r.Review.Notes {
		n := &r.Review.Notes[i]
		if n.Path == r.Review.BatchPath && n.Cursor == r.Review.BatchCursor {
			if n.Cursor == 0 {
				n.Provenance = cloneChatProvenance(r.Provenance)
			} else {
				n.Provenance = cloneChatProvenance(n.Provenance)
			}
			inheritChatProvenance(&n.Provenance, chat.Turn{Provenance: r.Provenance, Citations: r.Citations})
			var complete bool
			n.Sources, complete = mergeReviewSources(n.Sources, r.Citations, navigationSources)
			n.Provenance.Complete = n.Provenance.Complete && complete && reviewExternalEvidenceValid(n.Provenance, r.Citations, reviewExternalAllowance(r))
			if !n.Provenance.Complete {
				n.Cursor, n.Summary, n.State = 0, "", "unverified"
				break
			}
			n.Cursor++
			n.Summary = textLimit(strings.TrimSpace(n.Summary+"\n"+summary), 2000)
			if n.Cursor == n.Total {
				n.State = "examined"
			}
			break
		}
	}
	r.Review.BatchPath = ""
	r.Review.Complete = false
}

// Navigation is selected identically when preparing and finalizing a batch so
// its inherited source revisions remain associated with the derived analysis.
func reviewNavigationNotes(coverage *chat.Coverage) []chat.ReviewNote {
	notes := []chat.ReviewNote{}
	size := 0
	for _, n := range coverage.Notes {
		if n.Summary != "" && size < 1800 {
			n.Summary = textLimit(n.Summary, 450)
			size += len(n.Summary)
			notes = append(notes, n)
		}
	}
	return notes
}

func validReviewSources(sources []chat.Citation) bool {
	if len(sources) == 0 || len(sources) > 256 {
		return false
	}
	raw, err := json.Marshal(sources)
	if err != nil || len(raw) > 64<<10 {
		return false
	}
	for _, source := range sources {
		if source.Kind != "committed" || source.Path == "" || source.Revision == "" || source.Hash == "" || source.References == "" || source.Text != "" {
			return false
		}
	}
	return true
}

func mergeReviewSources(groups ...[]chat.Citation) ([]chat.Citation, bool) {
	sources := []chat.Citation{}
	seen := map[string]bool{}
	bytes := 2
	complete := true
	for _, group := range groups {
		for _, citation := range group {
			if citation.Kind == "external" && validExternalCapture(externalCapture(citation)) && citation.Path == "" && citation.Revision == "" {
				continue
			}
			source := chat.Citation{Scope: citation.Scope, Path: citation.Path, Revision: citation.Revision, Hash: citation.Hash, References: citation.References, Kind: citation.Kind}
			if source.Kind != "committed" || source.Path == "" || source.Revision == "" || source.Hash == "" || source.References == "" {
				complete = false
				continue
			}
			raw, _ := json.Marshal(source)
			key := string(raw)
			if seen[key] {
				continue
			}
			if len(sources) >= 256 || bytes+len(raw)+1 > 64<<10 {
				complete = false
				continue
			}
			seen[key] = true
			bytes += len(raw) + 1
			sources = append(sources, source)
		}
	}
	return sources, complete && len(sources) > 0
}
