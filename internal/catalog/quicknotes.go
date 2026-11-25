package catalog

import (
	"errors"
	"forgetmenote/internal/workspace"
	"strings"
)

// EnsureInbox is the sole provisioning exception for accounts without ordinary
// notebook creation rights. The protected binding and membership are one catalog
// write, so an acknowledged Inbox can never reopen as an ordinary shared project.
func (c *Catalog) EnsureInbox(subject string) (Project, error) {
	account, err := c.Account(subject)
	if err != nil {
		return Project{}, err
	}
	c.lock()
	defer c.mu.Unlock()
	if !c.enabledInboxSubject(subject, account.ID) {
		return Project{}, workspace.ErrForbidden
	}
	id := "inbox-" + account.ID
	if p, ok := c.state.Projects[id]; ok {
		return c.inboxProject(subject, account.ID, p)
	}
	store, created, err := c.createStore(id)
	if err != nil {
		return Project{}, err
	}
	p := Project{ID: id, Name: "Quicknotes Inbox", ProtectedInbox: account.ID, Members: map[string]string{subject: "owner"}}
	c.state.Projects[id] = p
	if err = c.persist(); err != nil {
		delete(c.state.Projects, id)
		return Project{}, c.abandonStore(id, store, created, err)
	}
	c.stores[id] = store
	p.Role = "owner"
	p.Members = nil
	return p, nil
}

func (c *Catalog) Inbox(subject string) (Project, error) {
	c.lock()
	defer c.mu.Unlock()
	account := c.state.Accounts[subject]
	if !c.enabledInboxSubject(subject, account.ID) {
		return Project{}, workspace.ErrForbidden
	}
	return c.inboxProject(subject, account.ID, c.state.Projects["inbox-"+account.ID])
}
func (c *Catalog) enabledInboxSubject(subject, id string) bool {
	return c.version >= 0 && c.state.Users[subject].Kind != "service" && len(id) == 32 && (subject == Bootstrap || ValidUser(subject) && !c.state.Users[subject].Disabled) && c.state.Accounts[subject].ID == id
}
func (c *Catalog) inboxProject(subject, id string, p Project) (Project, error) {
	if p.ID != "inbox-"+id || p.ProtectedInbox != id || len(p.Members) != 1 || p.Members[subject] != "owner" {
		return Project{}, workspace.ErrForbidden
	}
	p.Role = "owner"
	p.Members = nil
	return p, nil
}
func (c *Catalog) validateInboxes() error {
	for _, p := range c.state.Projects {
		if p.ProtectedInbox == "" {
			continue
		}
		if len(p.ProtectedInbox) != 32 || p.ID != "inbox-"+p.ProtectedInbox || len(p.Members) != 1 {
			return errors.New("invalid protected Inbox catalog binding")
		}
		for subject, role := range p.Members {
			if role != "owner" || c.state.Accounts[subject].ID != p.ProtectedInbox || strings.TrimSpace(subject) == "" {
				return errors.New("invalid protected Inbox membership")
			}
		}
	}
	return nil
}
