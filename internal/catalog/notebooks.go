package catalog

import (
	"errors"
	"fmt"
	"forgetmenote/internal/workspace"
	"strconv"
	"strings"
)

type NotebookAuthority struct {
	Type       string `json:"type"`
	Generation uint64 `json:"generation"`
}

func notebookAuthority(p Project) *NotebookAuthority {
	t := "forgetmenote"
	if p.Kind == "repository" {
		t = "github"
	}
	return &NotebookAuthority{Type: t, Generation: max(1, p.AuthorityGeneration)}
}
func (c *Catalog) Authority(id string) NotebookAuthority {
	c.lock()
	defer c.mu.Unlock()
	return *notebookAuthority(c.state.Projects[id])
}

type NotebookCapabilities struct {
	Read    bool `json:"read"`
	Edit    bool `json:"edit"`
	Refresh bool `json:"refresh"`
	Manage  bool `json:"manage"`
	Backup  bool `json:"backup"`
}

func capabilities(kind, role string) *NotebookCapabilities {
	return &NotebookCapabilities{Read: rank(role) >= 1, Edit: kind != "repository" && rank(role) >= 3, Refresh: rank(role) >= 4, Manage: rank(role) >= 4, Backup: kind != "repository" && rank(role) >= 4}
}

func (c *Catalog) Kind(id string) string {
	c.lock()
	defer c.mu.Unlock()
	kind := c.state.Projects[id].Kind
	if kind == "" {
		return "notes"
	}
	return kind
}

// StorageOwner identifies the only host permitted to upload this notebook:
// "local", the instance itself, for every notebook it lists, since each
// lives in its notes store; "" for any other ID. Managed backup bindings
// record it.
func (c *Catalog) StorageOwner(id string) string {
	c.lock()
	defer c.mu.Unlock()
	if _, ok := c.state.Projects[id]; !ok {
		return ""
	}
	return "local"
}

// CheckCreate validates a proposed ID without reserving it or creating content.
// CreateKind repeats this check under its own lock at final submission.
func (c *Catalog) CheckCreate(actor, id, name string) error {
	c.lock()
	defer c.mu.Unlock()
	return c.checkCreate(actor, id, name)
}
func (c *Catalog) checkCreate(actor, id, name string) error {
	if !c.canCreateProjects(actor) {
		return workspace.ErrForbidden
	}
	if !projectID.MatchString(id) || strings.TrimSpace(name) == "" || len(name) > 120 {
		return errors.New("use a lowercase notebook ID and a name of at most 120 characters")
	}
	if ReservedNotebookID(id) {
		return errors.New("notebook IDs beginning with inbox- or assistant-, and " + TaskQueuesProject + ", are reserved for system notebooks")
	}
	if _, exists := c.state.Projects[id]; exists || c.importing[id] {
		return workspace.ErrConflict
	}
	return nil
}

// ReservedNotebookID reports IDs that only EnsureInbox, SaveAssistant and
// SetTaskQueues may create, because they derive from a person's stable account
// ID or hold every task queue. Ordinary creation, provisioning and import must
// refuse them so no one can pre-claim a protected notebook.
func ReservedNotebookID(id string) bool {
	return strings.HasPrefix(id, "inbox-") || strings.HasPrefix(id, "assistant-") || id == TaskQueuesProject
}

// SetOpenAsWiki chooses whether members open a notes notebook in the wiki
// layout by default; each person can still switch layouts. Only the
// notebook's owners change it.
func (c *Catalog) SetOpenAsWiki(actor, project string, wiki bool) error {
	c.lock()
	defer c.mu.Unlock()
	if !c.owns(actor, project) {
		return workspace.ErrForbidden
	}
	p := c.state.Projects[project]
	if p.Kind == "repository" || p.protected() {
		return fmt.Errorf("%w: only shared notes notebooks can open as a wiki", ErrLocalInput)
	}
	if p.OpenAsWiki == wiki {
		return nil
	}
	return c.change(actor, "notebook.wiki", project, map[string]string{"openAsWiki": strconv.FormatBool(wiki)}, func() error {
		p := c.state.Projects[project]
		p.OpenAsWiki = wiki
		c.state.Projects[project] = p
		return nil
	})
}
