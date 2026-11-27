// Package taskqueue owns task queues: the task actions, the Slack task
// commands, forms and buttons, task notices and the weekday reminder.
//
// One system notebook holds each person's open tasks in
// people/<person ID>.md, in the format internal/tasks owns. Only these actions
// write it. Each change is one commit whose subject names the task, what
// happened and who did it, so Git history is the record and closed tasks
// simply leave the note. Anyone who may use tasks can add one to any queue; a
// queue's owner sees and controls it, and a requester sees, edits and
// withdraws only the tasks they filed. Every surface (MCP, the API, the CLI and
// Slack) goes through these actions, and internal/tasks decides each change.
package taskqueue

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"forgetmenote/internal/attribution"
	"forgetmenote/internal/auth"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/messaging"
	"forgetmenote/internal/tasks"
	"forgetmenote/internal/webcore"
	"forgetmenote/internal/workspace"
)

const taskWriteAttempts = 8

// PeopleLimit is the most people and audiences one request names. A
// request giving more than taskConfirmAbove people a copy waits for its
// requester to confirm the number, and none gives more than taskAudienceLimit.
// taskQueueLimit is the most open tasks one queue holds.
const (
	PeopleLimit       = 20
	taskConfirmAbove  = 20
	taskAudienceLimit = 200
	taskQueueLimit    = 1000
)

var taskMention = regexp.MustCompile(`^<@([A-Z0-9]+)(?:\|[^>]*)?>$`)

// taskAudience is a channel, a user group or the whole workspace named in a
// task request, as Slack writes their mentions, or "everyone".
var taskAudience = regexp.MustCompile(`^(?:<#([CG][A-Z0-9]+)(?:\|[^>]*)?>|<!subteam\^([A-Z0-9]+)(?:\|([^>]*))?>|<!everyone>|(?i:everyone))$`)

// taskConfirmation is a request that gives more than taskConfirmAbove people
// a copy, made without confirm. Count is how many copies it gives and
// Audiences what it named that grew it.
type taskConfirmation struct {
	Count     int
	Audiences []string
}

func (c taskConfirmation) Error() string { return c.Unwrap().Error() }

func (c taskConfirmation) Unwrap() error {
	return webcore.Error{Status: 409, Code: "confirmation_required", Message: c.question() + " Ask the user, then repeat the request with confirm true"}
}

// question asks a requester to confirm the number of copies.
func (c taskConfirmation) question() string {
	about := ""
	if len(c.Audiences) > 0 {
		about = " (" + taskNameList(c.Audiences) + ")"
	}
	return "That gives " + strconv.Itoa(c.Count) + " people their own copy" + about + ", and each gets a direct message."
}

type PersonView struct {
	ID        string `json:"id"`
	Name      string `json:"name,omitempty"`
	SlackUser string `json:"slackUser,omitempty"`
	You       bool   `json:"you,omitempty"`
}

type View struct {
	tasks.Task
	Owner     PersonView `json:"owner"`
	Requester PersonView `json:"requester"`
	NextDates []string   `json:"nextDates,omitempty"`
}

type List struct {
	Person string `json:"person,omitempty"`
	Mine   []View `json:"mine"`
	Filed  []View `json:"filed"`
}

type Input struct {
	Person   string   `json:"person,omitempty" jsonschema:"Whose queue: me (the default), a Slack mention or member ID in the task workspace, an account user ID, or a person ID from list_tasks"`
	People   []string `json:"people,omitempty" jsonschema:"Instead of person, give each of up to 20 people their own copy, which they close separately. Same forms as person, or a channel as <#C123>, a user group as <!subteam^S123>, or everyone: each active member in it but you gets a copy, and you must be in the channel"`
	Text     string   `json:"text" jsonschema:"The task, one line of at most 500 characters"`
	Due      string   `json:"due,omitempty" jsonschema:"Due date such as 2026-11-21; required with every"`
	Priority string   `json:"priority,omitempty" jsonschema:"high, normal or low; only for your own queue"`
	Every    string   `json:"every,omitempty" jsonschema:"Repeat on a fixed schedule counted from the due date, such as quarterly or 2 weeks (1 to 24 days, weeks, months or years)"`
	Lead     int      `json:"lead,omitempty" jsonschema:"For a recurring task, how many working days before each due date it joins the owner's weekday reminder (1 to 60); by default 1 for daily and weekly schedules and 5 for longer ones"`
	Source   string   `json:"source,omitempty" jsonschema:"HTTPS link to where the task came from, such as a Slack message link"`
	Confirm  bool     `json:"confirm,omitempty" jsonschema:"Set only after the user agreed to the number of copies, when a request gives more than 20 people a copy (at most 200)"`
}

// Added is the task added, and with people, every copy in the order named.
type Added struct {
	View
	Tasks []View `json:"tasks,omitempty" jsonschema:"With people, every copy in the order named, starting with the one above"`
}

type Update struct {
	ID          string  `json:"id" jsonschema:"Task ID from list_tasks"`
	Revision    string  `json:"revision" jsonschema:"The task's revision from list_tasks; a task changed since then is not overwritten"`
	Text        *string `json:"text,omitempty" jsonschema:"New text; owner or requester"`
	Due         *string `json:"due,omitempty" jsonschema:"New due date, or empty to clear it; owner or requester"`
	Priority    *string `json:"priority,omitempty" jsonschema:"high, normal or low; owner only"`
	SnoozeUntil *string `json:"snoozeUntil,omitempty" jsonschema:"Hide the task until this date, or empty to wake it; owner only"`
	Every       *string `json:"every,omitempty" jsonschema:"New schedule counted from the due date, or empty to stop repeating; owner, or the requester of a series they added"`
	Lead        *int    `json:"lead,omitempty" jsonschema:"Working days before each due date a recurring task joins the weekday reminder (1 to 60), or 0 for its schedule's default; owner or requester"`
	Outcome     string  `json:"outcome,omitempty" jsonschema:"Close the task: done or not_applicable (owner; reason required for not_applicable), or withdrawn (requester). A recurring task records the occurrence and moves to its next date"`
	Reason      string  `json:"reason,omitempty" jsonschema:"Why, up to 200 characters; required for not_applicable and stopping a schedule"`
	ReassignTo  string  `json:"reassignTo,omitempty" jsonschema:"Move the task to another person's queue; owner only. Same forms as add_task's person"`
}

type Result struct {
	Task       *View          `json:"task,omitempty"`
	Closed     string         `json:"closed,omitempty"`
	Occurrence *tasks.Closing `json:"occurrence,omitempty"`
	Revision   string         `json:"revision"`
}

type HistoryEntry struct {
	Revision string `json:"revision"`
	At       string `json:"at"`
	Change   string `json:"change"`
}

// Caller is who is acting: their queue's person, and the account acting
// for them when there is one.
type Caller struct {
	Person  catalog.TaskPerson
	Account string
}

func (s *Service) taskQueues() (catalog.TaskQueues, *workspace.Store, error) {
	settings, store, ok := s.catalog.TaskQueues()
	if !ok {
		return settings, nil, webcore.Error{Status: 404, Code: "not_found", Message: "Task queues are not enabled on this instance"}
	}
	return settings, store, nil
}

// accountTaskCaller is the signed-in account. Task queues are personal, so a
// credential limited to some notebooks and automations cannot use them.
func (s *Service) accountTaskCaller(ctx context.Context, create bool) (Caller, error) {
	if !s.hooks.Personal(ctx) {
		return Caller{}, webcore.Error{Status: 403, Code: "forbidden", Message: "Tasks need a connection to all of your notebooks"}
	}
	if _, _, err := s.taskQueues(); err != nil {
		return Caller{}, err
	}
	user := auth.Subject(ctx)
	person, err := s.catalog.TaskPersonForAccount(user, create)
	return Caller{Person: person, Account: user}, err
}

// verifyTaskMember checks with the messaging client that a member is an
// active, human, non-guest member of its workspace.
func (s *Service) verifyTaskMember(ctx context.Context, settings catalog.TaskQueues, user string) error {
	_, transport, ok := s.hooks.Client(settings.Client)
	verifier, verifies := transport.(messaging.MemberVerifier)
	if !ok || !verifies {
		return webcore.Error{Status: 403, Code: "forbidden", Message: "Task queues cannot check workspace members"}
	}
	return verifier.VerifyMember(ctx, messaging.Address{Client: settings.Client, Workspace: settings.Workspace, User: user, Channel: user, Direct: true})
}

// taskRecipient resolves whose queue an add or reassignment names. Names are
// never guessed: a member is named by mention or ID, an account by user ID.
func (s *Service) taskRecipient(ctx context.Context, caller Caller, name string) (catalog.TaskPerson, error) {
	settings, _, err := s.taskQueues()
	if err != nil {
		return catalog.TaskPerson{}, err
	}
	name = strings.TrimSpace(name)
	if m := taskMention.FindStringSubmatch(name); m != nil {
		name = m[1]
	}
	switch {
	case name == "" || strings.EqualFold(name, "me"):
		if caller.Person.ID == "" {
			if caller.Account == "" {
				return catalog.TaskPerson{}, ErrNoQueue
			}
			return s.catalog.TaskPersonForAccount(caller.Account, true)
		}
		return caller.Person, nil
	case catalog.ValidTaskPerson(name):
		if p, ok := s.catalog.TaskPerson(name); ok && (p.User == "" || s.verifyTaskMember(ctx, settings, p.User) == nil) {
			return p, nil
		}
	case settings.Client != "" && messaging.ValidID(name) && strings.ToUpper(name) == name && (strings.HasPrefix(name, "U") || strings.HasPrefix(name, "W")):
		if err := s.verifyTaskMember(ctx, settings, name); err != nil {
			return catalog.TaskPerson{}, webcore.Error{Status: 404, Code: "not_found", Message: "No active workspace member " + name + " can have tasks"}
		}
		return s.catalog.TaskPersonForMember(settings.Client, settings.Workspace, name, true)
	default:
		if p, err := s.catalog.TaskPersonForAccount(name, true); err == nil {
			return p, nil
		}
	}
	return catalog.TaskPerson{}, webcore.Error{Status: 404, Code: "not_found", Message: "No one by that ID can have tasks. Name a person by Slack mention or member ID, account user ID or person ID"}
}

// ErrNoQueue reports a caller who has no queue yet and so has nothing to list.
var ErrNoQueue = webcore.Error{Status: 404, Code: "not_found", Message: "You have no task queue yet"}

// PersonView names a person as their workspace knows them, or else by
// their account.
func (s *Service) personView(id string, caller Caller) PersonView {
	view := PersonView{ID: id, You: id == caller.Person.ID}
	p, ok := s.catalog.TaskPerson(id)
	if !ok {
		return view
	}
	view.SlackUser = s.catalog.TaskPersonMember(p.ID)
	if view.SlackUser != "" {
		view.Name = s.MemberName(view.SlackUser)
	}
	if account, _ := s.catalog.TaskPersonAccount(id); account != "" && view.Name == "" {
		if name, err := s.catalog.SubjectName(account); err == nil {
			view.Name = name
		}
	}
	return view
}

// memberNameCache holds the task workspace's member names. A member without a
// name rereads the list at most once a minute; the list is kept an hour. The
// list is read from Slack outside mu, and whoever asks meanwhile waits for it
// within their own deadline.
type memberNameCache struct {
	mu            sync.Mutex
	names         map[string]string
	fetched, read time.Time
	// reading is closed when the list being read arrives or fails.
	reading chan struct{}
}

// memberName is a task workspace member's Slack name, or empty when the
// member list cannot be read within five seconds.
func (s *Service) MemberName(member string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.MemberNameWithin(ctx, member)
}

// memberNameWithin is memberName, reading the member list only within ctx.
func (s *Service) MemberNameWithin(ctx context.Context, member string) string {
	c := &s.memberNames
	c.mu.Lock()
	name, known := c.names[member]
	if known && time.Since(c.fetched) < time.Hour {
		c.mu.Unlock()
		return name
	}
	if reading := c.reading; reading != nil {
		c.mu.Unlock()
		select {
		case <-reading:
		case <-ctx.Done():
			return name
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.names[member]
	}
	if time.Since(c.read) < time.Minute {
		c.mu.Unlock()
		return name
	}
	reading := make(chan struct{})
	c.read, c.reading = time.Now(), reading
	c.mu.Unlock()
	members, err := s.taskMembers(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reading = nil
	close(reading)
	switch {
	case err != nil:
		s.logAutomationError("Task member names", err)
	case members != nil:
		c.keep(members)
	}
	return c.names[member]
}

// taskMembers lists the task workspace's members, or none when its client
// cannot list them.
func (s *Service) taskMembers(ctx context.Context) ([]messaging.Member, error) {
	settings, _, err := s.taskQueues()
	_, transport, ok := s.hooks.Client(settings.Client)
	lister, lists := transport.(messaging.MemberLister)
	if err != nil || !ok || !lists {
		return nil, nil
	}
	return lister.Members(ctx)
}

// seed keeps a member list read for another purpose.
func (c *memberNameCache) seed(members []messaging.Member) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.keep(members)
}

// keep replaces the cached names with a fresh member list. Caller holds mu.
func (c *memberNameCache) keep(members []messaging.Member) {
	c.names, c.fetched, c.read = map[string]string{}, time.Now(), time.Now()
	for _, m := range members {
		if m.Name != "" {
			c.names[m.User] = m.Name
		}
	}
}

func (s *Service) view(t tasks.Task, owner string, caller Caller) View {
	view := View{Task: t, Owner: s.personView(owner, caller), Requester: s.personView(t.From, caller)}
	if t.Every != nil {
		due, _ := time.Parse(time.DateOnly, t.Due)
		view.NextDates = append([]string{t.Due}, t.Every.NextAfter(due, 1)...)
	}
	return view
}

// Book is every queue note at one revision.
type Book struct {
	revision string
	Queues   map[string]tasks.Queue
	hashes   map[string]string
}

func taskPath(person string) string { return "people/" + person + ".md" }

func ReadBook(ctx context.Context, store *workspace.Store) (Book, error) {
	snapshot, err := store.Snapshot(ctx, "")
	if err != nil {
		return Book{}, err
	}
	book := Book{revision: snapshot.Revision, Queues: map[string]tasks.Queue{}, hashes: map[string]string{}}
	for _, f := range snapshot.Files {
		person, ok := strings.CutPrefix(strings.TrimSuffix(f.Path, ".md"), "people/")
		if !ok || !catalog.ValidTaskPerson(person) || f.Path != taskPath(person) {
			continue
		}
		q, err := tasks.Parse(f.Content)
		if err != nil {
			return Book{}, webcore.Error{Status: 409, Code: "conflict", Message: "The queue note " + f.Path + " is not in task format: " + err.Error()}
		}
		book.Queues[person], book.hashes[person] = q, f.Hash
	}
	return book, nil
}

// find returns the owner holding a task and its index.
func (b Book) Find(id string) (string, int) {
	for person, q := range b.Queues {
		if i := q.Find(id); i >= 0 {
			return person, i
		}
	}
	return "", -1
}

// edit writes the queues of the named people as one group.
func (b Book) edits(people ...string) []workspace.NoteEdit {
	out := []workspace.NoteEdit{}
	for _, person := range people {
		edit := workspace.NoteEdit{Path: taskPath(person), Content: b.Queues[person].Format()}
		if hash := b.hashes[person]; hash != "" {
			edit.Revision, edit.Hash = b.revision, hash
		}
		out = append(out, edit)
	}
	return out
}

// apply makes a decided change to the queues: the task changes in place,
// leaves its queue, or moves to the end of its new owner's. It returns the
// people whose queues changed.
func (b *Book) apply(owner string, i int, d tasks.Decision) []string {
	q := b.Queues[owner]
	if d.Closed == "" {
		q.Tasks[i] = d.Task
		b.Queues[owner] = q
		return []string{owner}
	}
	q.Tasks = slices.Delete(q.Tasks, i, i+1)
	b.Queues[owner] = q
	if d.Closed != "reassigned" {
		return []string{owner}
	}
	moved := b.Queues[d.Owner]
	moved.Tasks = append(moved.Tasks, d.Task)
	b.Queues[d.Owner] = moved
	return []string{owner, d.Owner}
}

// taskContext attributes a commit to the account acting, or to a member who
// has none by their person ID.
func (s *Service) taskContext(ctx context.Context, caller Caller) context.Context {
	if caller.Account != "" && attribution.From(ctx).ID == caller.Account {
		return ctx
	}
	if caller.Account != "" {
		if account, err := s.catalog.Account(caller.Account); err == nil {
			return attribution.WithSource(ctx, account.Identity(), "tasks", "slack")
		}
	}
	return attribution.WithSource(ctx, attribution.Identity{Name: "Person " + caller.Person.ID, Email: caller.Person.ID + "@tasks.invalid"}, "tasks", "slack")
}

// commitTask rereads every queue, applies change and commits the queues it
// names as one commit, retrying when another task change committed first. An
// operation ID makes a repeated request a replay.
func (s *Service) commitTask(ctx context.Context, store *workspace.Store, caller Caller, operation string, change func(*Book) (string, []string, error)) (Book, string, error) {
	ctx = s.taskContext(ctx, caller)
	for attempt := 1; ; attempt++ {
		book, err := ReadBook(ctx, store)
		if err != nil {
			return book, "", err
		}
		summary, people, err := change(&book)
		if err != nil || len(people) == 0 {
			return book, book.revision, err
		}
		if len(summary) > 300 {
			summary = summary[:297] + "…"
			for !utf8.ValidString(summary) {
				summary = summary[:len(summary)-4] + "…"
			}
		}
		id := operation
		if id == "" {
			id = "task-" + randomTaskSuffix(16)
		}
		result, err := store.ApplyGroup(ctx, workspace.EditGroup{ID: id, Summary: summary, Edits: book.edits(people...)})
		// Retrying under the same operation is safe: a group that committed is
		// replayed, and one that did not commits once from the newer queues.
		if errors.Is(err, workspace.ErrConflict) && attempt < taskWriteAttempts {
			continue
		}
		return book, result.Revision, err
	}
}

func randomTaskSuffix(n int) string {
	raw := make([]byte, (n*5+7)/8)
	rand.Read(raw)
	return strings.ToLower(base32.StdEncoding.EncodeToString(raw))[:n]
}

// taskTime is now, as task queues read it.
func (s *Service) taskTime() time.Time {
	if s.Clock != nil {
		return s.Clock().UTC()
	}
	return time.Now().UTC()
}

// addTasks files a task in the queue of each person named, once per person
// and in the order named, so each owner closes their own copy. A channel, user
// group or everyone named gives each active member in it but the requester a
// copy, after anyone named alone. Every name is resolved and the task checked
// before anything is written, and each copy is its own commit, so its history
// reads like any other task's. More than taskConfirmAbove copies need
// in.Confirm. operation, when set, names the request so a retry adds nothing
// new, and fixes the task IDs.
func (s *Service) addTasks(ctx context.Context, caller Caller, in Input, operation string) ([]View, error) {
	settings, store, err := s.taskQueues()
	if err != nil {
		return nil, err
	}
	if caller.Person.ID == "" {
		if caller.Account == "" {
			return nil, workspace.ErrForbidden
		}
		if caller.Person, err = s.catalog.TaskPersonForAccount(caller.Account, true); err != nil {
			return nil, err
		}
	}
	names := in.People
	switch {
	case len(names) == 0:
		names = []string{in.Person}
	case strings.TrimSpace(in.Person) != "":
		return nil, webcore.Invalid("Name either person or people, not both")
	case len(names) > PeopleLimit:
		return nil, webcore.Invalid("Give a task to at most " + strconv.Itoa(PeopleLimit) + " people at once, or name a channel or user group")
	}
	owners, audiences, err := s.taskOwners(ctx, settings, caller, names)
	if err != nil {
		return nil, err
	}
	if len(owners) > taskAudienceLimit {
		return nil, webcore.Invalid("That names " + strconv.Itoa(len(owners)) + " people; a task goes to at most " + strconv.Itoa(taskAudienceLimit) + " at once")
	}
	t := tasks.Task{ID: "t-" + randomTaskSuffix(8), Text: strings.TrimSpace(in.Text), From: caller.Person.ID, Added: s.taskTime().Format(time.DateOnly), Due: strings.TrimSpace(in.Due), Source: strings.TrimSpace(in.Source)}
	switch priority := strings.ToLower(strings.TrimSpace(in.Priority)); priority {
	case "", "normal":
	case "high", "low":
		if len(owners) != 1 || owners[0].person != caller.Person.ID {
			return nil, webcore.Invalid("Only the queue's owner sets priority")
		}
		t.Priority = priority
	default:
		return nil, webcore.Invalid("Priority must be high, normal or low")
	}
	if strings.TrimSpace(in.Every) != "" {
		if t.Due == "" {
			return nil, webcore.Invalid("A recurring task needs a due date: its first date")
		}
		r, err := tasks.ParseRecurrence(in.Every, t.Due)
		if err != nil {
			return nil, webcore.Invalid(err.Error())
		}
		t.Every = &r
	}
	t.Lead = in.Lead
	if err = t.Check(); err != nil {
		return nil, webcore.Invalid(err.Error())
	}
	if len(owners) == 0 {
		return nil, webcore.Invalid("No one else there can have tasks")
	}
	if len(owners) > taskConfirmAbove && !in.Confirm {
		return nil, taskConfirmation{Count: len(owners), Audiences: audiences}
	}
	// Members of a channel, group or the workspace get a queue only now.
	people := []string{}
	for _, o := range owners {
		if o.person == "" {
			p, err := s.catalog.TaskPersonForMember(settings.Client, settings.Workspace, o.member, true)
			if err != nil {
				return nil, err
			}
			o.person = p.ID
		}
		if !slices.Contains(people, o.person) {
			people = append(people, o.person)
		}
	}
	if len(people) > 1 {
		book, err := ReadBook(ctx, store)
		if err != nil {
			return nil, err
		}
		for _, owner := range people {
			if len(book.Queues[owner].Tasks) >= taskQueueLimit {
				whose := "Your queue"
				if view := s.personView(owner, caller); !view.You {
					whose = "The queue of " + taskName(view)
				}
				return nil, webcore.Invalid(whose + " already holds " + strconv.Itoa(taskQueueLimit) + " open tasks")
			}
		}
	}
	views := []View{}
	// Owners hear about their copies once every copy that could be filed is.
	defer func() { s.noticeAdded(ctx, caller, views) }()
	for _, owner := range people {
		copyOperation := operation
		if operation != "" && len(people) > 1 {
			copyOperation += "/" + owner
		}
		view, err := s.fileTask(ctx, store, caller, owner, t, copyOperation)
		if err != nil && len(views) > 0 {
			return nil, fmt.Errorf("added %d of %d copies, then: %w", len(views), len(people), err)
		}
		if err != nil {
			return nil, err
		}
		views = append(views, view)
	}
	return views, nil
}

// taskOwner is one queue a request names: a person, or a workspace member who
// may have no queue yet.
type taskOwner struct{ person, member string }

// taskOwners resolves the names in a request to the queues they name, each
// once: people named alone first, then the members of each channel, user
// group or workspace named, leaving out the requester and anyone who is not
// an active, human, non-guest member. audiences describes what was expanded.
func (s *Service) taskOwners(ctx context.Context, settings catalog.TaskQueues, caller Caller, names []string) ([]taskOwner, []string, error) {
	owners, audiences := []taskOwner{}, []string{}
	persons, members := map[string]bool{}, map[string]bool{}
	var expand []string
	for _, name := range names {
		if taskAudience.MatchString(strings.TrimSpace(name)) {
			expand = append(expand, strings.TrimSpace(name))
			continue
		}
		owner, err := s.taskRecipient(ctx, caller, name)
		if err != nil {
			return nil, nil, err
		}
		if !persons[owner.ID] {
			persons[owner.ID] = true
			owners = append(owners, taskOwner{person: owner.ID})
			if member := s.catalog.TaskPersonMember(owner.ID); member != "" {
				members[member] = true
			}
		}
	}
	if len(expand) == 0 {
		return owners, audiences, nil
	}
	_, transport, ok := s.hooks.Client(settings.Client)
	lister, lists := transport.(messaging.AudienceLister)
	directory, listsAll := transport.(messaging.MemberLister)
	if settings.Client == "" || !ok || !lists || !listsAll {
		return nil, nil, webcore.Invalid("Channels, groups and everyone need a messaging client that lists members")
	}
	all, err := directory.Members(ctx)
	if err != nil {
		return nil, nil, err
	}
	s.memberNames.seed(all)
	eligible := map[string]bool{}
	for _, m := range all {
		eligible[m.User] = true
	}
	requester := s.catalog.TaskPersonMember(caller.Person.ID)
	for _, name := range expand {
		m := taskAudience.FindStringSubmatch(name)
		var list []string
		switch {
		case m[1] != "":
			list, err = lister.ChannelMembers(ctx, m[1])
			switch {
			case errors.Is(err, messaging.ErrAudienceHidden):
				return nil, nil, webcore.Invalid("I can't see <#" + m[1] + ">; if it is private, add me to it first")
			case errors.Is(err, messaging.ErrAudienceScope):
				return nil, nil, webcore.Invalid("Listing channel members needs the app's channels:read and groups:read permissions; ask an administrator to add them")
			case err != nil:
				return nil, nil, err
			case requester == "" || !slices.Contains(list, requester):
				return nil, nil, webcore.Invalid("You can only give tasks to channels you're in")
			}
			audiences = append(audiences, "everyone in <#"+m[1]+">")
		case m[2] != "":
			list, err = lister.GroupMembers(ctx, m[2])
			switch {
			case errors.Is(err, messaging.ErrAudienceHidden):
				return nil, nil, webcore.Invalid("I can't find the user group " + m[2])
			case errors.Is(err, messaging.ErrAudienceScope):
				return nil, nil, webcore.Invalid("Listing user groups needs the app's usergroups:read permission; ask an administrator to add it")
			case err != nil:
				return nil, nil, err
			}
			group := "user group " + m[2]
			if handle := strings.TrimSpace(m[3]); handle != "" {
				group = "@" + strings.TrimPrefix(mrkdwnEscape.Replace(handle), "@")
			}
			audiences = append(audiences, "everyone in "+group)
		default:
			for _, member := range all {
				list = append(list, member.User)
			}
			audiences = append(audiences, "everyone in the workspace")
		}
		for _, member := range list {
			if !eligible[member] || member == requester || members[member] {
				continue
			}
			members[member] = true
			if p, err := s.catalog.TaskPersonForMember(settings.Client, settings.Workspace, member, false); err == nil {
				if persons[p.ID] {
					continue
				}
				persons[p.ID] = true
				owners = append(owners, taskOwner{person: p.ID, member: member})
				continue
			} else if !errors.Is(err, catalog.ErrNoTaskPerson) {
				return nil, nil, err
			}
			owners = append(owners, taskOwner{member: member})
		}
	}
	return owners, audiences, nil
}

// noticeAdded tells each owner about their new copy, a few at a time.
func (s *Service) noticeAdded(ctx context.Context, caller Caller, views []View) {
	work := make(chan View)
	var wg sync.WaitGroup
	for range min(3, len(views)) {
		wg.Go(func() {
			for view := range work {
				s.noticeTask(ctx, taskNotice{Event: "added", Task: view, Actor: caller.Person.ID})
			}
		})
	}
	for _, view := range views {
		work <- view
	}
	close(work)
	wg.Wait()
}

// fileTask commits one task to an owner's queue.
func (s *Service) fileTask(ctx context.Context, store *workspace.Store, caller Caller, owner string, t tasks.Task, operation string) (View, error) {
	t.ID = "t-" + randomTaskSuffix(8)
	if operation != "" {
		sum := sha256.Sum256([]byte(operation))
		t.ID = "t-" + strings.ToLower(base32.StdEncoding.EncodeToString(sum[:]))[:8]
	}
	var added tasks.Task
	book, _, err := s.commitTask(ctx, store, caller, operation, func(b *Book) (string, []string, error) {
		if person, i := b.Find(t.ID); i >= 0 {
			if person != owner || b.Queues[person].Tasks[i].From != caller.Person.ID {
				return "", nil, workspace.ErrConflict
			}
			added = b.Queues[person].Tasks[i] // A retried request was already filed.
			return "", nil, nil
		}
		q := b.Queues[owner]
		if len(q.Tasks) >= taskQueueLimit {
			return "", nil, webcore.Invalid("This queue already holds " + strconv.Itoa(taskQueueLimit) + " open tasks")
		}
		q.Tasks = append(q.Tasks, t)
		b.Queues[owner] = q
		added = t
		return "Task " + t.ID + " added to " + owner + " by " + caller.Person.ID, []string{owner}, nil
	})
	if err != nil {
		return View{}, err
	}
	// The committed note is the queue as formatted here, so reading it back
	// gives the new task its revision.
	if added.Revision == "" {
		if q, err := tasks.Parse(book.Queues[owner].Format()); err == nil {
			if i := q.Find(added.ID); i >= 0 {
				added = q.Tasks[i]
			}
		}
	}
	return s.view(added, owner, caller), nil
}

// listTasks returns the caller's own queue and the open tasks they filed in
// other queues.
func (s *Service) List(ctx context.Context, caller Caller) (List, error) {
	_, store, err := s.taskQueues()
	if err != nil {
		return List{}, err
	}
	out := List{Person: caller.Person.ID, Mine: []View{}, Filed: []View{}}
	if caller.Person.ID == "" {
		return out, nil
	}
	book, err := ReadBook(ctx, store)
	if err != nil {
		return out, err
	}
	for _, t := range book.Queues[caller.Person.ID].Tasks {
		out.Mine = append(out.Mine, s.view(t, caller.Person.ID, caller))
	}
	owners := make([]string, 0, len(book.Queues))
	for owner := range book.Queues {
		owners = append(owners, owner)
	}
	slices.Sort(owners)
	for _, owner := range owners {
		if owner == caller.Person.ID {
			continue
		}
		for _, t := range book.Queues[owner].Tasks {
			if t.From == caller.Person.ID {
				out.Filed = append(out.Filed, s.view(t, owner, caller))
			}
		}
	}
	return out, nil
}

// changeTask edits, closes or reassigns one task: it reads the task as it is
// now, has tasks.Decide check and decide the change, commits it and tells
// whoever the change concerns.
func (s *Service) changeTask(ctx context.Context, caller Caller, in Update, operation string) (Result, error) {
	_, store, err := s.taskQueues()
	if err != nil {
		return Result{}, err
	}
	if !tasks.ValidID(in.ID) || caller.Person.ID == "" {
		return Result{}, taskNotFound(in.ID)
	}
	// The reason is checked, and a new owner found, before the task is read.
	if err := tasks.CheckReason(in.Reason); err != nil {
		return Result{}, taskRefusal(err)
	}
	change := tasks.Change{Revision: in.Revision, Text: in.Text, Due: in.Due, Priority: in.Priority, SnoozeUntil: in.SnoozeUntil, Every: in.Every, Lead: in.Lead, Outcome: in.Outcome, Reason: in.Reason}
	if in.ReassignTo != "" {
		target, err := s.taskRecipient(ctx, caller, in.ReassignTo)
		if err != nil {
			return Result{}, err
		}
		change.ReassignTo = target.ID
	}
	var decision tasks.Decision
	var before tasks.Task
	var previous string
	_, revision, err := s.commitTask(ctx, store, caller, operation, func(b *Book) (string, []string, error) {
		owner, i := b.Find(in.ID)
		if i < 0 {
			return "", nil, taskNotFound(in.ID)
		}
		previous, before = owner, b.Queues[owner].Tasks[i]
		d, err := tasks.Decide(before, owner, caller.Person.ID, change, s.taskTime())
		if err != nil {
			return "", nil, taskRefusal(err)
		}
		decision = d
		return d.Summary, b.apply(owner, i, d), nil
	})
	if err != nil {
		return Result{}, err
	}
	result := Result{Closed: decision.Closed, Occurrence: decision.Occurrence, Revision: revision}
	if result.Closed == "" || result.Closed == "reassigned" {
		book, err := ReadBook(ctx, store)
		if err != nil {
			return result, err
		}
		if person, i := book.Find(in.ID); i >= 0 {
			view := s.view(book.Queues[person].Tasks[i], person, caller)
			result.Task = &view
		}
	}
	notice := taskNotice{Actor: caller.Person.ID, Reason: decision.Reason, Occurrence: result.Occurrence, Previous: previous, Task: s.view(before, previous, caller)}
	if result.Task != nil {
		notice.Task = *result.Task
	}
	if notice.Event = decision.Notice(change, caller.Person.ID, before, notice.Task.Task, notice.Task.Owner.ID); notice.Event != "" {
		s.noticeTask(ctx, notice)
	}
	return result, nil
}

// taskRefusal gives a change internal/tasks refused the status every
// transport shows.
func taskRefusal(err error) error {
	var refused tasks.Refusal
	if !errors.As(err, &refused) {
		return err
	}
	switch refused.Kind {
	case tasks.ErrNotFound:
		return webcore.Error{Status: 404, Code: "not_found", Message: refused.Message}
	case tasks.ErrStale:
		return webcore.Error{Status: 409, Code: "conflict", Message: refused.Message}
	case tasks.ErrForbidden:
		return webcore.Error{Status: 403, Code: "forbidden", Message: refused.Message}
	}
	return webcore.Invalid(refused.Message)
}

func taskNotFound(id string) error { return taskRefusal(tasks.NotFound(id)) }

// taskHistory lists a task's changes for its current owner and its requester.
func (s *Service) taskHistory(ctx context.Context, caller Caller, id string) ([]HistoryEntry, error) {
	_, store, err := s.taskQueues()
	if err != nil {
		return nil, err
	}
	if !tasks.ValidID(id) || caller.Person.ID == "" {
		return nil, taskNotFound(id)
	}
	commits, err := store.TaskHistory(ctx, id)
	if err != nil || len(commits) == 0 {
		return nil, taskNotFound(id)
	}
	// The oldest commit names the queue and the requester; reassignments name
	// later owners. Only this task's own commits are read.
	owner, requester := "", ""
	for _, c := range slices.Backward(commits) {
		rest, _ := strings.CutPrefix(c.Message, "Task "+id+" added to ")
		if a, b, ok := strings.Cut(rest, " by "); ok && owner == "" {
			owner, requester = a, b
		}
		if rest, ok := strings.CutPrefix(c.Message, "Task "+id+": reassigned from "); ok {
			if _, after, ok := strings.Cut(rest, " to "); ok {
				owner, _, _ = strings.Cut(after, " ")
			}
		}
	}
	if caller.Person.ID != owner && caller.Person.ID != requester {
		return nil, taskNotFound(id)
	}
	out := make([]HistoryEntry, 0, len(commits))
	for _, c := range commits {
		change := strings.TrimPrefix(strings.TrimPrefix(c.Message, "Task "+id), ":")
		out = append(out, HistoryEntry{Revision: c.Revision, At: c.AuthoredAt, Change: strings.TrimSpace(change)})
	}
	return out, nil
}

// The task actions. The instance registers each for MCP, the API and chat with
// its tool description and scope, so every transport passes the same guard.

// ListAction lists the caller's queue and the tasks they filed.
func (s *Service) ListAction(ctx context.Context, in struct{}) (List, error) {
	caller, err := s.accountTaskCaller(ctx, false)
	if errors.Is(err, catalog.ErrNoTaskPerson) {
		return List{Mine: []View{}, Filed: []View{}}, nil
	}
	if err != nil {
		return List{}, err
	}
	return s.List(ctx, caller)
}

// AddAction adds a task to one queue, or a copy to each person named.
func (s *Service) AddAction(ctx context.Context, in Input) (Added, error) {
	caller, err := s.accountTaskCaller(ctx, true)
	if err != nil {
		return Added{}, err
	}
	if in.Confirm {
		// Hundreds of copies outlast a client's patience; a disconnect must
		// not stop them partway.
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), 3*time.Minute)
		defer cancel()
	}
	views, err := s.addTasks(ctx, caller, in, "")
	if err != nil {
		return Added{}, err
	}
	added := Added{View: views[0]}
	if len(in.People) > 0 {
		added.Tasks = views
	}
	return added, nil
}

// UpdateAction changes, closes or reassigns a task.
func (s *Service) UpdateAction(ctx context.Context, in Update) (Result, error) {
	caller, err := s.accountTaskCaller(ctx, false)
	if errors.Is(err, catalog.ErrNoTaskPerson) {
		return Result{}, taskNotFound(in.ID)
	}
	if err != nil {
		return Result{}, err
	}
	return s.changeTask(ctx, caller, in, "")
}

// HistoryInput names the task whose history is read.
type HistoryInput struct {
	ID string `json:"id" jsonschema:"Task ID"`
}

// HistoryAction lists every change to one task for its owner and requester.
func (s *Service) HistoryAction(ctx context.Context, in HistoryInput) ([]HistoryEntry, error) {
	caller, err := s.accountTaskCaller(ctx, false)
	if errors.Is(err, catalog.ErrNoTaskPerson) {
		return nil, taskNotFound(in.ID)
	}
	if err != nil {
		return nil, err
	}
	return s.taskHistory(ctx, caller, in.ID)
}

// Settings are the task queue settings with the reminder jobs that serve
// them. Enabling task queues creates no reminder job, so nobody is reminded
// of their tasks until an administrator adds one; Warnings says so. Like
// Project and Workspace, Reminders and Warnings are reported and a save
// ignores them, so settings read with task_queues can be changed and sent
// back.
type Settings struct {
	catalog.TaskQueues
	Reminders []string `json:"reminders,omitempty" jsonschema:"Reported: IDs of the enabled task-reminder automations for the client; ignored on save"`
	Warnings  []string `json:"warnings,omitempty" jsonschema:"Reported: setup that keeps task queues from working in full; ignored on save"`
}

func (s *Service) settings(settings catalog.TaskQueues) Settings {
	out := Settings{TaskQueues: settings}
	if settings.Client == "" {
		return out
	}
	for _, spec := range s.taskReminderSpecs(settings.Client) {
		out.Reminders = append(out.Reminders, spec.ID)
	}
	if len(out.Reminders) == 0 && settings.Project != "" && !settings.Disabled {
		out.Warnings = append(out.Warnings, "No enabled task-reminder automation names the client "+settings.Client+", so nobody gets weekday task reminders. Add one that runs as the client's members actor every 15 minutes.")
	}
	return out
}

// SettingsAction reads the task queue settings, for an administrator.
func (s *Service) SettingsAction(ctx context.Context, in struct{}) (Settings, error) {
	if !s.catalog.IsAdmin(auth.Subject(ctx)) {
		return Settings{}, workspace.ErrForbidden
	}
	settings, _, _ := s.catalog.TaskQueues()
	return s.settings(settings), nil
}

// SetSettingsAction enables or changes task queues, for an administrator.
func (s *Service) SetSettingsAction(ctx context.Context, in Settings) (Settings, error) {
	settings, err := s.catalog.SetTaskQueues(auth.Subject(ctx), in.TaskQueues)
	if err != nil {
		return Settings{}, err
	}
	return s.settings(settings), nil
}
