package catalog

import (
	"forgetmenote/internal/workspace"
	"strings"
)

func (c *Catalog) displayName(id string) string {
	if id == Bootstrap {
		return "Recovery administrator"
	}
	user, known := c.state.Users[id]
	if !known || user.Disabled {
		return "Former contributor"
	}
	if name := strings.TrimSpace(user.DisplayName); name != "" {
		return name
	}
	for username, credential := range c.state.Passwords {
		if credential.User == id {
			return username
		}
	}
	if user.Kind == "service" {
		return "Automation account"
	}
	return "Contributor"
}

// SubjectName presents only the authenticated account, without its credential
// or synthetic Git identity. The caller still owns authentication.
func (c *Catalog) SubjectName(subject string) (string, error) {
	c.lock()
	defer c.mu.Unlock()
	if c.version < 0 || subject != Bootstrap && (!ValidUser(subject) || c.state.Users[subject].Disabled) {
		return "", workspace.ErrForbidden
	}
	return c.displayName(subject), nil
}

// ParticipantIdentity keeps login subjects separate from opaque Git account
// identities. Neither namespace can impersonate the other for presentation.
type ParticipantIdentity struct{ Subject, Account string }

// ParticipantNames enriches already-read notebook provenance, without serving
// as an instance directory. Unrelated accounts are never named. Historical
// identifiers remain in content/receipts instead of being rewritten.
func (c *Catalog) ParticipantNames(actor, notebook string, identities []ParticipantIdentity) ([]string, error) {
	c.lock()
	defer c.mu.Unlock()
	project, ok := c.state.Projects[notebook]
	if !ok || rank(c.role(actor, project)) < rank("read") || c.state.Transfers[notebook].Phase == "publishing" {
		return nil, workspace.ErrForbidden
	}
	accounts := map[string]string{}
	for _, identity := range identities {
		if identity.Account != "" {
			accounts[identity.Account] = ""
		}
	}
	if len(accounts) > 0 {
		for id, account := range c.state.Accounts {
			if _, wanted := accounts[account.ID]; wanted && account.ID != "" {
				accounts[account.ID] = id
			}
		}
	}
	out := make([]string, len(identities))
	for i, identity := range identities {
		id := identity.Subject
		if identity.Account != "" {
			id = accounts[identity.Account]
		}
		out[i] = "Former contributor"
		if id != "" && rank(c.role(id, project)) >= rank("read") {
			out[i] = c.displayName(id)
		}
	}
	return out, nil
}
