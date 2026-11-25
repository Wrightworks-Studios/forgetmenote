package catalog

import "forgetmenote/internal/workspace"

// WithProjectAuthority makes revocation and a guarded organizer transaction
// mutually exclusive. The callback must not call Catalog methods.
func (c *Catalog) WithProjectAuthority(user, project, accountID, required string, userEpoch, projectEpoch uint64, action func(*workspace.Store) error) error {
	c.lock()
	defer c.mu.Unlock()
	p, ok := c.state.Projects[project]
	if !ok || activeTransfer(c.state.Transfers[project]) || p.Kind == "repository" && required == "edit" || p.ProtectedInbox != "" || c.state.AccessEpoch[user] != userEpoch || c.state.ProjectAccessEpoch[accessKey(project, user)] != projectEpoch || c.state.Accounts[user].ID != accountID || rank(required) == 0 || rank(c.role(user, p)) < rank(required) {
		return workspace.ErrForbidden
	}
	return action(c.stores[project])
}

// accessKey names one person's access to one notebook. Notebook IDs never
// contain a slash, so the key is unambiguous; Postgres jsonb refuses the NUL
// separator file catalogs used.
func accessKey(project, user string) string { return project + "/" + user }

func (c *Catalog) ProjectAccessEpoch(user, project string) uint64 {
	c.lock()
	defer c.mu.Unlock()
	return c.state.ProjectAccessEpoch[accessKey(project, user)]
}
