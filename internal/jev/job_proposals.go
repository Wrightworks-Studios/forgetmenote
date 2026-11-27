package jev

import (
	"context"
	"fmt"
	"forgetmenote/internal/decision"
	"forgetmenote/internal/knowledge"
	"forgetmenote/internal/workspace"
	"net/url"
	"path/filepath"
	"strings"
)

func (s *Service) jevOrganizationProposal(ctx context.Context, job *jevJob, work *jevJobWork, evidence []knowledge.JudgmentEvidence, tags []string, judge *decision.Service) (string, error) {
	if len(evidence) == 0 {
		return "", nil
	}
	store, err := s.catalog.Access(job.Owner, job.Project, "comment")
	if err != nil {
		return "", err
	}
	if work.Proposal == nil {
		source := evidence[0]
		doc, err := store.KnowledgeRead(ctx, source.Path, "")
		if err != nil {
			return "", err
		}
		if workspace.ContentHash(doc.Content) != source.Hash {
			return "", workspace.ErrConflict
		}
		var content []byte
		summary := ""
		if work.Kind == "tags" {
			if len(tags) == 0 {
				return "", nil
			}
			content, err = workspace.WithAdditionalTags(doc.Content, tags)
			if err != nil {
				return "", err
			}
			summary = "Add suggested tags to " + source.Path
		} else {
			if len(evidence) < 2 || source.Path == evidence[1].Path || work.Verdict != "related" && work.Verdict != "complementary" && work.Verdict != "versioned" {
				return "", nil
			}
			target := evidence[1].Path
			relative, err := filepath.Rel(filepath.Dir(source.Path), target)
			if err != nil {
				return "", err
			}
			destination := (&url.URL{Path: filepath.ToSlash(relative)}).String()
			if strings.Contains(string(doc.Content), destination) {
				return "", nil
			}
			content = []byte(strings.TrimRight(string(doc.Content), "\n") + "\n\nRelated: [Related note](<" + destination + ">)\n")
			summary = "Link related notes"
		}
		if string(content) == string(doc.Content) {
			return "", nil
		}
		if len(summary) > 300 {
			summary = "Add suggested note tags"
		}
		work.Proposal = &workspace.EditGroup{ID: "jev-" + job.ID + "-" + fmt.Sprint(job.Cursor), Summary: summary, Edits: []workspace.NoteEdit{{Path: source.Path, Revision: doc.Revision, Hash: source.Hash, Content: string(content)}}}
		if err = s.saveJevJob(job); err != nil {
			return "", err
		}
	}
	if err = s.checkJevJob(ctx, *job, judge); err != nil {
		return "", err
	}
	scope := workspace.MutationScope{Version: 1, Content: jevActiveScope()}
	for _, source := range evidence {
		scope.Sources = append(scope.Sources, workspace.MutationSource{Path: source.Path, Revision: source.Revision, Hash: source.Hash})
	}
	proposal, err := withEngine(s, ctx, store, func(e *knowledge.Engine) (knowledge.Proposal, error) {
		return e.Propose(workspace.WithMutationScope(ctx, scope), job.Owner, *work.Proposal)
	})
	return proposal.ID, err
}
