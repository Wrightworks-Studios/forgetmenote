package web

import (
	"context"
	"errors"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/decision"
	"forgetmenote/internal/knowledge"
	"forgetmenote/internal/workspace"
	"net/http"
	"os"
	"sort"
)

type jevNotification struct {
	Details     any                          `json:"details,omitempty"`
	ID          string                       `json:"id"`
	Project     string                       `json:"project"`
	Job         string                       `json:"job"`
	Kind        string                       `json:"kind"`
	Title       string                       `json:"title"`
	Summary     string                       `json:"summary"`
	CreatedAt   string                       `json:"createdAt"`
	Read        bool                         `json:"read"`
	FindingID   string                       `json:"findingId,omitempty"`
	FindingPath string                       `json:"findingPath,omitempty"`
	ProposalID  string                       `json:"proposalId,omitempty"`
	Evidence    []knowledge.JudgmentEvidence `json:"evidence"`
}
type jevReadState struct {
	Version   int             `json:"version"`
	AccountID string          `json:"accountId"`
	Read      map[string]bool `json:"read"`
}

func readStateName(account string) string { return "notifications/" + digest(account) + ".json" }
func newReadState(account string) jevReadState {
	return jevReadState{Version: 1, AccountID: account, Read: map[string]bool{}}
}
func validReadState(account string, state jevReadState) bool {
	return state.Version == 1 && state.AccountID == account && state.Read != nil && len(state.Read) <= 10000
}

func (s *Server) notificationReadState(account string) (jevReadState, error) {
	var state jevReadState
	err := s.jevState.read(readStateName(account), &state)
	if errors.Is(err, os.ErrNotExist) {
		return newReadState(account), nil
	}
	if err != nil || !validReadState(account, state) {
		return state, decision.ErrStorage
	}
	return state, nil
}

func (s *Server) notifications(ctx context.Context, user string) ([]jevNotification, error) {
	account, err := s.catalog.Account(user)
	if err != nil || !s.catalog.Enabled(user) {
		return nil, workspace.ErrForbidden
	}
	readState, err := s.notificationReadState(account.ID)
	jobs, listErr := s.jevState.jobs(nil)
	if err != nil || listErr != nil {
		return nil, errors.Join(err, listErr)
	}
	// Authorize before reading notebook discussions or considering any excerpt.
	stores := map[string]*workspace.Store{}
	findings := map[string]map[string]workspace.ReviewFinding{}
	// Many findings cite different passages of the same notes. Read each note
	// once for this response, then recheck notebook authority before returning it.
	type evidenceRead struct {
		hash, container string
		valid           bool
	}
	reads := map[string]map[string]evidenceRead{}
	for _, project := range s.catalog.List(user) {
		store, e := s.catalog.Access(user, project.ID, "read")
		if e != nil {
			continue
		}
		stores[project.ID] = store
		reads[project.ID] = map[string]evidenceRead{}
	}
	items := map[string]jevNotification{}
	for _, job := range jobs {
		store := stores[job.Project]
		if store == nil || job.Cursor == 0 {
			continue
		}
		if job.Kind == "audit" {
			if err := validateJevSources(ctx, store, job); err != nil {
				continue
			}
		}
		stored, e := s.readJevResults(job)
		if e != nil {
			// Retention may have removed the job since it was listed.
			if _, gone := s.jevState.readJob(job.ID); errors.Is(gone, chat.ErrNotFound) {
				continue
			}
			return nil, e
		}
		for offset := 0; ; {
			page, total, e := jevResultPageOf(job, offset, 100, func(index int) (jevJobResult, error) { return stored[index], nil })
			if e != nil {
				return nil, e
			}
			for _, result := range page {
				relevant := result.FindingID != "" || result.Kind == "audit" && result.Complete && result.Status != "preserved" || result.Kind == "tags" && result.Status == "suggested" || result.Kind == "organization" && result.Status != "unrelated" && result.Status != "uncertain"
				if !relevant || len(result.Evidence) == 0 {
					continue
				}
				visible := true
				for _, source := range result.Evidence {
					if source.Path == "AGENTS.md" || workspace.IsTrash(source.Path) {
						visible = false
						break
					}
					read, ok := reads[job.Project][source.Path]
					if !ok {
						doc, e := store.KnowledgeRead(ctx, source.Path, "")
						if e == nil {
							metadata, e := workspace.ParseMetadata(doc.Content)
							if e == nil && metadata.ContainerError == "" {
								read = evidenceRead{hash: workspace.ContentHash(doc.Content), container: metadata.Container, valid: true}
							}
						}
						reads[job.Project][source.Path] = read
					}
					if !read.valid || read.container == "draft" || read.container != "" && (job.Kind != "audit" || read.container != "archive") {
						visible = false
						break
					}
					expected := source.Hash
					for _, note := range job.Notes {
						if note.Path == source.Path && note.Revision == source.Revision {
							expected = note.CurrentHash
							break
						}
					}
					if read.hash != expected {
						visible = false
						break
					}
				}
				if !visible {
					continue
				}
				id := result.ID
				if result.FindingID != "" {
					if findings[job.Project] == nil {
						rows, e := store.Findings(ctx)
						if e != nil {
							return nil, e
						}
						findings[job.Project] = map[string]workspace.ReviewFinding{}
						for _, finding := range rows {
							findings[job.Project][finding.Comment.ID] = finding
						}
					}
					finding, ok := findings[job.Project][result.FindingID]
					if !ok || finding.Comment.Resolved || finding.Comment.Finding.Stale {
						continue
					}
					id = digest(job.Project + "\x00" + result.FindingID)
				}
				notification := jevNotification{ID: id, Project: job.Project, Job: job.ID, Kind: result.Kind, Title: result.Title, Summary: result.Summary, CreatedAt: job.CreatedAt, Read: readState.Read[id], FindingID: result.FindingID, FindingPath: result.FindingPath, ProposalID: result.ProposalID, Evidence: result.Evidence, Details: result.Details}
				if previous, ok := items[id]; !ok || notification.CreatedAt > previous.CreatedAt {
					items[id] = notification
				}
			}
			offset += len(page)
			if offset >= total {
				break
			}
		}
	}
	if s.quicknotes != nil {
		settings, e := s.quicknotes.Settings(user)
		if e != nil {
			return nil, e
		}
		if settings.NotifyDestination {
			captures, e := s.quicknotes.List(ctx, user)
			if e != nil {
				return nil, e
			}
			for _, capture := range captures {
				if capture.Status != "filed" {
					continue
				}
				store, e := s.catalog.Access(user, capture.Project, "read")
				if e != nil {
					continue
				}
				doc, e := store.Read(ctx, capture.Path, "")
				if e != nil || doc.Version != capture.Version || workspace.ContentHash(doc.Content) != capture.Hash {
					continue
				}
				id := digest("quicknotes\x00" + account.ID + "\x00" + capture.ID + "\x00" + capture.Project + "\x00" + capture.Path)
				items[id] = jevNotification{ID: id, Project: capture.Project, Kind: "quicknotes", Title: "Quicknote filed", Summary: "Saved in " + capture.Project + " / " + capture.Path, CreatedAt: capture.CreatedAt, Read: readState.Read[id], Evidence: []knowledge.JudgmentEvidence{{Path: capture.Path, Revision: doc.Revision, Hash: capture.Hash, Text: capture.Excerpt, URL: evidenceURL(capture.Path, doc.Revision)}}}
			}
		}
	}
	out := []jevNotification{}
	for _, item := range items {
		if _, err := s.catalog.Access(user, item.Project, "read"); err == nil && s.catalog.Enabled(user) {
			out = append(out, item)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt != out[j].CreatedAt {
			return out[i].CreatedAt > out[j].CreatedAt
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (s *Server) jevNotifications(w http.ResponseWriter, r *http.Request) {
	user, err := s.interactiveIdentity(r)
	if err != nil {
		chatFail(w, err)
		return
	}
	items, err := s.notifications(r.Context(), user)
	if err != nil {
		chatFail(w, err)
		return
	}
	count := 0
	for _, item := range items {
		if !item.Read {
			count++
		}
	}
	offset, limit, err := jevPage(r)
	if err != nil {
		chatFail(w, err)
		return
	}
	start, end := jevPageBounds(len(items), offset, limit)
	var next any
	if end < len(items) {
		next = end
	}
	jsonResponse(w, map[string]any{"unreadCount": count, "items": items[start:end], "total": len(items), "offset": start, "next": next})
}

func (s *Server) jevNotificationsRead(w http.ResponseWriter, r *http.Request) {
	user, err := s.interactiveIdentity(r)
	if err != nil {
		chatFail(w, err)
		return
	}
	var input struct {
		IDs []string `json:"ids"`
	}
	if !decode(w, r, &input) {
		return
	}
	if len(input.IDs) > 2000 {
		chatFail(w, decision.ErrLimit)
		return
	}
	items, err := s.notifications(r.Context(), user)
	if err != nil {
		chatFail(w, err)
		return
	}
	allowed := map[string]bool{}
	for _, item := range items {
		allowed[item.ID] = true
	}
	for _, id := range input.IDs {
		if !allowed[id] {
			chatFail(w, workspace.ErrForbidden)
			return
		}
	}
	account, err := s.catalog.Account(user)
	if err != nil {
		chatFail(w, err)
		return
	}
	s.jevState.mu.Lock()
	state := newReadState(account.ID)
	err = s.jevState.update(readStateName(account.ID), &state, func(bool) error {
		if !validReadState(account.ID, state) {
			return decision.ErrStorage
		}
		for _, id := range input.IDs {
			state.Read[id] = true
		}
		if len(state.Read) > 10000 {
			return decision.ErrLimit
		}
		return nil
	})
	s.jevState.mu.Unlock()
	if err != nil {
		chatFail(w, err)
		return
	}
	jsonResponse(w, map[string]bool{"ok": true})
}
