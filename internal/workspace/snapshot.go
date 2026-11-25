package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"forgetmenote/internal/capacity"
)

type SnapshotFile struct {
	Priority int      `json:"priority,omitempty"`
	Path     string   `json:"path"`
	Hash     string   `json:"hash"`
	Content  string   `json:"content"`
	Metadata Metadata `json:"metadata"`
}
type Snapshot struct {
	ScopeID    string         `json:"scopeId,omitempty"`
	Revision   string         `json:"revision"`
	Visibility string         `json:"visibility"`
	Files      []SnapshotFile `json:"files"`
}
type FileChange struct {
	Kind string `json:"kind"`
	Path string `json:"path"`
	From string `json:"from,omitempty"`
}
type ChangeSet struct {
	CommitsTruncated bool         `json:"commitsTruncated"`
	From             string       `json:"from"`
	Revision         string       `json:"revision"`
	FreshReview      bool         `json:"freshReview"`
	Reason           string       `json:"reason,omitempty"`
	Files            []FileChange `json:"files"`
	Commits          []Commit     `json:"commits"`
}

// Snapshots are bounded committed inputs, never working-tree reads.
func MaxSnapshotBytes() int { return capacity.IndexBytes.Limit() }

func ContentHash(content []byte) string { h := sha256.Sum256(content); return hex.EncodeToString(h[:]) }
func (s *Store) visibleNote(path string) bool {
	return s.visibleNoteWithExclusions(path, s.browseExcluded)
}
func (s *Store) visibleNoteWithExclusions(path string, exclusions []string) bool {
	if !strings.HasSuffix(path, ".md") || matchesPrefix(path, exclusions) {
		return false
	}
	_, err := s.resolve(path, false)
	return err == nil
}

type snapshotSelection struct {
	revision, visibility string
	exclusions           []string
	visibleTrash         map[string]bool
	generation           uint64
}
