package catalog

import (
	"errors"
	"strings"
	"time"

	"forgetmenote/internal/workspace"
)

// A single record per notebook owns the handoff. Content staging belongs to the
// existing storage owner, never to this catalog or a gateway-local repository.
type AuthorityTransfer struct {
	Files                  int    `json:"files"`
	Notes                  int    `json:"notes"`
	Attachments            int    `json:"attachments"`
	Bytes                  int    `json:"bytes"`
	ID                     string `json:"id"`
	Project                string `json:"project"`
	Actor                  string `json:"actor"`
	Epoch                  uint64 `json:"epoch"`
	AccessEpoch            uint64 `json:"accessEpoch"`
	Generation             uint64 `json:"generation"`
	From                   string `json:"from"`
	To                     string `json:"to"`
	Revision               string `json:"revision"`
	Source                 string `json:"source"`
	SourceGeneration       uint64 `json:"sourceGeneration"`
	SourceRevision         string `json:"sourceRevision"`
	SourceRepository       int64  `json:"sourceRepository"`
	SourceBranch           string `json:"sourceBranch"`
	Repository             string `json:"repository"`
	SnapshotHash           string `json:"snapshotHash"`
	BackupGeneration       uint64 `json:"backupGeneration,omitempty"`
	OperatorWritersStopped bool   `json:"operatorWritersStopped"`
	Phase                  string `json:"phase"`
	Expires                int64  `json:"expires"`
	ResultRevision         string `json:"resultRevision,omitempty"`
	Error                  string `json:"error,omitempty"`
}

func (c *Catalog) transferOwner(actor, project string) error {
	p, ok := c.state.Projects[project]
	if !ok || p.protected() || !c.owns(actor, project) {
		return workspace.ErrForbidden
	}
	return nil
}
func activeTransfer(p AuthorityTransfer) bool {
	return p.Phase == "preparing" || p.Phase == "publishing"
}
func (c *Catalog) PreviewAuthority(actor, project string, in AuthorityTransfer) (AuthorityTransfer, error) {
	c.lock()
	defer c.mu.Unlock()
	if err := c.transferOwner(actor, project); err != nil {
		return in, err
	}
	old := c.state.Transfers[project]
	if activeTransfer(old) {
		return in, workspace.ErrConflict
	}
	authority := notebookAuthority(c.state.Projects[project])
	if (in.To != "github" && in.To != "forgetmenote") || in.To == authority.Type {
		return in, errors.New("choose a different notebook authority")
	}
	id, err := workspace.NewNoteID()
	if err != nil {
		return in, err
	}
	in.ID, in.Project, in.Actor, in.From, in.Generation = id, project, actor, authority.Type, authority.Generation
	in.Epoch, in.AccessEpoch = c.state.AccessEpoch[actor], c.state.ProjectAccessEpoch[accessKey(project, actor)]
	in.Phase, in.Expires = "preview", time.Now().Add(15*time.Minute).Unix()
	if c.state.Transfers == nil {
		c.state.Transfers = map[string]AuthorityTransfer{}
	}
	c.state.Transfers[project] = in
	if err = c.persist(); err != nil {
		if old.ID == "" {
			delete(c.state.Transfers, project)
		} else {
			c.state.Transfers[project] = old
		}
	}
	return in, err
}

func (c *Catalog) validateAuthorityTransfers() error {
	for project, p := range c.state.Transfers {
		book, ok := c.state.Projects[project]
		if !ok || project != p.Project || !workspace.ValidNoteID(p.ID) || p.Actor == "" || p.Generation == 0 || p.From == p.To || (p.From != "forgetmenote" && p.From != "github") || (p.To != "forgetmenote" && p.To != "github") || len(p.Revision) != 40 || strings.Trim(p.Revision, "0123456789abcdef") != "" {
			return errors.New("invalid notebook authority transfer; restore the full instance")
		}
		published := p.Phase == "publishing" || p.Phase == "complete"
		if !published && p.Phase != "preview" && p.Phase != "preparing" && p.Phase != "failed" {
			return errors.New("unknown notebook authority transfer phase")
		}
		expectedType, expectedGeneration := p.From, p.Generation
		if published {
			expectedType, expectedGeneration = p.To, p.Generation+1
		}
		actual := notebookAuthority(book)
		if actual.Type != expectedType || actual.Generation != expectedGeneration {
			return errors.New("notebook authority and transfer receipt disagree")
		}
	}
	return nil
}
func (c *Catalog) checkTransfer(actor, project, id string) (AuthorityTransfer, error) {
	p := c.state.Transfers[project]
	if err := c.transferOwner(actor, project); err != nil {
		return p, err
	}
	if p.ID != id || p.Actor != actor {
		return p, workspace.ErrConflict
	}
	if p.Epoch != c.state.AccessEpoch[actor] || p.AccessEpoch != c.state.ProjectAccessEpoch[accessKey(project, actor)] {
		return p, workspace.ErrForbidden
	}
	return p, nil
}
func (c *Catalog) BeginAuthority(actor, project, id string) (AuthorityTransfer, *workspace.Store, error) {
	c.lock()
	defer c.mu.Unlock()
	p, err := c.checkTransfer(actor, project, id)
	if err != nil {
		return p, nil, err
	}
	if p.Phase == "complete" || p.Phase == "publishing" || p.Phase == "preparing" {
		return p, c.stores[project], nil
	}
	if p.Phase != "preview" || p.Expires <= time.Now().Unix() || p.Generation != notebookAuthority(c.state.Projects[project]).Generation {
		return p, nil, workspace.ErrConflict
	}
	old := p
	p.Phase = "preparing"
	c.state.Transfers[project] = p
	if err = c.persist(); err != nil {
		c.state.Transfers[project] = old
		return p, nil, err
	}
	return p, c.stores[project], nil
}

// Publish changes only routing/authority. Reads stay unavailable until the owner
// and source manager complete the already-prepared transaction.
func (c *Catalog) PublishAuthority(actor, project, id string) (AuthorityTransfer, error) {
	c.lock()
	defer c.mu.Unlock()
	p, err := c.checkTransfer(actor, project, id)
	if err != nil {
		return p, err
	}
	if p.Phase == "publishing" || p.Phase == "complete" {
		return p, nil
	}
	book := c.state.Projects[project]
	if p.Phase != "preparing" || notebookAuthority(book).Generation != p.Generation {
		return p, workspace.ErrConflict
	}

	book.Kind = "notes"
	if p.To == "github" {
		book.Kind = "repository"
	}
	book.AuthorityGeneration = p.Generation + 1
	p.Phase = "publishing"
	c.state.Projects[project], c.state.Transfers[project] = book, p
	err = c.persist()
	// An uncertain catalog replacement stays fenced. A retry must persist the
	// publication decision before the storage owner is allowed to commit.
	return p, err
}
func (c *Catalog) CompleteAuthority(project, id, revision string) error {
	c.lock()
	defer c.mu.Unlock()
	p := c.state.Transfers[project]
	if p.ID != id {
		return workspace.ErrConflict
	}
	if p.Phase == "complete" {
		return nil
	}
	if p.Phase != "publishing" {
		return workspace.ErrConflict
	}
	old := p
	p.Phase = "complete"
	p.ResultRevision = revision
	c.state.Transfers[project] = p
	if err := c.persist(); err != nil {
		c.state.Transfers[project] = old
		return err
	}
	return nil
}
func (c *Catalog) AbortAuthority(project, id, message string) error {
	c.lock()
	defer c.mu.Unlock()
	p := c.state.Transfers[project]
	if p.ID != id || p.Phase == "publishing" || p.Phase == "complete" {
		return workspace.ErrConflict
	}
	old := p
	p.Phase = "failed"
	p.Error = message
	c.state.Transfers[project] = p
	if err := c.persist(); err != nil {
		c.state.Transfers[project] = old
		return err
	}
	return nil
}
func (c *Catalog) AuthorityTransfer(actor, project string) (AuthorityTransfer, error) {
	c.lock()
	defer c.mu.Unlock()
	if err := c.transferOwner(actor, project); err != nil {
		return AuthorityTransfer{}, err
	}
	return c.state.Transfers[project], nil
}

// Recovery uses the operator-owned catalog after startup, not a user credential.
func (c *Catalog) PendingAuthorities() []AuthorityTransfer {
	c.lock()
	defer c.mu.Unlock()
	out := []AuthorityTransfer{}
	for _, p := range c.state.Transfers {
		if activeTransfer(p) {
			out = append(out, p)
		}
	}
	return out
}

// AuthorityStore rereads the catalog first: recovery and cancellation act on a
// transfer another gateway may have just advanced.
func (c *Catalog) AuthorityStore(project, id string) (*workspace.Store, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.checked = time.Time{}
	c.refresh()
	if c.version < 0 {
		return nil, workspace.ErrForbidden
	}
	p := c.state.Transfers[project]
	if p.ID != id || !activeTransfer(p) {
		return nil, workspace.ErrConflict
	}
	return c.stores[project], nil
}
func (c *Catalog) AuthorityChanging(project string) bool {
	c.lock()
	defer c.mu.Unlock()
	return activeTransfer(c.state.Transfers[project])
}

// CheckNotebookOwner is for provider authorization while a handoff fences content
// APIs. The provider manager also checks its own paused/transfer generation.
func (c *Catalog) CheckNotebookOwner(actor, project string) error {
	c.lock()
	defer c.mu.Unlock()
	if !c.owns(actor, project) {
		return workspace.ErrForbidden
	}
	return nil
}

func (c *Catalog) EnsureAuthorityPublication(project, id string) error {
	c.lock()
	defer c.mu.Unlock()
	p := c.state.Transfers[project]
	if p.ID != id || p.Phase != "publishing" {
		return workspace.ErrConflict
	}
	return c.persist()
}
