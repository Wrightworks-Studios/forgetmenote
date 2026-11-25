package workspace

import (
	"encoding/json"
	"errors"
	"path"
	"strings"
)

// OrganizationAction is an organizer-owned, closed set of operations. The
// gateway supplies authenticated policy; models can never supply write payloads.
type OrganizationAction struct {
	ID       string           `json:"id"`
	Kind     string           `json:"kind"`
	Path     string           `json:"path"`
	Target   string           `json:"target,omitempty"`
	Tags     []string         `json:"tags,omitempty"`
	Sources  []MutationSource `json:"sources"`
	Expected []MutationSource `json:"expected,omitempty"`
	Allowed  []string         `json:"allowed"`
	Folders  []string         `json:"folders,omitempty"`
}
type OrganizationFile struct {
	Path   string  `json:"path"`
	Before *string `json:"before"`
	After  *string `json:"after"`
}
type OrganizationResult struct {
	Revision string             `json:"revision"`
	Replayed bool               `json:"replayed"`
	Files    []OrganizationFile `json:"files"`
	Trash    *TrashEntry        `json:"trash,omitempty"`
	// TrashLinks names the notes in the Trash that link to the note a move
	// relocates, which Files leaves out: the move updates their links, and
	// its undo reverts them, on a best-effort basis.
	TrashLinks []OrganizationTrashLink `json:"trashLinks,omitempty"`
}

// OrganizationTrashLink is a note in the Trash that links to the note an
// organization move relocates. Notes in the Trash are outside every
// selected scope; keeping their links working is the system's, as for an
// ordinary move, so the move updates them without a selection and leaves
// one it may not change as it is, naming it, instead of refusing.
type OrganizationTrashLink struct {
	Path         string `json:"path"`
	OriginalPath string `json:"originalPath,omitempty"`
	// Updated reports whether the revision changed the note: the move
	// rewrote its links to reach the new location, or the undo reverted
	// them. Reason says why it did not.
	Updated bool   `json:"updated"`
	Reason  string `json:"reason,omitempty"`
}

func organizationOperation(a OrganizationAction, actor string) operationIdentity {
	raw, _ := json.Marshal(a)
	return operationIdentity{ID: ContentHash([]byte(actor + "\x00organization:" + a.ID)), Request: ContentHash(raw)}
}
func containsPath(paths []string, p string) bool {
	for _, v := range paths {
		if v == p {
			return true
		}
	}
	return false
}

var ErrOrganizationOutcomeUnknown = errors.New("organization commit outcome needs receipt recovery")

func unsupportedReference(content []byte, target string) bool {
	body := string(MarkdownBody(content))
	// Unsupported link syntax cannot be repaired by the Markdown link owner.
	return (strings.Contains(body, "[[") || strings.Contains(body, "<a ") || strings.Contains(body, "<img ")) && (strings.Contains(body, path.Base(target)) || strings.Contains(body, strings.TrimSuffix(path.Base(target), ".md")))
}
