// Package workspace owns every read and write of one notebook kept in the
// notes store (internal/notestore): its notes, attachments, discussions and
// policy files, their revision checks, and their history as Git commits.
package workspace

import (
	"context"
	"errors"
	"fmt"
	"forgetmenote/internal/capacity"
	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
)

var ErrConflict = errors.New("the notebook changed; reload and reconcile your draft")
var ErrDirty = errors.New("notebook contains external changes; commit or resolve them before saving")
var ErrForbidden = errors.New("this path is not accessible")
var ErrReadOnly = fmt.Errorf("%w: notebook is GitHub-authoritative; edit its upstream repository", ErrForbidden)

// ErrInvalid classifies a request the caller must correct before it can
// succeed: a malformed or unsupported value, one past a bound of a single
// request, or notebook content a handoff cannot take. Transports answer 400.
var ErrInvalid = errors.New("invalid notebook request")

// ErrCapacity classifies a refusal because the notebook holds as much of
// something as it may, such as lock rules, Trash records, discussions or
// live notes, until some is released. Transports answer 429, which MCP
// marks retryable.
var ErrCapacity = errors.New("notebook capacity reached")

// ErrLimit classifies a refusal because what a request needs exceeds a
// fixed limit that belongs to the notebook or to one of its notes, such as
// the history of a note the Trash may record or the files one change set
// may list, so no retry can succeed. Transports answer 413, which MCP does
// not mark retryable.
var ErrLimit = errors.New("notebook limit exceeded")

// A refusal is an error of one class above that keeps its own message.
type refusal struct {
	message string
	class   error
}

func (r *refusal) Error() string { return r.message }
func (r *refusal) Unwrap() error { return r.class }

// invalidInput is an ErrInvalid refusal that says message.
func invalidInput(message string) error { return &refusal{message, ErrInvalid} }

// capacityReached is an ErrCapacity refusal that says message.
func capacityReached(message string) error { return &refusal{message, ErrCapacity} }

// limitExceeded is an ErrLimit refusal that says message.
func limitExceeded(message string) error { return &refusal{message, ErrLimit} }

func MaxFileBytes() int { return capacity.FileBytes.Limit() }

// Archived originals can exceed the ordinary note/attachment budget. Reads and
// restores must honor the same source budget that admitted their capture.
func MaxReadBytes(path string) int {
	if strings.HasPrefix(path, "assets/sources/") && SourceOriginalPath(path) {
		return max(MaxFileBytes(), MaxSourceBytes())
	}
	return MaxFileBytes()
}

type Document struct {
	Path     string `json:"path"`
	Content  []byte `json:"content"`
	Revision string `json:"revision"`
	Version  string `json:"version"`
}
type Commit struct {
	AuthorName     string `json:"authorName"`
	AuthorEmail    string `json:"authorEmail"`
	CommitterName  string `json:"committerName"`
	CommitterEmail string `json:"committerEmail"`
	AuthoredAt     string `json:"authoredAt"`
	Source         string `json:"source,omitempty"`
	UserID         string `json:"userId,omitempty"`
	Client         string `json:"client,omitempty"`
	Revision       string `json:"revision"`
	Message        string `json:"message"`
	Path           string `json:"path,omitempty"`
}
type Entry struct {
	TrashID       string    `json:"trashId,omitempty"`
	Title         string    `json:"title,omitempty"`
	Excerpt       string    `json:"excerpt,omitempty"`
	Path          string    `json:"path"`
	Editable      bool      `json:"editable"`
	Metadata      *Metadata `json:"metadata,omitempty"`
	MetadataError string    `json:"metadataError,omitempty"`
}
type Status struct {
	Revision string `json:"revision"`
	Dirty    bool   `json:"dirty"`
}

type Store struct {
	backend             *dbNotebook
	readOnly            atomic.Bool
	authority           AuthorityState
	authorityGeneration atomic.Uint64
	identityHead        string
	identityIDs         map[string]string
	identityObjects     map[string]string
	liveWaits           map[string]*liveWaiter
	liveWaiting         int
	liveClosed          bool
	liveStorageFailed   bool
	live                map[string]*liveRecord
	checkpointing       bool
	watchers            map[chan struct{}]struct{}
	observedHead        string
	Root                string
	mu                  sync.Mutex
	locksRevision       string
	trashRevision       string
	trashCache          TrashIndex
	projectLocks        []string
	browseExcluded      []string
	metadataPaths       []string
	metadataHead        string
	metadataObjects     map[string]string
	metadataCache       map[string]Entry
	committedReads      committedReads
	// A notebook has no working tree: notes is its notebook in the notes
	// store, and Root only names its state.
	notes     *notestore.Notebook
	noteStore *notestore.Store
	notebook  string
}

var revisionPattern = regexp.MustCompile(`^[a-f0-9]{40}$`)

func matchesPrefix(path string, prefixes []string) bool {
	for _, prefix := range prefixes {
		prefix = strings.Trim(strings.TrimSpace(prefix), "/")
		if prefix != "" && (path == prefix || strings.HasPrefix(path, prefix+"/")) {
			return true
		}
	}
	return false
}
func (s *Store) editable(path string) bool {
	return !s.readOnly.Load() && !IsTrash(path) && !matchesPrefix(path, s.projectLocks)
}

// SetReadOnly applies the catalog's immutable content origin to this store.
// Connection management remains outside the content store.
func (s *Store) SetReadOnly(value bool) { s.readOnly.Store(value) }
func (s *Store) resolve(path string, write bool) (string, error) {
	if path == "" || len(path) > 512 || strings.ContainsAny(path, "\\\x00\r\n") || filepath.IsAbs(path) {
		return "", ErrForbidden
	}
	for _, p := range strings.Split(path, "/") {
		if p == "" || strings.HasPrefix(p, ".") {
			return "", ErrForbidden
		}
	}
	if write && (!gitPath(path) || !s.editable(path)) {
		return "", ErrForbidden
	}
	// No file system lies under Root; tree entries are checked by mode.
	return filepath.Join(s.Root, path), nil
}

// gitPath reports whether a revision may write path: git fsck refuses a
// tree holding any name it takes for .git, such as git~1, and a notebook
// whose export fails fsck can be neither backed up nor restored. Reads stay
// permissive, so a file an earlier build stored under such a name can still
// be read, moved elsewhere or deleted.
func gitPath(path string) bool {
	for _, name := range strings.Split(path, "/") {
		if !gitobj.ValidName(name) {
			return false
		}
	}
	return true
}

// Search is Find of query alone: a literal, case-folded search of committed
// notes, not shell syntax.
func (s *Store) Search(ctx context.Context, query string) ([]Entry, error) {
	return s.Find(ctx, query, "", "", "")
}
func filterFoundEntries(results []Entry, tag, status, pathPrefix string) []Entry {
	filtered := []Entry{}
	for _, entry := range results {
		if !strings.HasPrefix(entry.Path, pathPrefix) {
			continue
		}
		if status != "" && (entry.Metadata == nil || !strings.EqualFold(entry.Metadata.Status, status)) {
			continue
		}
		if tag != "" {
			found := false
			if entry.Metadata != nil {
				for _, value := range entry.Metadata.Tags {
					if strings.EqualFold(value, tag) {
						found = true
						break
					}
				}
			}
			if !found {
				continue
			}
		}
		filtered = append(filtered, entry)
	}
	results = filtered
	if len(results) > 100 {
		results = results[:100]
	}
	return results
}
