// Package catalog owns the instance's notebooks and permissions outside notebook content and history.
package catalog

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"forgetmenote/internal/notestore"
	"forgetmenote/internal/pg"
	"forgetmenote/internal/workspace"
)

const Bootstrap = "@local-admin"

type User struct {
	Kind              string `json:"kind,omitempty"`
	Steward           string `json:"steward,omitempty"`
	ID                string `json:"id"`
	DisplayName       string `json:"displayName,omitempty"`
	CanCreateProjects bool   `json:"canCreateProjects,omitempty"`
	Admin             bool   `json:"admin"`
	Disabled          bool   `json:"disabled"`
}
type Project struct {
	GroupRoles          map[string]string          `json:"groupRoles,omitempty"`
	EffectiveMembers    map[string]EffectiveAccess `json:"effectiveMembers,omitempty"`
	Authority           *NotebookAuthority         `json:"authority,omitempty"`
	AuthorityGeneration uint64                     `json:"authorityGeneration,omitempty"`
	Kind                string                     `json:"kind,omitempty"`
	Capabilities        *NotebookCapabilities      `json:"capabilities,omitempty"`
	ProtectedInbox      string                     `json:"protectedInbox,omitempty"`
	AssistantOwner      string                     `json:"assistantOwner,omitempty"`
	ID                  string                     `json:"id"`
	Name                string                     `json:"name"`
	Role                string                     `json:"role,omitempty"`
	Members             map[string]string          `json:"members,omitempty"`
	OpenAsWiki          bool                       `json:"openAsWiki,omitempty"`
	// TaskQueues marks the system notebook that only task actions write.
	TaskQueues bool `json:"taskQueues,omitempty"`
}
type catalogData struct {
	AssistantPolicy    *AssistantPolicy             `json:"assistantPolicy,omitempty"`
	Assistants         map[string]AssistantSettings `json:"assistants,omitempty"`
	ChatClients        *ChatClients                 `json:"chatClients,omitempty"`
	AI                 *AISettings                  `json:"ai,omitempty"`
	ConnectionGrants   map[string]ConnectionGrant   `json:"connectionGrants,omitempty"`
	Groups             map[string]Group             `json:"groups,omitempty"`
	Invitations        map[string]Invitation        `json:"invitations,omitempty"`
	Activity           []Activity                   `json:"activity,omitempty"`
	ActivitySequence   uint64                       `json:"activitySequence,omitempty"`
	InstanceName       string                       `json:"instanceName,omitempty"`
	InstanceLogoHash   string                       `json:"instanceLogoHash,omitempty"`
	InstanceLogo       []byte                       `json:"instanceLogo,omitempty"`
	Transfers          map[string]AuthorityTransfer `json:"authorityTransfers,omitempty"`
	ProjectAccessEpoch map[string]uint64            `json:"projectAccessEpoch,omitempty"`
	Passwords          map[string]LocalCredential   `json:"passwords,omitempty"`
	OIDC               []ExternalIdentity           `json:"oidc,omitempty"`
	MessageLinks       []MessageLink                `json:"messageLinks,omitempty"`
	MessageDeclines    []MessageLinkDecline         `json:"messageLinkDeclines,omitempty"`
	TaskQueues         *TaskQueues                  `json:"taskQueues,omitempty"`
	TaskPeople         []TaskPerson                 `json:"taskPeople,omitempty"`
	AccessEpoch        map[string]uint64            `json:"accessEpoch,omitempty"`
	Accounts           map[string]Account           `json:"accounts,omitempty"`
	Users              map[string]User              `json:"users"`
	Projects           map[string]Project           `json:"projects"`
}
type Catalog struct {
	activity       []Activity
	logo           []byte
	activityStored uint64
	logoStored     string
	db             *pg.DB
	version        int64 // stored version state reflects; -1 grants nothing
	checked        time.Time
	freshness      time.Duration
	commitFault    func() error // tests: a lost commit acknowledgement
	mu             sync.Mutex
	data           string
	state          catalogData
	notes          *notestore.Store // holds every notebook's content
	own            *ownNotes        // the notes store Open made, or nil
	stores         map[string]*workspace.Store
	importing      map[string]bool // IDs Import reserved for its restore
	browseExcluded []string
	dirLock        *os.File // this process owns the data directory
}

var projectID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
var userID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.@-]{0,127}$`)

func ValidUser(id string) bool { return userID.MatchString(id) }

// Open opens the catalog with a notes store of its own over db, keeping
// content blocks in the blob directory data and exporting notebooks into
// data/exports for backups, as an instance does by default. A process that
// serves the instance opens its notes store itself and uses OpenWithNotes.
func Open(data string, browseExcluded []string, db *pg.DB) (*Catalog, error) {
	return open(data, browseExcluded, db, nil)
}

// OpenWithNotes opens the catalog over notes, the instance's notes store,
// which the caller closes after the catalog.
func OpenWithNotes(data string, browseExcluded []string, db *pg.DB, notes *notestore.Store) (*Catalog, error) {
	if notes == nil {
		return nil, errors.New("the catalog requires the instance's notes store")
	}
	return open(data, browseExcluded, db, notes)
}

func open(data string, browseExcluded []string, db *pg.DB, notes *notestore.Store) (*Catalog, error) {
	if db == nil {
		return nil, errors.New("the catalog requires the instance database")
	}
	if err := os.MkdirAll(data, 0700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(data, "catalog.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, errors.New("another ForgetMeNote process owns this data directory")
	}
	c := &Catalog{data: data, browseExcluded: browseExcluded, dirLock: lock, stores: map[string]*workspace.Store{}, db: db, version: -1, freshness: catalogFreshness, notes: notes}
	if notes == nil {
		if c.own, err = openOwnNotes(data, db); err != nil {
			c.Close()
			return nil, err
		}
		c.notes = c.own.notes
	}
	ctx, cancel := context.WithTimeout(context.Background(), catalogTimeout)
	defer cancel()
	if err = c.load(ctx); err != nil {
		c.Close()
		return nil, fmt.Errorf("cannot read project catalog: %w", err)
	}
	c.checked = time.Now()
	if len(c.state.Projects) == 0 {
		if err = c.Create(Bootstrap, "default", "My notes"); err != nil {
			c.Close()
			return nil, err
		}
	}
	return c, nil
}

// validate checks a loaded document and opens the stores of its notebooks.
func (c *Catalog) validate() error {
	if ai := c.state.AI; ai != nil && (ai.Revision == 0 || ai.validate() != nil) {
		return errors.New("invalid instance AI policy")
	}
	if clients := c.state.ChatClients; clients != nil && (clients.Revision == 0 || clients.validate() != nil) {
		return errors.New("invalid chat client policy")
	}
	if err := c.initOrganization(); err != nil {
		return err
	}
	for _, a := range c.state.Accounts {
		if len(a.ID) != 32 {
			return errors.New("invalid account identity")
		}
	}
	if err := c.validateInboxes(); err != nil {
		return err
	}
	if err := c.validateAssistants(); err != nil {
		return err
	}
	if err := c.validateAuthorityTransfers(); err != nil {
		return err
	}
	for id, p := range c.state.Projects {
		if !projectID.MatchString(id) || p.ID != id || p.Kind != "" && p.Kind != "notes" && p.Kind != "repository" {
			return errors.New("invalid project catalog")
		}
		store := c.stores[id]
		if store == nil {
			var err error
			if store, err = c.openStore(id); err != nil {
				return err
			}
			c.stores[id] = store
		}
		store.SetReadOnly(p.Kind == "repository")
		store.SetAuthorityGeneration(max(1, p.AuthorityGeneration))
	}
	return nil
}
func (c *Catalog) Close() {
	for _, s := range c.stores {
		s.Close()
	}
	if c.own != nil {
		c.own.close()
	}
	if c.dirLock != nil {
		syscall.Flock(int(c.dirLock.Fd()), syscall.LOCK_UN)
		c.dirLock.Close()
	}
}

type catalogEnvelope struct {
	Version  int         `json:"version"`
	Users    any         `json:"users"`
	Projects any         `json:"projects"`
	Data     catalogData `json:"data"`
}

func (c *Catalog) admin(id string) bool {
	return c.version >= 0 && (id == Bootstrap || c.state.Users[id].Admin && !c.state.Users[id].Disabled && c.state.Users[id].Kind != "service")
}
func (c *Catalog) IsAdmin(id string) bool { c.lock(); defer c.mu.Unlock(); return c.admin(id) }
func (c *Catalog) owns(actor, project string) bool {
	p, ok := c.state.Projects[project]
	return ok && c.role(actor, p) == "owner"
}
func (c *Catalog) Enabled(id string) bool {
	c.lock()
	defer c.mu.Unlock()
	return c.version >= 0 && (id == Bootstrap || ValidUser(id) && !c.state.Users[id].Disabled)
}
func rank(role string) int {
	switch role {
	case "read":
		return 1
	case "comment":
		return 2
	case "edit":
		return 3
	case "owner":
		return 4
	}
	return 0
}
func (c *Catalog) Access(user, id, required string) (*workspace.Store, error) {
	c.lock()
	defer c.mu.Unlock()
	p, ok := c.state.Projects[id]
	if !ok || rank(required) == 0 || rank(c.role(user, p)) < rank(required) {
		return nil, workspace.ErrForbidden
	}
	if transfer := c.state.Transfers[id]; transfer.Phase == "publishing" || transfer.Phase == "preparing" && required != "read" {
		return nil, workspace.ErrForbidden
	}
	if p.Kind == "repository" && required == "edit" || p.TaskQueues && required != "read" {
		return nil, workspace.ErrReadOnly
	}
	return c.stores[id], nil
}
func (c *Catalog) List(user string) []Project {
	c.lock()
	defer c.mu.Unlock()
	out := []Project{}
	for _, p := range c.state.Projects {
		role := c.role(user, p)
		if rank(role) == 0 {
			continue
		}
		p.Role = role
		p.Members = nil
		p.GroupRoles = nil
		p.EffectiveMembers = nil
		p.Capabilities = capabilities(p.Kind, role)
		if activeTransfer(c.state.Transfers[p.ID]) || p.TaskQueues {
			p.Capabilities.Edit = false
			p.Capabilities.Refresh = false
			p.Capabilities.Backup = false
		}
		p.Authority = notebookAuthority(p)
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
func (c *Catalog) Create(actor, id, name string) error {
	return c.CreateKind(actor, id, name, "")
}
func (c *Catalog) CreateKind(actor, id, name, kind string) error {
	c.lock()
	defer c.mu.Unlock()
	return c.createNotebook(actor, id, name, kind, actor)
}

func (c *Catalog) createNotebook(actor, id, name, kind, owner string) error {
	if err := c.checkCreate(actor, id, name); err != nil {
		return err
	}
	if kind != "" && kind != "notes" && kind != "repository" {
		return errors.New("notebook kind must be notes or repository")
	}
	release, err := c.reserveNotebook(id)
	if err != nil {
		return err
	}
	defer release()
	store, created, err := c.createStore(id)
	if err != nil {
		return err
	}
	c.state.Projects[id] = Project{ID: id, Kind: kind, Name: strings.TrimSpace(name), Members: map[string]string{owner: "owner"}}
	if err = c.persist(); err != nil {
		delete(c.state.Projects, id)
		return c.abandonStore(id, store, created, err)
	}
	c.stores[id] = store
	store.SetReadOnly(kind == "repository")
	return nil
}
func (c *Catalog) Member(actor, project, user, role string) error {
	c.lock()
	defer c.mu.Unlock()
	if !c.owns(actor, project) {
		return workspace.ErrForbidden
	}
	if (user != Bootstrap && !ValidUser(user)) || role != "" && rank(role) == 0 {
		return errors.New("invalid username or role")
	}
	if c.state.Users[user].Kind == "service" && role == "owner" {
		return workspace.ErrForbidden
	}
	p, ok := c.state.Projects[project]
	if !ok {
		return workspace.ErrForbidden
	}
	if p.protected() && (len(p.Members) != 1 || p.Members[user] != "owner" || role != "owner") {
		return workspace.ErrForbidden
	}
	old := p.Members[user]
	if old == "owner" && role != "owner" {
		another := false
		for id, assigned := range p.Members {
			if id != user && assigned == "owner" && !c.state.Users[id].Disabled && c.state.Users[id].Kind != "service" {
				another = true
				break
			}
		}
		if !another {
			return ErrLastOwner
		}
	}
	return c.change(actor, "notebook.member", project, map[string]string{"user": user, "before": old, "role": role}, func() error {
		p := c.state.Projects[project]
		if p.Members == nil {
			p.Members = map[string]string{}
		}
		if role == "" {
			delete(p.Members, user)
		} else {
			p.Members[user] = role
		}
		c.state.Projects[project] = p
		if rank(role) < rank(old) {
			c.state.ProjectAccessEpoch[accessKey(project, user)]++
		}
		if _, known := c.state.Users[user]; !known && user != Bootstrap {
			c.state.Users[user] = User{ID: user}
		}
		return nil
	})
}

func (c *Catalog) SetUser(actor string, user User) error {
	c.lock()
	defer c.mu.Unlock()
	return c.setUser(actor, user)
}
func (c *Catalog) setUser(actor string, user User) error {
	if !c.admin(actor) {
		return workspace.ErrForbidden
	}
	if !ValidUser(user.ID) {
		return errors.New("invalid username")
	}
	if len(user.DisplayName) > 120 || strings.ContainsAny(user.DisplayName, "\r\n\x00") {
		return errors.New("use a display name of at most 120 characters on one line")
	}
	user.DisplayName = strings.TrimSpace(user.DisplayName)
	old := c.state.Users[user.ID]
	user.Kind, user.Steward = old.Kind, old.Steward
	if user.Kind == "service" && (user.Admin || user.CanCreateProjects) {
		return workspace.ErrForbidden
	}
	if actor != Bootstrap && old.Admin && !old.Disabled && (!user.Admin || user.Disabled) {
		another := false
		for id, u := range c.state.Users {
			if id != user.ID && u.Admin && !u.Disabled && u.Kind != "service" {
				another = true
				break
			}
		}
		if !another {
			return ErrLastAdmin
		}
	}
	return c.change(actor, "account.update", user.ID, map[string]string{"admin": fmt.Sprint(user.Admin), "disabled": fmt.Sprint(user.Disabled)}, func() error {
		c.state.Users[user.ID] = user
		if user.Disabled || old.Admin && !user.Admin {
			c.state.AccessEpoch[user.ID]++
		}
		return nil
	})
}

func (c *Catalog) Administration(actor string) ([]User, []Project, error) {
	c.lock()
	defer c.mu.Unlock()
	users := []User{}
	projects := []Project{}
	if c.admin(actor) {
		for _, u := range c.state.Users {
			users = append(users, u)
		}
	}
	for _, p := range c.state.Projects {
		if !c.owns(actor, p.ID) {
			continue
		}
		copy := p
		copy.GroupRoles = maps.Clone(p.GroupRoles)
		copy.EffectiveMembers = map[string]EffectiveAccess{}
		for id := range c.state.Users {
			access := c.effective(id, p)
			if access.Direct != "" || len(access.Groups) > 0 {
				copy.EffectiveMembers[id] = access
			}
		}
		copy.Members = map[string]string{}
		for k, v := range p.Members {
			copy.Members[k] = v
		}
		projects = append(projects, copy)
	}
	if !c.canCreateProjects(actor) && len(projects) == 0 {
		return nil, nil, workspace.ErrForbidden
	}
	if !c.admin(actor) {
		seen := map[string]bool{}
		for _, p := range projects {
			for id := range p.EffectiveMembers {
				if id != Bootstrap && !seen[id] {
					users = append(users, c.state.Users[id])
					seen[id] = true
				}
			}
		}
	}
	sort.Slice(users, func(i, j int) bool { return users[i].ID < users[j].ID })
	sort.Slice(projects, func(i, j int) bool { return projects[i].ID < projects[j].ID })
	return users, projects, nil
}
