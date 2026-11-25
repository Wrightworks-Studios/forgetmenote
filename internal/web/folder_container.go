package web

import (
	"context"
	"fmt"
	"net/http"
	"path"
	"strings"

	"forgetmenote/internal/webcore"
	"forgetmenote/internal/workspace"
)

type folderContainerResult struct {
	Revision string `json:"revision"`
	Changed  int    `json:"changed"`
}

// setFolderContainer moves every note under a folder to one collection
// (Notes, Drafts or Archives). The edits are bound to the caller's snapshot
// revision and each note's content hash, so a note changed since the caller
// looked is a conflict rather than a silent overwrite. ApplyGroup keeps its
// lock, visibility and live-edit checks; groups hold at most 50 notes, so a
// larger folder is moved in several commits.
func (s *Server) setFolderContainer(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Folder    string `json:"folder"`
		Container string `json:"container"`
		Revision  string `json:"revision"`
	}
	if !decode(w, r, &in) {
		return
	}
	result, err := moveFolderContainer(r.Context(), projectStore(r.Context()), in.Folder, in.Container, in.Revision)
	respond(w, result, err)
}

func moveFolderContainer(ctx context.Context, store *workspace.Store, folder, container, revision string) (folderContainerResult, error) {
	if container != "" && container != "draft" && container != "archive" {
		return folderContainerResult{}, webcore.Invalid("choose Notes, Drafts, or Archives")
	}
	if folder == "" || folder != path.Clean(folder) || strings.HasPrefix(folder, "/") || strings.HasPrefix(folder, "..") || folder == "Trash" || strings.HasPrefix(folder, "Trash/") {
		return folderContainerResult{}, webcore.Invalid("choose a notebook folder outside Trash")
	}
	if revision == "" {
		return folderContainerResult{}, webcore.Invalid("folder moves need the revision you are viewing")
	}
	snapshot, err := store.Snapshot(ctx, revision)
	if err != nil {
		return folderContainerResult{}, err
	}
	edits := []workspace.NoteEdit{}
	for _, file := range snapshot.Files {
		if !strings.HasPrefix(file.Path, folder+"/") || !strings.HasSuffix(file.Path, ".md") {
			continue
		}
		if file.Metadata.ContainerError != "" {
			return folderContainerResult{}, webcore.Invalid(fmt.Sprintf("%s has an invalid collection field; fix it before moving the folder", file.Path))
		}
		if file.Metadata.Container == container {
			continue
		}
		content, err := workspace.WithContainer([]byte(file.Content), container)
		if err != nil {
			return folderContainerResult{}, fmt.Errorf("%s: %w", file.Path, err)
		}
		edits = append(edits, workspace.NoteEdit{Path: file.Path, Content: string(content), Revision: snapshot.Revision, Hash: file.Hash})
	}
	// A lock refuses the grouped edit; name it rather than an inaccessible path.
	locks, err := store.Locks(ctx, "")
	if err != nil {
		return folderContainerResult{}, err
	}
	for _, edit := range edits {
		if rule := locks.Covering(edit.Path); rule != "" {
			return folderContainerResult{}, webcore.Error{Status: 403, Code: "locked", Message: fmt.Sprintf("“%s” is locked. Unlock it before moving the notes in this folder.", rule)}
		}
	}
	result := folderContainerResult{Revision: snapshot.Revision}
	destination := map[string]string{"": "Notes", "draft": "Drafts", "archive": "Archives"}[container]
	name := folder
	if len(name) > 200 {
		name = name[:200] + "…"
	}
	for start := 0; start < len(edits); start += 50 {
		chunk := edits[start:min(len(edits), start+50)]
		// A deterministic ID makes a retried request replay instead of repeating.
		id := workspace.ContentHash([]byte(fmt.Sprintf("folder-container\x00%s\x00%s\x00%s\x00%d", folder, container, snapshot.Revision, start)))
		applied, err := store.ApplyGroup(ctx, workspace.EditGroup{ID: id, Summary: "Move folder " + name + " to " + destination, Edits: chunk})
		if err != nil {
			if result.Changed > 0 {
				return result, fmt.Errorf("moved %d of %d notes before stopping: %w", result.Changed, len(edits), err)
			}
			return result, err
		}
		result.Revision = applied.Revision
		result.Changed += len(chunk)
	}
	return result, nil
}
