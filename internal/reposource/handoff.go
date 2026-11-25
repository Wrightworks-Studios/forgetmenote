package reposource

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"forgetmenote/internal/workspace"
)

// Read every tree entry, not the index's supported subset. The current GitHub
// default branch must still name the exact revision selected by the preview.
func (m *Manager) NotebookSnapshot(ctx context.Context, project, id, sha, branch string) (workspace.NotebookSnapshot, error) {
	out := workspace.NotebookSnapshot{Files: map[string][]byte{}}
	err := m.read(ctx, project, id, func(c *cache, s State) error {
		repo, current, _, err := m.github.Resolve(ctx, s.Installation, s.Repository.ID)
		if err != nil {
			return err
		}
		if repo.ID != s.Repository.ID || repo.DefaultBranch != branch || current != sha || s.Downloaded != sha {
			return ErrConflict
		}
		out.Revision, out.Source, out.Repository = sha, s.ID, repo.FullName
		tree, err := c.tree(ctx, sha)
		if err != nil {
			return err
		}
		var total int64
		for _, entry := range tree {
			if entry.Mode != "100644" {
				return errors.New("handoff does not support executable files, symlinks or submodules")
			}
			if entry.Bytes < 0 || entry.Bytes > int64(workspace.MaxFileBytes()) || entry.Bytes > c.limits.FileBytes || total+entry.Bytes > workspace.MaxAuthorityBytes {
				return ErrLimit
			}
			total += entry.Bytes
		}
		return c.readBlobs(ctx, tree, func(entry Entry, b []byte) error { out.Files[entry.Path] = append([]byte{}, b...); return nil })
	})
	return out, err
}
func (m *Manager) BeginAuthority(project, actor, id, source string, generation uint64, sha string) (State, error) {
	if !identifier.MatchString(id) || len(id) != 32 || !revision.MatchString(sha) {
		return State{}, ErrDenied
	}
	if err := m.check(project, actor); err != nil {
		return State{}, err
	}
	m.mu.Lock()
	if cancel := m.running[project]; cancel != nil {
		cancel()
	}
	if cancel := m.processingJobs[project]; cancel != nil {
		cancel()
	}
	m.mu.Unlock()
	n, err := m.book(project)
	if err != nil {
		return State{}, err
	}
	if err := n.lock(); err != nil {
		return State{}, err
	}
	defer n.mu.Unlock()
	s := n.state.Sources[n.state.Active]
	if s.Handoff == id {
		return s, nil
	}
	if s.ID != source || s.Generation != generation || s.Downloaded != sha || !s.Connected || s.Handoff != "" {
		return s, ErrConflict
	}
	s.Handoff = id
	s.HandoffPaused = s.Paused
	s.Paused = true
	s.Generation++
	return s, n.save(s)
}
func (m *Manager) PinNotebook(ctx context.Context, project, id string, snapshot workspace.NotebookSnapshot) error {
	n, err := m.book(project)
	if err != nil {
		return err
	}
	return m.read(ctx, project, snapshot.Source, func(c *cache, s State) error {
		if s.Handoff != id || s.Downloaded != snapshot.Revision {
			return ErrConflict
		}
		raw, err := json.Marshal(snapshot)
		if err != nil {
			return err
		}
		rel := "notebooks/" + snapshot.Revision + ".json"
		if old, err := c.readEvidence(ctx, rel, int64(workspace.MaxAuthorityBytes*2)); err == nil {
			if hash(old) != hash(raw) {
				return ErrConflict
			}
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		size := s.EvidenceBytes
		if c.blobs == nil {
			if size, err = diskBytes(filepath.Join(c.dir, "evidence")); err != nil {
				return err
			}
		}
		if size+int64(len(raw)) > c.limits.EvidenceBytes {
			return ErrLimit
		}
		if err = c.keepEvidence(ctx, rel, raw); err != nil {
			return err
		}
		return n.addEvidence(s.ID, int64(len(raw)))
	})
}
func (m *Manager) FinishAuthority(project, id, to string, commit bool) error {
	n, err := m.book(project)
	if err != nil {
		return err
	}
	if err := n.lock(); err != nil {
		return err
	}
	defer n.mu.Unlock()
	s := n.state.Sources[n.state.Active]
	if s.Handoff == "" && s.LastHandoff == id {
		if commit && s.LastHandoffOutcome == to || !commit && s.LastHandoffOutcome == "aborted" {
			return nil
		}
		return ErrConflict
	}
	if s.Handoff != id {
		return ErrConflict
	}
	s.Handoff = ""
	s.LastHandoff = id
	s.LastHandoffOutcome = "aborted"
	if commit {
		s.LastHandoffOutcome = to
	}
	s.Generation++
	if commit {
		s.NotebookLayout = true
		s.HistoricalOnly = to == "forgetmenote"
		s.Paused = s.HistoricalOnly
		s.Pending = !s.HistoricalOnly
		s.NextAttempt = time.Time{}
	} else {
		s.Paused = s.HandoffPaused
	}
	return n.save(s)
}
func (c *cache) pinnedNotebookBlob(ctx context.Context, sha, p string) ([]byte, error) {
	raw, err := c.readEvidence(ctx, "notebooks/"+sha+".json", int64(workspace.MaxAuthorityBytes*2))
	if err != nil {
		return nil, err
	}
	var snapshot workspace.NotebookSnapshot
	if json.Unmarshal(raw, &snapshot) != nil || snapshot.Revision != sha {
		return nil, ErrUnavailable
	}
	b, ok := snapshot.Files[p]
	if !ok {
		return nil, ErrUnavailable
	}
	return b, nil
}
func notebookNote(p string) bool {
	return strings.HasSuffix(p, ".md") && p != "AGENTS.md" && !strings.HasPrefix(p, "comments/") && !strings.HasPrefix(p, "assets/") && !workspace.IsTrash(p)
}

func (s State) indexID() string {
	id := s.IndexConfig.id()
	if s.NotebookLayout {
		return hash([]byte(id + "\x00notebook-layout-v1"))
	}
	return id
}
func notebookScope(scope *workspace.ContentScope) workspace.ContentScope {
	if scope != nil {
		return *scope
	}
	return workspace.ContentScope{Policy: workspace.ContainerPolicy{Version: 1, Archives: "lower", Drafts: "exclude"}}
}
func (q SearchQuery) notebookTier(s State, ch SourceChunk) int {
	if !s.NotebookLayout {
		return 0
	}
	scope, ok := q.Scopes[s.Project]
	if !ok {
		scope = notebookScope(nil)
	}
	return scope.Tier(workspace.Metadata{Container: ch.Container})
}

type NotebookDocument struct {
	Path, NoteID, Title, Container string
}

// Listing uses the complete tree, independently of optional index exclusions.
func (m *Manager) NotebookDocuments(ctx context.Context, project, id, sha string, scope workspace.ContentScope) ([]NotebookDocument, error) {
	out := []NotebookDocument{}
	if err := scope.Validate(); err != nil {
		return out, err
	}
	err := m.read(ctx, project, id, func(c *cache, s State) error {
		if !s.NotebookLayout || sha != s.Downloaded {
			return ErrConflict
		}
		tree, err := c.tree(ctx, sha)
		if err != nil {
			return err
		}
		notes := []Entry{}
		for _, entry := range tree {
			if notebookNote(entry.Path) {
				if entry.Mode != "100644" || !entry.Supported {
					return ErrLimit
				}
				notes = append(notes, entry)
			}
		}
		return c.readBlobs(ctx, notes, func(entry Entry, b []byte) error {
			metadata, err := workspace.ParseMetadata(b)
			if err != nil {
				return err
			}
			if scope.Tier(metadata) < 0 {
				return nil
			}
			title, _ := workspace.NoteSummary(b)
			out = append(out, NotebookDocument{entry.Path, metadata.ID, title, metadata.Container})
			return nil
		})
	})
	return out, err
}
