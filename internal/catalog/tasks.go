package catalog

import (
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"forgetmenote/internal/messaging"
	"forgetmenote/internal/workspace"
)

// TaskQueuesProject is the system notebook that holds every person's open
// tasks, one note per person. It has no members: admins read it, and only task
// actions, which check their own permissions, write it.
const TaskQueuesProject = "task-queues"

const maxTaskPeople = 100000

var ErrNoTaskPerson = errors.New("this person has no task queue yet")

var taskPersonID = regexp.MustCompile(`^p-[a-z2-7]{10}$`)

// TaskQueues configures task queues. Client names the messaging client whose
// workspace members have queues; Group, when set, limits the accounts that may
// use them. Workspace is reported from the client and never stored.
type TaskQueues struct {
	Project   string `json:"project"`
	Client    string `json:"client,omitempty"`
	Workspace string `json:"workspace,omitempty"`
	Group     string `json:"group,omitempty"`
	// AwayStatuses are words or phrases in a member's custom status, such as
	// "ooo", that mean they are away; the reminder skips them that day. Empty
	// uses the built-in list (vacationing, out sick, out of office and others).
	AwayStatuses []string `json:"awayStatuses,omitempty"`
	// HolidayTitles are words or phrases that begin the title of an all-day
	// event on any of the reminder's holiday calendars, such as "holiday" or
	// "thanksgiving", that make its dates days off for everyone. Empty means
	// "holiday".
	HolidayTitles []string `json:"holidayTitles,omitempty"`
	Disabled      bool     `json:"disabled,omitempty"`
	Revision      uint64   `json:"revision"`
}

// TaskPerson owns one queue. It is a messaging member, whose account is the one
// linked to them, or an account that had no linked member when its queue
// began. Linking that account to a member without a queue moves the queue to
// the member, so a person never needs a second one.
type TaskPerson struct {
	ID        string    `json:"id"`
	Account   string    `json:"account,omitempty"`
	Client    string    `json:"client,omitempty"`
	Workspace string    `json:"workspace,omitempty"`
	User      string    `json:"user,omitempty"`
	Created   time.Time `json:"created"`
}

func ValidTaskPerson(id string) bool { return taskPersonID.MatchString(id) }

// SetTaskQueues enables or changes task queues, creating their notebook the
// first time. Administrator only.
func (c *Catalog) SetTaskQueues(actor string, in TaskQueues) (TaskQueues, error) {
	c.lock()
	defer c.mu.Unlock()
	if !c.admin(actor) {
		return TaskQueues{}, workspace.ErrForbidden
	}
	var revision uint64
	if c.state.TaskQueues != nil {
		revision = c.state.TaskQueues.Revision
	}
	if in.Revision != revision {
		return TaskQueues{}, workspace.ErrConflict
	}
	if in.Client != "" {
		if _, ok := c.chatClient(in.Client); !ok {
			return TaskQueues{}, fmt.Errorf("%w: no messaging client %q is configured", ErrLocalInput, in.Client)
		}
	}
	away, err := taskPhrases("away statuses", in.AwayStatuses)
	if err != nil {
		return TaskQueues{}, err
	}
	holidays, err := taskPhrases("holiday titles", in.HolidayTitles)
	if err != nil {
		return TaskQueues{}, err
	}
	in.AwayStatuses, in.HolidayTitles = away, holidays
	if _, ok := c.state.Groups[in.Group]; in.Group != "" && !ok {
		return TaskQueues{}, fmt.Errorf("%w: no group %q exists", ErrLocalInput, in.Group)
	}
	p, exists := c.state.Projects[TaskQueuesProject]
	if exists && !p.TaskQueues {
		return TaskQueues{}, fmt.Errorf("%w: a notebook with the ID %s already exists; rename it first", workspace.ErrConflict, TaskQueuesProject)
	}
	var store *workspace.Store
	var created bool
	if !exists {
		var err error
		if store, created, err = c.createStore(TaskQueuesProject); err != nil {
			return TaskQueues{}, err
		}
	}
	in.Project, in.Workspace, in.Revision = TaskQueuesProject, "", revision+1
	details := map[string]string{"client": in.Client, "group": in.Group, "disabled": fmt.Sprint(in.Disabled)}
	err = c.change(actor, "tasks.settings", TaskQueuesProject, details, func() error {
		settings := in
		c.state.TaskQueues = &settings
		if !exists {
			c.state.Projects[TaskQueuesProject] = Project{ID: TaskQueuesProject, Name: "Task queues", Kind: "notes", TaskQueues: true}
		}
		return nil
	})
	if err != nil {
		if store != nil {
			err = c.abandonStore(TaskQueuesProject, store, created, err)
		}
		return TaskQueues{}, err
	}
	if store != nil {
		c.stores[TaskQueuesProject] = store
	}
	return c.taskQueues(), nil
}

// TaskQueues returns the current settings and the notebook store when task
// queues are enabled. Callers check task permissions themselves.
func (c *Catalog) TaskQueues() (TaskQueues, *workspace.Store, bool) {
	c.lock()
	defer c.mu.Unlock()
	settings := c.taskQueues()
	store := c.stores[TaskQueuesProject]
	if c.version < 0 || c.state.TaskQueues == nil || settings.Disabled || store == nil {
		return settings, nil, false
	}
	return settings, store, true
}

func (c *Catalog) taskQueues() TaskQueues {
	if c.state.TaskQueues == nil {
		return TaskQueues{}
	}
	settings := *c.state.TaskQueues
	if client, ok := c.chatClient(settings.Client); ok {
		settings.Workspace = client.Workspace
	}
	return settings
}

func (c *Catalog) chatClient(id string) (messaging.Client, bool) {
	if c.state.ChatClients == nil || id == "" {
		return messaging.Client{}, false
	}
	client, ok := c.state.ChatClients.Clients[id]
	return client, ok
}

func (c *Catalog) tasksEnabled() bool {
	return c.version >= 0 && c.state.TaskQueues != nil && !c.state.TaskQueues.Disabled && c.stores[TaskQueuesProject] != nil
}

// taskAccount reports whether an account may have a queue and use tasks.
func (c *Catalog) taskAccount(user string) bool {
	u, ok := c.state.Users[user]
	if !ok || u.Disabled || u.Kind == "service" || !ValidUser(user) {
		return false
	}
	group := c.state.TaskQueues.Group
	return group == "" || slices.Contains(c.state.Groups[group].Members, user)
}

// memberAccount is the account a member's queue belongs to: the client's own
// mapping, or the member's link.
func (c *Catalog) memberAccount(client, workspaceID, user string) string {
	if config, ok := c.chatClient(client); ok && config.Workspace == workspaceID && config.Users[user] != "" {
		return config.Users[user]
	}
	for _, l := range c.state.MessageLinks {
		if l.Client == client && l.Workspace == workspaceID && l.User == user {
			return l.Account
		}
	}
	return ""
}

// accountMember is the task client's member an account is mapped or linked to.
func (c *Catalog) accountMember(account string) string {
	config, ok := c.chatClient(c.state.TaskQueues.Client)
	if !ok {
		return ""
	}
	members := []string{}
	for user, mapped := range config.Users {
		if mapped == account {
			members = append(members, user)
		}
	}
	for _, l := range c.state.MessageLinks {
		if l.Client == c.state.TaskQueues.Client && l.Workspace == config.Workspace && l.Account == account && config.Users[l.User] == "" {
			members = append(members, l.User)
		}
	}
	slices.Sort(members)
	if len(members) == 0 {
		return ""
	}
	return members[0]
}

func (c *Catalog) taskPersonIndex(match func(TaskPerson) bool) int {
	return slices.IndexFunc(c.state.TaskPeople, match)
}

func (c *Catalog) newTaskPerson(p TaskPerson) (TaskPerson, error) {
	if len(c.state.TaskPeople) >= maxTaskPeople {
		return TaskPerson{}, errors.New("too many task queues")
	}
	for {
		raw := make([]byte, 7)
		if _, err := rand.Read(raw); err != nil {
			return TaskPerson{}, err
		}
		p.ID = "p-" + strings.ToLower(base32.StdEncoding.EncodeToString(raw))[:10]
		if c.taskPersonIndex(func(q TaskPerson) bool { return q.ID == p.ID }) < 0 {
			break
		}
	}
	p.Created = time.Now().UTC()
	return p, c.change(Bootstrap, "tasks.person", p.ID, map[string]string{"client": p.Client, "workspace": p.Workspace, "user": p.User, "account": p.Account}, func() error {
		c.state.TaskPeople = append(c.state.TaskPeople, p)
		return nil
	})
}

// TaskPersonForMember returns the queue owner for a member of the task
// client's workspace. With create, a member without a queue gets one: the
// queue of their linked account when it has no member yet, or a new one.
// Callers verify that the member is an active, human, non-guest member.
func (c *Catalog) TaskPersonForMember(client, workspaceID, user string, create bool) (TaskPerson, error) {
	c.lock()
	defer c.mu.Unlock()
	return c.taskPersonForMember(client, workspaceID, user, create)
}

func (c *Catalog) taskPersonForMember(client, workspaceID, user string, create bool) (TaskPerson, error) {
	if !c.tasksEnabled() {
		return TaskPerson{}, errors.New("task queues are not enabled")
	}
	settings := c.taskQueues()
	if client != settings.Client || workspaceID != settings.Workspace || !validMessageLink(client, workspaceID, user) {
		return TaskPerson{}, workspace.ErrForbidden
	}
	if i := c.taskPersonIndex(func(p TaskPerson) bool {
		return p.Client == client && p.Workspace == workspaceID && p.User == user
	}); i >= 0 {
		return c.state.TaskPeople[i], nil
	}
	account := c.memberAccount(client, workspaceID, user)
	i := -1
	if account != "" && c.taskAccount(account) {
		i = c.taskPersonIndex(func(p TaskPerson) bool { return p.Account == account })
	}
	switch {
	case i >= 0 && !create:
		return c.state.TaskPeople[i], nil
	case i >= 0:
		moved := c.state.TaskPeople[i]
		moved.Account, moved.Client, moved.Workspace, moved.User = "", client, workspaceID, user
		return moved, c.change(Bootstrap, "tasks.person.member", moved.ID, map[string]string{"client": client, "workspace": workspaceID, "user": user}, func() error {
			c.state.TaskPeople[i] = moved
			return nil
		})
	case !create:
		return TaskPerson{}, ErrNoTaskPerson
	}
	return c.newTaskPerson(TaskPerson{Client: client, Workspace: workspaceID, User: user})
}

// TaskPersonForAccount returns the queue owner for an account: its linked
// member's queue, or else its own. With create, it makes the queue it lacks.
func (c *Catalog) TaskPersonForAccount(user string, create bool) (TaskPerson, error) {
	c.lock()
	defer c.mu.Unlock()
	if !c.tasksEnabled() {
		return TaskPerson{}, errors.New("task queues are not enabled")
	}
	if !c.taskAccount(user) {
		return TaskPerson{}, workspace.ErrForbidden
	}
	if member := c.accountMember(user); member != "" {
		settings := c.taskQueues()
		return c.taskPersonForMember(settings.Client, settings.Workspace, member, create)
	}
	if i := c.taskPersonIndex(func(p TaskPerson) bool { return p.Account == user }); i >= 0 {
		return c.state.TaskPeople[i], nil
	}
	if !create {
		return TaskPerson{}, ErrNoTaskPerson
	}
	return c.newTaskPerson(TaskPerson{Account: user})
}

// TaskPerson returns a queue owner by ID.
func (c *Catalog) TaskPerson(id string) (TaskPerson, bool) {
	c.lock()
	defer c.mu.Unlock()
	i := c.taskPersonIndex(func(p TaskPerson) bool { return p.ID == id })
	if i < 0 {
		return TaskPerson{}, false
	}
	return c.state.TaskPeople[i], true
}

// TaskPersonMember returns the member of the task client's workspace who hears
// about a queue: its own member, or the member its account is mapped or linked
// to. It is empty when nobody can be messaged.
func (c *Catalog) TaskPersonMember(id string) string {
	c.lock()
	defer c.mu.Unlock()
	i := c.taskPersonIndex(func(p TaskPerson) bool { return p.ID == id })
	if i < 0 || c.state.TaskQueues == nil {
		return ""
	}
	p := c.state.TaskPeople[i]
	if p.User != "" {
		if config, ok := c.chatClient(c.state.TaskQueues.Client); !ok || p.Client != c.state.TaskQueues.Client || p.Workspace != config.Workspace {
			return ""
		}
		return p.User
	}
	return c.accountMember(p.Account)
}

// TaskPersonAccount returns the account that currently owns a queue, if it may
// use tasks: the queue's own account, or its member's mapped or linked account.
func (c *Catalog) TaskPersonAccount(id string) (string, error) {
	c.lock()
	defer c.mu.Unlock()
	i := c.taskPersonIndex(func(p TaskPerson) bool { return p.ID == id })
	if i < 0 {
		return "", ErrNoTaskPerson
	}
	p := c.state.TaskPeople[i]
	account := p.Account
	if p.User != "" {
		account = c.memberAccount(p.Client, p.Workspace, p.User)
	}
	if account == "" || !c.tasksEnabled() || !c.taskAccount(account) {
		return "", nil
	}
	return account, nil
}

// taskPhrases normalizes up to 20 words or phrases of 1 to 40 characters,
// lowercased, sorted and without repeats.
func taskPhrases(name string, in []string) ([]string, error) {
	if len(in) > 20 {
		return nil, fmt.Errorf("%w: name at most 20 %s", ErrLocalInput, name)
	}
	out := []string{}
	for _, phrase := range in {
		phrase = strings.ToLower(strings.TrimSpace(phrase))
		if phrase == "" || len([]rune(phrase)) > 40 || strings.ContainsAny(phrase, "\n\r") {
			return nil, fmt.Errorf("%w: each of the %s is one line of 1 to 40 characters", ErrLocalInput, name)
		}
		out = append(out, phrase)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return slices.Compact(slices.Sorted(slices.Values(out))), nil
}
