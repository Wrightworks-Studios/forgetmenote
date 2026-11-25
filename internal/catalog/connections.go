package catalog

import (
	"errors"
	"regexp"
	"slices"

	"forgetmenote/internal/workspace"
)

type ConnectionGrant struct {
	Connection string              `json:"connection"`
	Revision   uint64              `json:"revision"`
	Users      map[string][]string `json:"users"`
	Groups     map[string][]string `json:"groups"`
}

var connectionID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,95}$`)

func (c *Catalog) ConnectionAccess(actor, id string) (ConnectionGrant, error) {
	c.lock()
	defer c.mu.Unlock()
	if !c.admin(actor) {
		return ConnectionGrant{}, workspace.ErrForbidden
	}
	g := c.state.ConnectionGrants[id]
	g.Connection = id
	g.Users = cloneOperations(g.Users)
	g.Groups = cloneOperations(g.Groups)
	return g, nil
}
func cloneOperations(in map[string][]string) map[string][]string {
	out := map[string][]string{}
	for k, v := range in {
		out[k] = slices.Clone(v)
	}
	return out
}
func (c *Catalog) SetConnectionAccess(actor string, in ConnectionGrant) (ConnectionGrant, error) {
	c.lock()
	defer c.mu.Unlock()
	if !c.admin(actor) {
		return ConnectionGrant{}, workspace.ErrForbidden
	}
	if !connectionID.MatchString(in.Connection) || len(in.Users)+len(in.Groups) > 4096 {
		return ConnectionGrant{}, errors.New("invalid connection grant")
	}
	if c.state.ConnectionGrants[in.Connection].Revision != in.Revision {
		return ConnectionGrant{}, workspace.ErrConflict
	}
	for user, ops := range in.Users {
		if _, ok := c.state.Users[user]; !ok {
			return ConnectionGrant{}, errors.New("unknown account")
		}
		if !validOperations(ops) {
			return ConnectionGrant{}, errors.New("invalid operations")
		}
	}
	for group, ops := range in.Groups {
		if _, ok := c.state.Groups[group]; !ok {
			return ConnectionGrant{}, errors.New("unknown group")
		}
		if !validOperations(ops) {
			return ConnectionGrant{}, errors.New("invalid operations")
		}
	}
	err := c.change(actor, "connection.grant", in.Connection, nil, func() error {
		in.Revision++
		in.Users = cloneOperations(in.Users)
		in.Groups = cloneOperations(in.Groups)
		c.state.ConnectionGrants[in.Connection] = in
		return nil
	})
	in.Users = cloneOperations(in.Users)
	in.Groups = cloneOperations(in.Groups)
	return in, err
}
func validOperations(ops []string) bool {
	if len(ops) > 256 {
		return false
	}
	for _, op := range ops {
		if !connectionID.MatchString(op) {
			return false
		}
	}
	return true
}
func (c *Catalog) ConnectionAllowed(actor, id, operation string) bool {
	c.lock()
	defer c.mu.Unlock()
	if c.version < 0 {
		return false
	}
	if c.admin(actor) {
		return true
	}
	u, ok := c.state.Users[actor]
	if !ok || u.Disabled {
		return false
	}
	g := c.state.ConnectionGrants[id]
	allows := func(ops []string) bool { return operation == "" && len(ops) > 0 || slices.Contains(ops, operation) }
	if allows(g.Users[actor]) {
		return true
	}
	for group, ops := range g.Groups {
		if slices.Contains(c.state.Groups[group].Members, actor) && allows(ops) {
			return true
		}
	}
	return false
}

// BindExternalIdentity is operator administration; the identity must not be
// inferred from matching email addresses on an incoming assertion.
func (c *Catalog) BindExternalIdentity(actor, issuer, sub, user string) error {
	c.lock()
	defer c.mu.Unlock()
	if !c.admin(actor) {
		return workspace.ErrForbidden
	}
	u, ok := c.state.Users[user]
	if !ok || u.Kind == "service" || u.Disabled || issuer == "" || sub == "" || len(sub) > 1024 {
		return workspace.ErrForbidden
	}
	for _, v := range c.state.OIDC {
		if v.Issuer == issuer && v.Subject == sub {
			if v.User == user {
				return nil
			}
			return errors.New("external identity already bound")
		}
	}
	return c.change(actor, "account.external.bind", user, map[string]string{"issuer": issuer}, func() error {
		c.state.OIDC = append(c.state.OIDC, ExternalIdentity{Issuer: issuer, Subject: sub, User: user})
		return nil
	})
}
