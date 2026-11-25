package catalog

import (
	"errors"
	"forgetmenote/internal/workspace"
	"sort"
	"strings"
)

var ErrLastOwner = errors.New("assign another active Owner before removing the last Owner")

func (c *Catalog) canCreateProjects(id string) bool {
	return c.version >= 0 && (c.admin(id) || !c.state.Users[id].Disabled && c.state.Users[id].Kind != "service" && c.state.Users[id].CanCreateProjects)
}
func (c *Catalog) CanCreateProjects(id string) bool {
	c.lock()
	defer c.mu.Unlock()
	return c.canCreateProjects(id)
}

// Omitted fields preserve settings made by newer clients and by other operators.
type UserUpdate struct {
	ID                string  `json:"id"`
	Admin             *bool   `json:"admin,omitempty"`
	Disabled          *bool   `json:"disabled,omitempty"`
	DisplayName       *string `json:"displayName,omitempty"`
	CanCreateProjects *bool   `json:"canCreateProjects,omitempty"`
}

func (c *Catalog) UpdateUser(actor string, change UserUpdate) error {
	c.lock()
	defer c.mu.Unlock()
	if !c.admin(actor) {
		return workspace.ErrForbidden
	}
	u := c.state.Users[change.ID]
	u.ID = change.ID
	if change.Admin != nil {
		u.Admin = *change.Admin
	}
	if change.Disabled != nil {
		u.Disabled = *change.Disabled
	}
	if change.DisplayName != nil {
		u.DisplayName = *change.DisplayName
	}
	if change.CanCreateProjects != nil {
		u.CanCreateProjects = *change.CanCreateProjects
	}
	return c.setUser(actor, u)
}

// A bounded, deliberate search lets Owners add existing people without exposing
// account status, provider claims, credentials, or unrelated memberships.
type Person struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (c *Catalog) FindPeople(actor, project, query string) ([]Person, error) {
	c.lock()
	defer c.mu.Unlock()
	if !c.owns(actor, project) {
		return nil, workspace.ErrForbidden
	}
	query = strings.ToLower(strings.TrimSpace(query))
	if len(query) > 120 {
		return nil, errors.New("search must be at most 120 characters")
	}
	out := []Person{}
	if len([]rune(query)) < 2 {
		return out, nil
	}
	names := map[string]string{}
	for name, credential := range c.state.Passwords {
		names[credential.User] = name
	}
	for id, u := range c.state.Users {
		if u.Disabled {
			continue
		}
		name := u.DisplayName
		if name == "" {
			name = names[id]
		}
		if name == "" {
			name = id
		}
		if strings.Contains(strings.ToLower(name), query) || strings.EqualFold(id, query) {
			out = append(out, Person{id, name})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name == out[j].Name {
			return out[i].ID < out[j].ID
		}
		return out[i].Name < out[j].Name
	})
	if len(out) > 50 {
		out = out[:50]
	}
	return out, nil
}
