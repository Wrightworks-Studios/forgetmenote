package catalog

import (
	"errors"
	"sort"
	"strings"

	"forgetmenote/internal/workspace"
)

var ErrLastAdmin = errors.New("assign another active human Admin before disabling or demoting the last Admin")

func (c *Catalog) CreateService(actor, name, steward string) (User, error) {
	c.lock()
	defer c.mu.Unlock()
	if !c.admin(actor) {
		return User{}, workspace.ErrForbidden
	}
	if !validLabel(name) || !c.humanSteward(steward) {
		return User{}, errors.New("provide a name and an active human steward")
	}
	u := User{ID: "service-" + strings.ToLower(secretToken()), DisplayName: strings.TrimSpace(name), Kind: "service", Steward: steward}
	err := c.change(actor, "service.create", u.ID, map[string]string{"steward": steward}, func() error { c.state.Users[u.ID] = u; return nil })
	return u, err
}
func (c *Catalog) humanSteward(id string) bool {
	u, ok := c.state.Users[id]
	return ok && !u.Disabled && u.Kind != "service"
}
func (c *Catalog) SetServiceSteward(actor, id, steward string) error {
	c.lock()
	defer c.mu.Unlock()
	if !c.admin(actor) || c.state.Users[id].Kind != "service" {
		return workspace.ErrForbidden
	}
	if !c.humanSteward(steward) {
		return errors.New("select an active human steward")
	}
	return c.change(actor, "service.steward", id, map[string]string{"steward": steward}, func() error { u := c.state.Users[id]; u.Steward = steward; c.state.Users[id] = u; return nil })
}
func (c *Catalog) UserInfo(actor, id string) (User, error) {
	c.lock()
	defer c.mu.Unlock()
	if !c.admin(actor) {
		return User{}, workspace.ErrForbidden
	}
	u, ok := c.state.Users[id]
	if !ok {
		return User{}, workspace.ErrForbidden
	}
	return u, nil
}
func (c *Catalog) ReassignOwnership(actor, user, replacement string, projects []string) error {
	c.lock()
	defer c.mu.Unlock()
	if !c.admin(actor) {
		return workspace.ErrForbidden
	}
	if user == replacement || !c.humanSteward(replacement) || len(projects) == 0 || len(projects) > 256 {
		return errors.New("select notebooks and an active replacement owner")
	}
	return c.change(actor, "ownership.reassign", user, map[string]string{"replacement": replacement, "projects": strings.Join(projects, ",")}, func() error {
		for _, id := range projects {
			p, ok := c.state.Projects[id]
			if !ok || p.protected() || p.Members[user] != "owner" {
				return workspace.ErrForbidden
			}
			p.Members[replacement] = "owner"
			delete(p.Members, user)
			c.state.ProjectAccessEpoch[accessKey(id, user)]++
			c.state.Projects[id] = p
		}
		return nil
	})
}

type Offboarding struct {
	User     User                       `json:"user"`
	Groups   []string                   `json:"groups"`
	Owned    []string                   `json:"owned"`
	Access   map[string]EffectiveAccess `json:"access"`
	Services []User                     `json:"services"`
}

func (c *Catalog) Offboarding(actor, id string) (Offboarding, error) {
	c.lock()
	defer c.mu.Unlock()
	if !c.admin(actor) {
		return Offboarding{}, workspace.ErrForbidden
	}
	user, ok := c.state.Users[id]
	if !ok {
		return Offboarding{}, workspace.ErrForbidden
	}
	out := Offboarding{User: user, Groups: []string{}, Owned: []string{}, Access: map[string]EffectiveAccess{}, Services: []User{}}
	for _, g := range c.state.Groups {
		for _, member := range g.Members {
			if member == id {
				out.Groups = append(out.Groups, g.ID)
				break
			}
		}
	}
	for _, p := range c.state.Projects {
		access := c.effective(id, p)
		if access.Role != "" || access.Direct != "" || len(access.Groups) > 0 {
			out.Access[p.ID] = access
		}
		if !p.protected() && p.Members[id] == "owner" {
			out.Owned = append(out.Owned, p.ID)
		}
	}
	for _, u := range c.state.Users {
		if u.Kind == "service" && u.Steward == id {
			out.Services = append(out.Services, u)
		}
	}
	sort.Strings(out.Groups)
	sort.Strings(out.Owned)
	sort.Slice(out.Services, func(i, j int) bool { return out.Services[i].ID < out.Services[j].ID })
	return out, nil
}
