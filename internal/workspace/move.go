package workspace

import (
	"strings"
)

// MoveResult is a move's or a duplicate's outcome. UpdatedLinks counts every
// note whose links it updated, notes in the Trash included. Of the notes in
// the Trash that link into what moved, TrashLinksUpdated counts those it
// updated and TrashLinksSkipped those a scoped chat action's move left as
// they were because an ordinary move would refuse to change them: counts
// only, as the Trash is outside the action's corpus.
type MoveResult struct {
	From              string `json:"from"`
	To                string `json:"to"`
	Revision          string `json:"revision"`
	UpdatedLinks      int    `json:"updatedLinks"`
	TrashLinksUpdated int    `json:"trashLinksUpdated,omitempty"`
	TrashLinksSkipped int    `json:"trashLinksSkipped,omitempty"`
}

func movedPath(path, from, to string) string {
	if path == from {
		return to
	}
	if strings.HasPrefix(path, from+"/") {
		return to + strings.TrimPrefix(path, from)
	}
	return path
}
