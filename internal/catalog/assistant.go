package catalog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"forgetmenote/internal/workspace"
)

// AssistantPolicy controls the instance feature; personal settings can only
// narrow it. Neither notebook prose nor provider credentials grant authority.
type AssistantPolicy struct {
	RetainDays       int    `json:"retainDays,omitempty"`
	TriageChars      int    `json:"triageChars,omitempty"`
	MemoryChars      int    `json:"memoryChars,omitempty"`
	BatchItems       int    `json:"batchItems,omitempty"`
	LookbackHours    int    `json:"lookbackHours,omitempty"`
	Revision         uint64 `json:"revision"`
	Enabled          bool   `json:"enabled"`
	DirectoryProject string `json:"directoryProject,omitempty"`
	AIConnection     string `json:"aiConnection,omitempty"`
	Model            string `json:"model,omitempty"`
	ReasoningEffort  string `json:"reasoningEffort,omitempty"`
	IntervalSeconds  int    `json:"intervalSeconds,omitempty"`
}

type AssistantWatch struct {
	Connection string          `json:"connection"`
	Operation  string          `json:"operation"`
	Arguments  json.RawMessage `json:"arguments"`
}
type AssistantSettings struct {
	Watches            []AssistantWatch `json:"watches,omitempty"`
	MailboxDrafts      bool             `json:"mailboxDrafts"`
	Revision           uint64           `json:"revision"`
	Project            string           `json:"project"`
	Enabled            bool             `json:"enabled"`
	ExternalProcessing bool             `json:"externalProcessing"`
	Features           []string         `json:"features"`
	ExcludedProjects   []string         `json:"excludedProjects,omitempty"`
	TimeZone           string           `json:"timeZone"`
	QuietStart         string           `json:"quietStart,omitempty"`
	QuietEnd           string           `json:"quietEnd,omitempty"`
	Notifications      bool             `json:"notifications"`
	MaintainMemory     bool             `json:"maintainMemory"`
	// ReplyReminders adds the Slack messages waiting on the person's reply to
	// their morning task reminder, read with their own Slack grant. Nothing it
	// reads reaches a model, so it needs no AI processing consent.
	ReplyReminders bool `json:"replyReminders,omitempty"`
}

var AssistantFeatures = []string{"email", "slack", "calendar", "documents", "followups", "briefing", "development", "operations"}

func (p Project) protected() bool {
	return p.ProtectedInbox != "" || p.AssistantOwner != "" || p.TaskQueues
}

func (p AssistantPolicy) Validate() error {
	if p.DirectoryProject != "" && !projectID.MatchString(p.DirectoryProject) || p.AIConnection != "" && !connectionID.MatchString(p.AIConnection) || len(p.Model) > 200 || strings.TrimSpace(p.Model) != p.Model {
		return errors.New("invalid assistant policy")
	}
	if !slices.Contains([]string{"", "none", "low", "medium", "high", "xhigh", "max"}, p.ReasoningEffort) {
		return errors.New("invalid assistant reasoning effort")
	}
	if p.RetainDays < 0 || int64(p.RetainDays) > math.MaxInt64/int64(24*time.Hour) {
		return errors.New("invalid assistant retention")
	}
	if p.BatchItems < 0 || p.LookbackHours < 0 || p.TriageChars < 0 || p.MemoryChars < 0 {
		return errors.New("assistant batch and lookback must be positive")
	}
	if int64(p.LookbackHours) > math.MaxInt64/int64(time.Hour) || int64(p.IntervalSeconds) > math.MaxInt64/int64(time.Second) {
		return errors.New("assistant time range is too large")
	}
	if p.IntervalSeconds < 0 || p.IntervalSeconds > 0 && p.IntervalSeconds < 60 {
		return errors.New("monitoring interval must be at least 60 seconds")
	}
	if p.IntervalSeconds > 31*86400 {
		return errors.New("monitoring interval exceeds the scheduler's 31-day range")
	}
	return nil
}

func (s AssistantSettings) Validate() error {
	if _, err := time.LoadLocation(s.TimeZone); err != nil || s.TimeZone == "" {
		return errors.New("choose a valid time zone")
	}
	if (s.QuietStart == "") != (s.QuietEnd == "") {
		return errors.New("provide both quiet-hour times")
	}
	for _, clock := range []string{s.QuietStart, s.QuietEnd} {
		if clock != "" {
			if _, err := time.Parse("15:04", clock); err != nil {
				return errors.New("quiet hours must use HH:MM")
			}
		}
	}
	for _, feature := range s.Features {
		if !slices.Contains(AssistantFeatures, feature) {
			return errors.New("unknown assistant feature")
		}
	}
	for _, watch := range s.Watches {
		var args map[string]any
		if !connectionID.MatchString(watch.Connection) || !connectionID.MatchString(watch.Operation) || json.Unmarshal(watch.Arguments, &args) != nil || args == nil {
			return errors.New("invalid personal data watch")
		}
	}
	for _, project := range s.ExcludedProjects {
		if !projectID.MatchString(project) {
			return errors.New("invalid excluded notebook")
		}
	}
	if s.Enabled && !s.ExternalProcessing {
		return errors.New("enable AI processing consent before enabling the assistant")
	}
	return nil
}

func (c *Catalog) AssistantPolicy() AssistantPolicy {
	c.lock()
	defer c.mu.Unlock()
	if c.version < 0 {
		return AssistantPolicy{}
	}
	if c.state.AssistantPolicy == nil {
		return AssistantPolicy{}
	}
	return *c.state.AssistantPolicy
}

func (c *Catalog) SetAssistantPolicy(actor string, policy AssistantPolicy) (AssistantPolicy, error) {
	c.lock()
	defer c.mu.Unlock()
	if !c.admin(actor) {
		return AssistantPolicy{}, workspace.ErrForbidden
	}
	var revision uint64
	if c.state.AssistantPolicy != nil {
		revision = c.state.AssistantPolicy.Revision
	}
	if revision != policy.Revision {
		return AssistantPolicy{}, workspace.ErrConflict
	}
	if err := policy.Validate(); err != nil {
		return AssistantPolicy{}, err
	}
	if policy.DirectoryProject != "" {
		p, ok := c.state.Projects[policy.DirectoryProject]
		if !ok || p.protected() {
			return AssistantPolicy{}, errors.New("select a shared responsibilities notebook")
		}
	}
	policy.Revision++
	err := c.change(actor, "assistant.policy", "instance", nil, func() error {
		c.state.AssistantPolicy = &policy
		return nil
	})
	return policy, err
}

func cloneAssistant(s AssistantSettings) AssistantSettings {
	s.Watches = slices.Clone(s.Watches)
	for i := range s.Watches {
		s.Watches[i].Arguments = slices.Clone(s.Watches[i].Arguments)
	}
	s.Features = slices.Clone(s.Features)
	s.ExcludedProjects = slices.Clone(s.ExcludedProjects)
	return s
}

func (c *Catalog) Assistant(subject string) (AssistantSettings, error) {
	account, err := c.Account(subject)
	if err != nil {
		return AssistantSettings{}, err
	}
	c.lock()
	defer c.mu.Unlock()
	if !c.enabledInboxSubject(subject, account.ID) {
		return AssistantSettings{}, workspace.ErrForbidden
	}
	settings, ok := c.state.Assistants[subject]
	if !ok {
		// Memory is on until the account turns it off. Migration 0015 turned on
		// the settings stored before this default.
		return AssistantSettings{TimeZone: "UTC", Features: []string{}, MaintainMemory: true}, nil
	}
	return cloneAssistant(settings), nil
}

// SaveAssistant provisions the protected binding and settings in one catalog
// transaction. Content seeding is idempotent and can resume after a restart.
func (c *Catalog) SaveAssistant(subject string, in AssistantSettings) (AssistantSettings, error) {
	account, err := c.Account(subject)
	if err != nil {
		return AssistantSettings{}, err
	}
	c.lock()
	defer c.mu.Unlock()
	if !c.enabledInboxSubject(subject, account.ID) {
		return AssistantSettings{}, workspace.ErrForbidden
	}
	old := c.state.Assistants[subject]
	if in.Revision != old.Revision {
		return AssistantSettings{}, workspace.ErrConflict
	}
	if (in.Enabled || in.ReplyReminders && !old.ReplyReminders) && (c.state.AssistantPolicy == nil || !c.state.AssistantPolicy.Enabled) {
		return AssistantSettings{}, errors.New("the administrator has not enabled personal assistants")
	}
	if in.ReplyReminders && !old.ReplyReminders && (!c.tasksEnabled() || !c.taskAccount(subject)) {
		return AssistantSettings{}, errors.New("reply reminders come with the morning task reminder, which needs task queues")
	}
	if in.TimeZone == "" {
		in.TimeZone = "UTC"
	}
	if err = in.Validate(); err != nil {
		return AssistantSettings{}, err
	}
	id := "assistant-" + account.ID
	if in.Project != "" && in.Project != id {
		return AssistantSettings{}, workspace.ErrForbidden
	}
	if p, ok := c.state.Projects[id]; ok && (p.AssistantOwner != account.ID || p.Members[subject] != "owner") {
		return AssistantSettings{}, workspace.ErrConflict
	}
	store, exists := c.stores[id]
	created := false
	if !exists {
		store, created, err = c.createStore(id)
		if err != nil {
			return AssistantSettings{}, err
		}
	}
	in.Project, in.Revision = id, old.Revision+1
	in = cloneAssistant(in)
	slices.Sort(in.Features)
	in.Features = slices.Compact(in.Features)
	err = c.change(subject, "assistant.settings", subject, nil, func() error {
		if c.state.Assistants == nil {
			c.state.Assistants = map[string]AssistantSettings{}
		}
		c.state.Assistants[subject] = in
		if !exists {
			c.state.Projects[id] = Project{ID: id, Name: "My assistant", Kind: "notes", AssistantOwner: account.ID, Members: map[string]string{subject: "owner"}}
		}
		return nil
	})
	if err != nil {
		if !exists {
			err = c.abandonStore(id, store, created, err)
		}
		return AssistantSettings{}, err
	}
	c.stores[id] = store
	return cloneAssistant(in), nil
}

// ActiveAssistants is scheduler input, never an employee directory endpoint.
func (c *Catalog) ActiveAssistants() map[string]AssistantSettings {
	c.lock()
	defer c.mu.Unlock()
	out := map[string]AssistantSettings{}
	if c.state.AssistantPolicy == nil || !c.state.AssistantPolicy.Enabled {
		return out
	}
	for user, settings := range c.state.Assistants {
		if settings.Enabled && settings.ExternalProcessing && c.enabledInboxSubject(user, c.state.Accounts[user].ID) {
			out[user] = cloneAssistant(settings)
		}
	}
	return out
}

// ReplyReminderAccounts lists the accounts whose morning task reminder also
// lists the Slack messages waiting on their reply. It is scheduler input,
// never an employee directory endpoint.
func (c *Catalog) ReplyReminderAccounts() []string {
	c.lock()
	defer c.mu.Unlock()
	out := []string{}
	if c.state.AssistantPolicy == nil || !c.state.AssistantPolicy.Enabled {
		return out
	}
	for user, settings := range c.state.Assistants {
		if settings.ReplyReminders && c.enabledInboxSubject(user, c.state.Accounts[user].ID) {
			out = append(out, user)
		}
	}
	slices.Sort(out)
	return out
}

func (c *Catalog) validateAssistants() error {
	if c.state.AssistantPolicy != nil && (c.state.AssistantPolicy.Revision == 0 || c.state.AssistantPolicy.Validate() != nil) {
		return errors.New("invalid assistant policy")
	}
	for user, settings := range c.state.Assistants {
		id := c.state.Accounts[user].ID
		p := c.state.Projects[settings.Project]
		if len(id) != 32 || settings.Revision == 0 || settings.Validate() != nil || p.ID != "assistant-"+id || p.AssistantOwner != id || p.Kind != "notes" || p.ProtectedInbox != "" || len(p.Members) != 1 || p.Members[user] != "owner" || len(p.GroupRoles) != 0 || c.state.Users[user].Kind == "service" {
			return errors.New("invalid private assistant binding")
		}
	}
	for _, p := range c.state.Projects {
		if p.AssistantOwner != "" {
			found := false
			for user := range p.Members {
				found = found || c.state.Assistants[user].Project == p.ID || c.projectMemoryOf(user, p)
			}
			if !found {
				return errors.New("orphaned private assistant notebook")
			}
		}
	}
	return nil
}

// Project memory notebooks are version-10 state: earlier binaries refuse them as
// orphaned assistant notebooks, so they never lose their protection on rollback.
func (c *Catalog) projectMemoryOf(user string, p Project) bool {
	suffix, ok := strings.CutPrefix(p.ID, "assistant-"+p.AssistantOwner+"-")
	return ok && len(suffix) == 16 && strings.Trim(suffix, "0123456789abcdef") == "" && c.state.Accounts[user].ID == p.AssistantOwner && c.state.Assistants[user].Project == "assistant-"+p.AssistantOwner && p.Kind == "notes" && p.ProtectedInbox == "" && len(p.Members) == 1 && p.Members[user] == "owner" && len(p.GroupRoles) == 0 && c.state.Users[user].Kind != "service"
}

// ProjectMemory returns the private notebook holding one project's agent memory
// for subject, or "" when it does not exist and create is unset. Like the
// assistant notebook, which holds core memory and must exist first, its ID
// derives from the account, so no one can pre-claim it, and it is never shared.
func (c *Catalog) ProjectMemory(subject, key string, create bool) (string, error) {
	account, err := c.Account(subject)
	if err != nil {
		return "", err
	}
	c.lock()
	defer c.mu.Unlock()
	if !c.enabledInboxSubject(subject, account.ID) || c.state.Assistants[subject].Project == "" {
		return "", workspace.ErrForbidden
	}
	if key = strings.TrimSpace(key); key == "" || !utf8.ValidString(key) {
		return "", errors.New("project memory needs a project key")
	}
	sum := sha256.Sum256([]byte(key))
	id := "assistant-" + account.ID + "-" + hex.EncodeToString(sum[:8])
	if p, ok := c.state.Projects[id]; ok {
		if p.AssistantOwner != account.ID || p.Members[subject] != "owner" {
			return "", workspace.ErrConflict
		}
		return id, nil
	}
	if !create {
		return "", nil
	}
	name := []rune("Memory: " + key)
	if len(name) > 120 {
		name = append(name[:119], '…')
	}
	store, created, err := c.createStore(id)
	if err != nil {
		return "", err
	}
	err = c.change(subject, "assistant.memory", id, nil, func() error {
		c.state.Projects[id] = Project{ID: id, Name: string(name), Kind: "notes", AssistantOwner: account.ID, Members: map[string]string{subject: "owner"}}
		return nil
	})
	if err != nil {
		return "", c.abandonStore(id, store, created, err)
	}
	c.stores[id] = store
	return id, nil
}

// AssistantSubject resolves only an enabled personal binding, never an email
// guess. Used with an identity verified by the employee's provider grant.
func (c *Catalog) AssistantSubject(account string) (string, bool) {
	c.lock()
	defer c.mu.Unlock()
	if c.state.AssistantPolicy == nil || !c.state.AssistantPolicy.Enabled {
		return "", false
	}
	for user, settings := range c.state.Assistants {
		if settings.Enabled && settings.ExternalProcessing && c.state.Accounts[user].ID == account && c.enabledInboxSubject(user, account) {
			return user, true
		}
	}
	return "", false
}
