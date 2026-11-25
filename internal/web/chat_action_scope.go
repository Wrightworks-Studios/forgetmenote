package web

import (
	"context"
	"encoding/json"
	"errors"
	"forgetmenote/internal/actions"
	"os"
	"strings"

	"forgetmenote/internal/chat"
	"forgetmenote/internal/knowledge"
	"forgetmenote/internal/workspace"
)

var errChatPersonalApproval = errors.New("a change carrying personal assistant context needs your approval in this notebook")

// chatReadOnly refuses notebook changes from a run whose corpus reaches another
// notebook. Such a change is not offered for approval.
func chatReadOnly(project string, run chat.Run) error {
	if corpus := run.Options.Corpus; corpus != nil {
		for _, p := range append(append([]string(nil), corpus.SourceProjects...), corpus.NoteProjects...) {
			if p != project {
				return errors.New("cross-notebook source context is read-only; save any explanation with an explicitly authorized destination")
			}
		}
	}
	return nil
}

// chatPersonalApproval reports whether a notebook change from a conversation
// with personal assistant context must be approved by the person: other people
// may read any notebook but their own assistant notebook.
func (s *Server) chatPersonalApproval(user, project string, personal bool) bool {
	if !personal {
		return false
	}
	settings, err := s.catalog.Assistant(user)
	return err != nil || settings.Project == "" || settings.Project != project
}

func (s *Server) guardChatAction(ctx context.Context, project, name string, raw json.RawMessage) (context.Context, error) {
	name = actions.LegacyName(name)
	scope := chat.RunScopeFrom(ctx)
	if scope == nil {
		return ctx, nil
	}
	if !s.actions[name].Policy.ChatAvailable() {
		return ctx, chat.ErrOutsideCorpus
	}

	if p := s.actions[name].Policy; p.Scope == selectedDocument || p.Scope == selectedNotebooks {
		// Shared handlers enforce the exact project/origin and current binding.
		return ctx, nil
	}
	if s.actions[name].Policy.Scope == selectedRepository {
		var in struct {
			Project, Source, Revision, Path string
		}
		if json.Unmarshal(raw, &in) != nil {
			return ctx, chat.ErrOutsideCorpus
		}
		if err := s.checkSourceChatScope(ctx, in.Project); err != nil {
			return ctx, err
		}
		if s.repositories == nil {
			return ctx, chat.ErrOutsideCorpus
		}
		state, err := s.repositories.Status(in.Project)
		if err != nil {
			return ctx, err
		}
		if !state.Connected || !state.IndexConfig.ExternalProcessing || state.ID != scope.Run.Options.Corpus.SourceBindings[in.Project] {
			return ctx, chat.ErrOutsideCorpus
		}
		if in.Source != "" && in.Source != state.ID || in.Revision != "" && in.Revision != state.Downloaded {
			return ctx, chat.ErrOutsideCorpus
		}
		if s.actions[name].Policy.SourceContent {
			return ctx, s.repositories.AuthorizeProcessing(ctx, in.Project, state.ID, state.Downloaded, []string{in.Path})
		}
		return ctx, nil
	}
	if !s.actions[name].Policy.Read {
		if err := chatReadOnly(scope.Project, scope.Run); err != nil {
			return ctx, err
		}
		if !scope.Approved() && s.chatPersonalApproval(subject(ctx), scope.Project, scope.Personal) {
			return ctx, errChatPersonalApproval
		}
	}
	store, err := s.catalog.Access(subject(ctx), project, "read")
	if err != nil {
		return ctx, err
	}
	var in struct {
		Path, Revision, SourceRevision, From, To, Kind, ID, Decision string
		Paths                                                        []string
		Group                                                        workspace.EditGroup
		Finding                                                      workspace.FindingInput
	}
	if err = json.Unmarshal(raw, &in); err != nil {
		return ctx, err
	}
	mutation := workspace.MutationScope{Version: 1, Content: scope.Content}
	switch name {
	case "set_note_container", "set_project_instructions", "trash_notes", "restore_trash", "empty_trash", "move_notes", "duplicate_notes", "archive_source", "upload_attachment", "set_note_lock":
		mutation.Control = name
	}
	if name == "submit_change_group" {
		for _, edit := range in.Group.Edits {
			if edit.Delete {
				mutation.Control = "trash_notes"
			}
		}
	}
	ctx = workspace.WithMutationScope(ctx, mutation)
	current := func(path string, allowNew bool) error {
		if path == "" || path == "AGENTS.md" || workspace.IsTrash(path) || !strings.HasSuffix(path, ".md") {
			return chat.ErrOutsideCorpus
		}
		doc, e := store.KnowledgeRead(ctx, path, "")
		if errors.Is(e, os.ErrNotExist) && allowNew {
			return nil
		}
		if e != nil {
			return e
		}
		metadata, e := workspace.ParseMetadata(doc.Content)
		if e != nil || scope.Content.Tier(metadata) < 0 {
			return chat.ErrOutsideCorpus
		}
		return nil
	}
	switch name {
	case "list_comments", "add_comment", "resolve_comment", "delete_comment", "read_live_note", "patch_live_note", "checkpoint_live_note", "set_note_container", "note_locks":
		err = current(in.Path, false)
	case "note_history":
		if !scope.Run.Options.Corpus.History {
			err = chat.ErrOutsideCorpus
		} else {
			err = current(in.Path, false)
		}
	case "save_note", "set_note_properties":
		err = current(in.Path, true)
	case "submit_change_group":
		err = store.ValidateMutationGroup(ctx, in.Group)
		if err == nil {
			for _, edit := range in.Group.Edits {
				if edit.Revision != "" {
					_, err = readChatNote(ctx, store, edit.Path, edit.Revision)
					if err != nil {
						break
					}
				}
			}
		}
	case "file_review_finding":
		err = current(in.Finding.Path, false)
		if err == nil {
			for _, ref := range in.Finding.Evidence {
				if ref.Path != "" {
					proofScope := *scope
					proofScope.Origin.Attachments = nil
					doc, e := readChatNote(chat.WithScope(ctx, &proofScope), store, ref.Path, ref.Revision)
					err = e
					if err != nil {
						break
					}
					historical := scope.Run.Options.Corpus.History && ref.Revision != ""
					mutation.Sources = append(mutation.Sources, workspace.MutationSource{Path: ref.Path, Revision: doc.Revision, Hash: workspace.ContentHash(doc.Content), Historical: historical})
				}
			}
			ctx = workspace.WithMutationScope(ctx, mutation)
		}
	case "review_decision":
		if in.Kind == "finding" {
			err = current(in.Path, false)
		} else if in.Kind == "proposal" {
			var proposals []knowledge.Proposal
			proposals, err = withEngine(s, ctx, store, func(e *knowledge.Engine) ([]knowledge.Proposal, error) { return e.Proposals(ctx) })
			if err == nil {
				err = chat.ErrOutsideCorpus
				for _, proposal := range proposals {
					if proposal.ID == in.ID {
						err = validateChatProposal(ctx, store, proposal, scope.Content)
						break
					}
				}
			}
		} else {
			err = chat.ErrOutsideCorpus
		}
	case "archive_source":
		if in.Path != "" {
			err = current(in.Path, false)
		}
	case "restore_note":
		err = current(in.Path, false)
		if err == nil {
			_, err = readChatNote(ctx, store, in.Path, in.SourceRevision)
		}
	case "trash_notes":
		for _, path := range in.Paths {
			if err = current(path, false); err != nil {
				break
			}
		}
	case "upload_attachment":
		// Existing media can belong to several notes. Every owner must be eligible;
		// an unattached new asset grants no source-reading exception.
		var snapshot workspace.Snapshot
		snapshot, err = store.Snapshot(ctx, "")
		if err == nil {
			for _, file := range snapshot.Files {
				if strings.Contains(file.Content, in.Path) && scope.Content.Tier(file.Metadata) < 0 {
					err = chat.ErrOutsideCorpus
					break
				}
			}
		}
	case "complete_workflow":
		if scope.Run.Options.Review && (scope.Run.Review == nil || scope.Run.Review.CorpusID != scope.Run.Options.Corpus.ID()) {
			err = chat.ErrOutsideCorpus
		}
	}
	return ctx, err
}

func validateChatProposal(ctx context.Context, store *workspace.Store, proposal knowledge.Proposal, scope workspace.ContentScope) error {
	if err := store.ValidateMutationGroup(ctx, proposal.Group); err != nil {
		return err
	}
	for _, edit := range proposal.Group.Edits {
		if before := proposal.Before[edit.Path]; before != "" {
			metadata, err := workspace.ParseMetadata([]byte(before))
			if err != nil || scope.Tier(metadata) < 0 {
				return chat.ErrOutsideCorpus
			}
		}
	}
	return nil
}

// This is the model projection of an explicit control, not the private receipt
// or confirmation preview. In particular Empty Trash never supplies source text,
// and a move reports how many notes in the Trash it updated or skipped, never
// which: the Trash is outside every chat corpus.
func chatControlResult(name string, result json.RawMessage) string {
	if p := policy(name); !p.Control && !p.Edit && !p.Proposal {
		return textLimit(string(result), 12000)
	}
	var value map[string]json.RawMessage
	_ = json.Unmarshal(result, &value)
	out := map[string]any{"action": name, "completed": true}
	for _, key := range []string{"error", "revision", "count", "removed", "replayed", "id", "status", "trashLinksUpdated", "trashLinksSkipped"} {
		if raw, ok := value[key]; ok {
			var scalar any
			if json.Unmarshal(raw, &scalar) == nil {
				switch scalar.(type) {
				case string, float64, bool:
					out[key] = scalar
				}
			}
		}
	}
	if _, ok := out["error"]; ok {
		out["completed"] = false
	}
	raw, _ := json.Marshal(out)
	return string(raw)
}
