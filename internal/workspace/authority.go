package workspace

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const MaxAuthorityBytes = 64 << 20

// ErrOwnerUnavailable reports a notebook whose storage could not answer, so
// a request's outcome is unknown; retry it against the same notebook.
var ErrOwnerUnavailable = errors.New("notebook owner unavailable; retry against the same owner")

type AuthorityState struct {
	Version    int    `json:"version"`
	Generation uint64 `json:"generation"`
	Type       string `json:"type"`
	Transfer   string `json:"transfer,omitempty"`
	Phase      string `json:"phase,omitempty"`
	To         string `json:"to,omitempty"`
	Revision   string `json:"revision,omitempty"`
	StageHash  string `json:"stageHash,omitempty"`
	Committed  string `json:"committed,omitempty"`
}
type AuthorityRequest struct {
	ID         string `json:"id"`
	Generation uint64 `json:"generation"`
	From       string `json:"from"`
	To         string `json:"to"`
	Revision   string `json:"revision"`
}
type NotebookSnapshot struct {
	Revision   string            `json:"revision"`
	Source     string            `json:"source,omitempty"`
	Repository string            `json:"repository,omitempty"`
	Files      map[string][]byte `json:"files"`
}
type authorityContextKey struct{}

func WithAuthority(ctx context.Context, generation uint64) context.Context {
	return context.WithValue(ctx, authorityContextKey{}, generation)
}
func authorityGeneration(ctx context.Context) uint64 {
	n, _ := ctx.Value(authorityContextKey{}).(uint64)
	return n
}

// SetAuthorityGeneration installs the authority generation the catalog holds
// for this notebook; every mutation fences on it.
func (s *Store) SetAuthorityGeneration(generation uint64) { s.authorityGeneration.Store(generation) }
func (s *Store) mutationContext(ctx context.Context) context.Context {
	if authorityGeneration(ctx) != 0 {
		return ctx
	}
	return WithAuthority(ctx, max(1, s.authorityGeneration.Load()))
}
func (s *Store) checkAuthority(ctx context.Context) error {
	if s.readOnly.Load() {
		return ErrReadOnly
	}
	// Any gateway may have prepared or completed a handoff of the notebook
	// since this one last looked.
	if err := s.backend.(*dbNotebook).checkStoredAuthority(ctx); err != nil {
		return err
	}
	if s.authority.Phase == "prepared" {
		return ErrForbidden
	}
	// A notebook fences on the generation the catalog installed.
	if gen := authorityGeneration(ctx); gen != 0 && gen != max(1, s.authorityGeneration.Load()) {
		return ErrConflict
	}
	return nil
}

var errInvalidAuthority = errors.New("invalid notebook authority state; restore the full instance")

// validAuthorityState reports whether state is a handoff receipt this
// service wrote: a notebook's authority record in the notes store, or the
// authority.json a previous release kept beside its working tree.
func validAuthorityState(state AuthorityState) bool {
	return state.Version == 1 && state.Generation != 0 &&
		(state.Type == "forgetmenote" || state.Type == "github") &&
		(state.Phase == "prepared" || state.Phase == "complete" || state.Phase == "aborted") &&
		ValidNoteID(state.Transfer) && (state.To == "forgetmenote" || state.To == "github") &&
		(state.Phase == "complete") == (state.Type == state.To) && revisionPattern.MatchString(state.Revision) &&
		(state.StageHash == "" || len(state.StageHash) == 64 && strings.Trim(state.StageHash, "0123456789abcdef") == "") &&
		(state.Phase != "complete" || state.Generation >= 2 && revisionPattern.MatchString(state.Committed))
}
func (s *Store) authorityState() AuthorityState {
	out := s.authority
	if out.Generation == 0 {
		out.Version = 1
		out.Generation = 1
		out.Type = "forgetmenote"
		if s.readOnly.Load() {
			out.Type = "github"
		}
	}
	return out
}

// Validate the complete tree, including files omitted by retrieval, with the
// parsers of a committed lock policy and Trash index. No repository content
// becomes application configuration or consent.
func ValidateNotebookSnapshot(ctx context.Context, snapshot NotebookSnapshot) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !revisionPattern.MatchString(snapshot.Revision) || snapshot.Files == nil || len(snapshot.Files) > 10000 {
		return ErrConflict
	}
	var total int
	ids := map[string]bool{}
	paths := map[string]bool{}
	for p, b := range snapshot.Files {
		total += len(b)
		if len(b) > MaxReadBytes(p) || total > MaxAuthorityBytes {
			return invalidInput("handoff exceeds the configured file/source byte budget or 64 MiB total")
		}
		if p == "" || len(p) > 512 || strings.ContainsAny(p, "\\\x00\r\n") || filepath.IsAbs(p) {
			return ErrForbidden
		}
		for _, part := range strings.Split(p, "/") {
			if part == "" || part == "." || part == ".." {
				return ErrForbidden
			}
		}
		if !gitPath(p) {
			return ErrForbidden
		}
		if paths[strings.ToLower(p)] {
			return invalidInput("handoff contains case-ambiguous paths")
		}
		paths[strings.ToLower(p)] = true
		if bytes.HasPrefix(b, []byte("version https://git-lfs.github.com/spec/v1\n")) {
			return invalidInput(fmt.Sprintf("resolve Git LFS content before importing %s", p))
		}
		if !notebookFile(p) {
			switch {
			case strings.HasPrefix(p, "comments/"):
			case strings.HasPrefix(p, "assets/"):
				return ErrForbidden
			case strings.HasSuffix(p, ".md"):
				return invalidInput(fmt.Sprintf("unsupported hidden notebook file: %s", p))
			}
			return invalidInput(fmt.Sprintf("unsupported notebook file: %s", p))
		}
		switch {
		case strings.HasPrefix(p, "comments/"):
			var comments []Comment
			if json.Unmarshal(b, &comments) != nil {
				return invalidInput("invalid notebook discussion metadata")
			}
		case strings.HasSuffix(p, ".md") && !strings.HasPrefix(p, "assets/"):
			if !utf8.Valid(b) || bytes.IndexByte(b, 0) >= 0 {
				return invalidInput(fmt.Sprintf("note must be UTF-8: %s", p))
			}
			if _, err := ParseMetadata(b); err != nil {
				return fmt.Errorf("invalid metadata in %s: %w", p, err)
			}
			id, err := NoteID(b)
			if err != nil {
				return fmt.Errorf("invalid identity in %s: %w", p, err)
			}
			if id != "" {
				if ids[id] {
					return invalidInput("duplicate notebook identity")
				}
				ids[id] = true
			}
		}
	}
	// A tree holds a file or a folder at a path, never both.
	for p := range snapshot.Files {
		for dir := path.Dir(p); dir != "."; dir = path.Dir(dir) {
			if _, ok := snapshot.Files[dir]; ok {
				return invalidInput(fmt.Sprintf("handoff has a file and a folder at %s", dir))
			}
		}
	}
	return validateSnapshotPolicies(snapshot.Files)
}

// validateSnapshotPolicies parses a snapshot's lock policy and Trash index as
// loadLocks and loadTrash parse committed ones, checking their paths by
// resolve's rules.
func validateSnapshotPolicies(files map[string][]byte) error {
	policies := &Store{}
	if raw, ok := files[locksPath]; ok {
		// Policies are deliberately small and bounded, including imported repositories.
		if len(raw) > 65536 {
			return invalidInput("lock policy exceeds 64 KiB")
		}
		var policy lockPolicy
		if err := json.Unmarshal(raw, &policy); err != nil || policy.Version != 1 || policy.Paths == nil || len(policy.Paths) > 256 {
			return invalidInput("invalid project lock policy")
		}
		seen := map[string]bool{}
		for _, locked := range policy.Paths {
			if _, err := policies.resolve(locked, false); err != nil {
				return err
			}
			if seen[locked] {
				return invalidInput("duplicate project lock path")
			}
			seen[locked] = true
		}
	}
	// The Trash's archives hold tombstones only, numbered from 1 without a
	// gap, as readers find them, and no record twice (checkTrashArchives).
	archives := 0
	var records [][]TrashRecord
	for p, raw := range files {
		if p == trashIndexPath || !trashFile(p) {
			continue
		}
		archives++
		archive, err := parseTrashArchive(raw)
		if err != nil {
			return err
		}
		if err := policies.validateTrash(archive); err != nil {
			return err
		}
		records = append(records, archive.Records)
	}
	for k := 1; k <= archives; k++ {
		if _, ok := files[trashArchivePath(k)]; !ok {
			return invalidInput("invalid Trash archive")
		}
	}
	raw, ok := files[trashIndexPath]
	if !ok {
		if archives > 0 {
			return invalidInput("invalid Trash archive")
		}
		for p := range files {
			if IsTrash(p) {
				return invalidInput("existing Trash content is not system managed; relocate it before enabling Trash")
			}
		}
		return nil
	}
	if len(raw) > trashFileBytes {
		return invalidInput("Trash index exceeds 8 MiB")
	}
	var index TrashIndex
	if json.Unmarshal(raw, &index) != nil || index.Version != 1 || index.Records == nil || len(index.Records) > trashFileRecords {
		return invalidInput("invalid Trash index")
	}
	if err := policies.validateTrash(index); err != nil {
		return err
	}
	return distinctTrashRecords(append(records, index.Records)...)
}
