package catalog

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"forgetmenote/internal/branding"
	"forgetmenote/internal/pg"
	"forgetmenote/internal/workspace"
)

const activityLimit = 5000

type Group struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Members  []string `json:"members"`
	Revision uint64   `json:"revision"`
}
type EffectiveAccess struct {
	Role   string   `json:"role"`
	Direct string   `json:"direct,omitempty"`
	Groups []string `json:"groups,omitempty"`
}
type Activity struct {
	ID      uint64            `json:"id"`
	Time    time.Time         `json:"time"`
	Actor   string            `json:"actor"`
	Action  string            `json:"action"`
	Target  string            `json:"target"`
	Outcome string            `json:"outcome"`
	Details map[string]string `json:"details,omitempty"`
}
type Organization struct {
	Name        string       `json:"name"`
	Groups      []Group      `json:"groups"`
	Invitations []Invitation `json:"invitations"`
	Activity    []Activity   `json:"activity"`
}

func (c *Catalog) initOrganization() error {
	if c.state.ConnectionGrants == nil {
		c.state.ConnectionGrants = map[string]ConnectionGrant{}
	}
	if c.state.Accounts == nil {
		c.state.Accounts = map[string]Account{}
	}
	if c.state.Passwords == nil {
		c.state.Passwords = map[string]LocalCredential{}
	}
	if c.state.AccessEpoch == nil {
		c.state.AccessEpoch = map[string]uint64{}
	}
	if c.state.Transfers == nil {
		c.state.Transfers = map[string]AuthorityTransfer{}
	}
	if c.state.Groups == nil {
		c.state.Groups = map[string]Group{}
	}
	if c.state.Invitations == nil {
		c.state.Invitations = map[string]Invitation{}
	}
	if c.state.ProjectAccessEpoch == nil {
		c.state.ProjectAccessEpoch = map[string]uint64{}
	}
	for id, g := range c.state.Groups {
		if !projectID.MatchString(id) || g.ID != id || !validLabel(g.Name) || g.Revision == 0 || len(g.Members) > 4096 {
			return errors.New("invalid organization group")
		}
		for _, user := range g.Members {
			if _, ok := c.state.Users[user]; !ok {
				return errors.New("unknown group member")
			}
		}
	}
	for _, u := range c.state.Users {
		if u.Kind != "" && u.Kind != "human" && u.Kind != "service" || u.Kind == "service" && (u.Admin || u.CanCreateProjects) {
			return errors.New("invalid account kind")
		}
	}
	for _, p := range c.state.Projects {
		for id, role := range p.GroupRoles {
			if _, ok := c.state.Groups[id]; !ok || rank(role) < 1 || rank(role) > 3 || p.protected() {
				return errors.New("invalid notebook group access")
			}
		}
	}
	return nil
}
func validLabel(s string) bool {
	return strings.TrimSpace(s) != "" && len(s) <= 120 && !strings.ContainsAny(s, "\r\n\x00")
}

// change owns a catalog transaction: authorization, state and its audit receipt
// persist together. A private copy keeps failed writes out of live permissions.
// The caller holds c.mu; the callback must not invoke public Catalog methods.
func (c *Catalog) change(actor, action, target string, details map[string]string, fn func() error) error {
	return c.changeOutcome(actor, action, target, "succeeded", details, fn)
}
func (c *Catalog) changeOutcome(actor, action, target, outcome string, details map[string]string, fn func() error) error {
	previous := c.state
	activity, logo := c.activity, c.logo
	committed := false
	defer func() {
		if !committed {
			c.state, c.activity, c.logo = previous, activity, logo
		}
	}()
	c.state = cloneCatalog(previous)
	var err error
	if err = c.initOrganization(); err != nil {
		c.state = previous
		return err
	}
	if err = fn(); err != nil {
		c.state = previous
		return err
	}
	c.appendActivity(actor, action, target, outcome, details)
	if err = c.persist(); err != nil {
		c.state = previous
		return err
	}
	committed = true
	return nil
}
func (c *Catalog) appendActivity(actor, action, target, outcome string, details map[string]string) {
	c.state.ActivitySequence++
	c.activity = append(c.activity, Activity{ID: c.state.ActivitySequence, Time: time.Now().UTC(), Actor: actor, Action: action, Target: target, Outcome: outcome, Details: maps.Clone(details)})
	if len(c.activity) > activityLimit {
		c.activity = slices.Clone(c.activity[len(c.activity)-activityLimit:])
	}
}

// RecordActivity records external-boundary intent before touching another store.
// A pending receipt survives a crash; a subsequent receipt records its outcome.
//
// Every data call records two, reads included, so jobs starting together on
// several gateways contend for the document. A receipt decides nothing from
// the catalog, so when another gateway committed first it is recorded over the
// stored document, as Account adopts one. The gateway that just committed keeps
// winning while it has receipts queued, so this retries for as long as one
// database call may hold the lock rather than once. Each conflict is another
// gateway's commit, so the instance always progresses.
func (c *Catalog) RecordActivity(actor, action, target, outcome string, details map[string]string) error {
	c.lock()
	defer c.mu.Unlock()
	deadline := time.Now().Add(catalogTimeout)
	for {
		err := c.changeOutcome(actor, action, target, outcome, details, func() error {
			// change appends the receipt after this callback.
			return nil
		})
		if !errors.Is(err, pg.ErrConflict) || errors.Is(err, errCommitUnknown) || time.Now().After(deadline) {
			return err
		}
		c.refresh()
	}
}

func (c *Catalog) role(user string, p Project) string {
	if c.version < 0 {
		return ""
	}
	if c.admin(user) {
		return "owner"
	}
	if c.state.Users[user].Disabled {
		return ""
	}
	role := p.Members[user]
	if !p.protected() {
		for id, grant := range p.GroupRoles {
			if rank(grant) > rank(role) && slices.Contains(c.state.Groups[id].Members, user) {
				role = grant
			}
		}
	}
	if c.state.Users[user].Kind == "service" && rank(role) > rank("edit") {
		return "edit"
	}
	return role
}
func (c *Catalog) effective(user string, p Project) EffectiveAccess {
	access := EffectiveAccess{Role: c.role(user, p), Direct: p.Members[user], Groups: []string{}}
	for id := range p.GroupRoles {
		if slices.Contains(c.state.Groups[id].Members, user) {
			access.Groups = append(access.Groups, id)
		}
	}
	sort.Strings(access.Groups)
	return access
}
func (c *Catalog) Interactive(user string) bool {
	c.lock()
	defer c.mu.Unlock()
	return c.version >= 0 && (user == Bootstrap || ValidUser(user) && !c.state.Users[user].Disabled) && c.state.Users[user].Kind != "service"
}
func (c *Catalog) IsService(user string) bool {
	c.lock()
	defer c.mu.Unlock()
	return c.state.Users[user].Kind == "service"
}
func (c *Catalog) Organization(actor string) (Organization, error) {
	c.lock()
	defer c.mu.Unlock()
	if !c.admin(actor) {
		return Organization{}, workspace.ErrForbidden
	}
	out := Organization{Name: c.state.InstanceName, Groups: []Group{}, Invitations: []Invitation{}, Activity: []Activity{}}
	for _, g := range c.state.Groups {
		g.Members = slices.Clone(g.Members)
		out.Groups = append(out.Groups, g)
	}
	for _, v := range c.state.Invitations {
		v.TokenHash = ""
		v.Groups = slices.Clone(v.Groups)
		v.Projects = maps.Clone(v.Projects)
		out.Invitations = append(out.Invitations, v)
	}
	for i := len(c.activity) - 1; i >= 0; i-- {
		v := c.activity[i]
		v.Details = maps.Clone(v.Details)
		out.Activity = append(out.Activity, v)
	}
	sort.Slice(out.Groups, func(i, j int) bool { return out.Groups[i].Name < out.Groups[j].Name })
	sort.Slice(out.Invitations, func(i, j int) bool { return out.Invitations[i].Created.After(out.Invitations[j].Created) })
	return out, nil
}
func (c *Catalog) SetInstanceName(actor, name string) error {
	return c.SetInstanceBranding(actor, name, nil)
}

// A nil logo preserves the current image; an empty image restores the default.
// Name, image and audit receipt persist in the same catalog transaction.
func (c *Catalog) SetInstanceBranding(actor, name string, logo *[]byte) error {
	c.lock()
	defer c.mu.Unlock()
	if !c.admin(actor) {
		return workspace.ErrForbidden
	}
	name = strings.TrimSpace(name)
	if name == "" || !utf8.ValidString(name) || utf8.RuneCountInString(name) > 120 || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return fmt.Errorf("%w: provide an instance name of at most 120 characters on one line", ErrLocalInput)
	}
	var normalized []byte
	if logo != nil {
		var err error
		normalized, err = branding.NormalizeLogo(*logo)
		if err != nil {
			return fmt.Errorf("%w: %s", ErrLocalInput, err)
		}
	}
	return c.change(actor, "instance.branding", "instance", nil, func() error {
		c.state.InstanceName = name
		if logo != nil {
			c.logo = normalized
		}
		return nil
	})
}
func (c *Catalog) InstanceName() string {
	c.lock()
	defer c.mu.Unlock()
	if c.state.InstanceName == "" {
		return "ForgetMeNote"
	}
	return c.state.InstanceName
}

func (c *Catalog) InstanceLogo() []byte {
	c.lock()
	defer c.mu.Unlock()
	return slices.Clone(c.logo)
}

// InstanceLogoVersion is the hex SHA-256 the committed custom logo is stored
// under, or "" without one, so callers need not copy the logo to fingerprint it.
func (c *Catalog) InstanceLogoVersion() string {
	c.lock()
	defer c.mu.Unlock()
	if len(c.logo) == 0 {
		return ""
	}
	return c.logoStored
}

// ProvisionWorkNotebook creates an ordinary private notebook without granting
// creation or instance privileges to its owner. An exact repeat is harmless;
// existing shared, source and protected notebooks are never repurposed.
func (c *Catalog) ProvisionWorkNotebook(actor, user, id, name string) error {
	c.lock()
	defer c.mu.Unlock()
	if !c.admin(actor) {
		return workspace.ErrForbidden
	}
	u, ok := c.state.Users[user]
	if !ok || u.Disabled || u.Kind == "service" {
		return errors.New("select an enabled person")
	}
	if p, exists := c.state.Projects[id]; exists {
		if p.Kind != "repository" && !p.protected() && p.Name == strings.TrimSpace(name) && len(p.Members) == 1 && p.Members[user] == "owner" && len(p.GroupRoles) == 0 && !activeTransfer(c.state.Transfers[id]) {
			return nil
		}
		return workspace.ErrConflict
	}
	return c.createNotebook(actor, id, name, "notes", user)
}

func (c *Catalog) SetGroup(actor string, g Group) (Group, error) {
	c.lock()
	defer c.mu.Unlock()
	if !c.admin(actor) {
		return Group{}, workspace.ErrForbidden
	}
	if !projectID.MatchString(g.ID) || !validLabel(g.Name) || len(g.Members) > 4096 {
		return Group{}, errors.New("provide a group ID, name, and at most 4096 members")
	}
	old, exists := c.state.Groups[g.ID]
	if old.Revision != g.Revision {
		return Group{}, workspace.ErrConflict
	}
	if !exists && len(c.state.Groups) >= 256 {
		return Group{}, errors.New("group limit reached")
	}
	for _, id := range g.Members {
		if _, ok := c.state.Users[id]; !ok {
			return Group{}, errors.New("select existing accounts")
		}
	}
	g.Name = strings.TrimSpace(g.Name)
	g.Members = slices.Clone(g.Members)
	sort.Strings(g.Members)
	g.Members = slices.Compact(g.Members)
	// Reusing a deleted group ID must not revive a stale editor's revision.
	g.Revision = c.state.ActivitySequence + 1
	err := c.change(actor, "group.set", g.ID, nil, func() error {
		for _, user := range old.Members {
			if !slices.Contains(g.Members, user) {
				c.revokeGroupWork(g.ID, user)
			}
		}
		c.state.Groups[g.ID] = g
		return nil
	})
	return g, err
}
func (c *Catalog) revokeGroupWork(group, user string) {
	for id, p := range c.state.Projects {
		if p.GroupRoles[group] != "" {
			c.state.ProjectAccessEpoch[accessKey(id, user)]++
		}
	}
}
func (c *Catalog) DeleteGroup(actor, id string, revision uint64) error {
	c.lock()
	defer c.mu.Unlock()
	if !c.admin(actor) {
		return workspace.ErrForbidden
	}
	g, ok := c.state.Groups[id]
	if !ok || g.Revision != revision {
		return workspace.ErrConflict
	}
	return c.change(actor, "group.delete", id, nil, func() error {
		for _, user := range g.Members {
			c.revokeGroupWork(id, user)
		}
		delete(c.state.Groups, id)
		for key, grant := range c.state.ConnectionGrants {
			if _, exists := grant.Groups[id]; exists {
				delete(grant.Groups, id)
				grant.Revision++
				c.state.ConnectionGrants[key] = grant
			}
		}
		for key, p := range c.state.Projects {
			delete(p.GroupRoles, id)
			c.state.Projects[key] = p
		}
		for key, invite := range c.state.Invitations {
			if invite.Status == "pending" && slices.Contains(invite.Groups, id) {
				invite.Status, invite.TokenHash = "revoked", ""
				c.state.Invitations[key] = invite
			}
		}
		return nil
	})
}
func (c *Catalog) SetProjectGroup(actor, project, group, role string) error {
	c.lock()
	defer c.mu.Unlock()
	if !c.owns(actor, project) || c.state.Projects[project].protected() {
		return workspace.ErrForbidden
	}
	g, ok := c.state.Groups[group]
	if !ok || role != "" && (rank(role) < 1 || rank(role) > 3) {
		return errors.New("select an existing group and Reader, Commenter or Editor access")
	}
	return c.change(actor, "notebook.group", project, map[string]string{"group": group, "role": role}, func() error {
		p := c.state.Projects[project]
		if rank(role) < rank(p.GroupRoles[group]) {
			for _, user := range g.Members {
				c.state.ProjectAccessEpoch[accessKey(project, user)]++
			}
		}
		if p.GroupRoles == nil {
			p.GroupRoles = map[string]string{}
		}
		if role == "" {
			delete(p.GroupRoles, group)
		} else {
			p.GroupRoles[group] = role
		}
		c.state.Projects[project] = p
		return nil
	})
}
func (c *Catalog) ProjectGroups(actor, project string) ([]Group, error) {
	c.lock()
	defer c.mu.Unlock()
	if !c.owns(actor, project) || c.state.Projects[project].protected() {
		return nil, workspace.ErrForbidden
	}
	out := []Group{}
	for _, g := range c.state.Groups {
		g.Members = nil
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
