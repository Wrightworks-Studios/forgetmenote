// Package automation owns bounded durable work in the instance process. Its
// runner uses the application's existing actions and workspace commit boundary.
package automation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"forgetmenote/internal/capacity"
	"forgetmenote/internal/jsonfile"
	"forgetmenote/internal/pg"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"forgetmenote/internal/chat"
	"forgetmenote/internal/connectors"
	"forgetmenote/internal/messaging"
)

type Call struct {
	Time       map[string]TimeArgument `json:"time,omitempty"`
	Connection string                  `json:"connection"`
	Operation  string                  `json:"operation"`
	Arguments  map[string]any          `json:"arguments"`
	SchemaHash string                  `json:"schemaHash,omitempty"`
}
type Assertion struct {
	Pointer string          `json:"pointer"`
	Equals  json.RawMessage `json:"equals"`
}
type Check struct {
	Evidence      []Call      `json:"evidence,omitempty"`
	SQLTemplate   string      `json:"sqlTemplate,omitempty"`
	DependsOn     []string    `json:"dependsOn,omitempty"`
	Reference     *Call       `json:"reference,omitempty"`
	Name          string      `json:"name"`
	Block         string      `json:"block,omitempty"`
	Call          Call        `json:"call"`
	Assertions    []Assertion `json:"assertions"`
	MaxDurationMS int64       `json:"maxDurationMs,omitempty"`
}
type UsageTrigger struct {
	Errors          bool  `json:"errors,omitempty"`
	SlowMS          int64 `json:"slowMs,omitempty"`
	CooldownSeconds int   `json:"cooldownSeconds"`
}
type Observation struct {
	ProfileRevision string `json:"profileRevision"`
	Connection      string `json:"connection"`
	Operation       string `json:"operation"`
	Outcome         string `json:"outcome"`
	DurationMS      int64  `json:"durationMs"`
	Receipt         string `json:"receipt,omitempty"`
}
type Delivery = messaging.Address
type Condition struct {
	Pointer     string          `json:"pointer"`
	Equals      json.RawMessage `json:"equals,omitempty"`
	GreaterThan *float64        `json:"greaterThan,omitempty"`
	LessThan    *float64        `json:"lessThan,omitempty"`
}
type Spec struct {
	Personal           *Personal           `json:"personal,omitempty"`
	ReasoningEffort    string              `json:"reasoningEffort,omitempty"`
	Script             *Script             `json:"script,omitempty"`
	Repair             *RepairPolicy       `json:"repair,omitempty"`
	Retry              *RetryPolicy        `json:"retry,omitempty"`
	Drive              *DriveSync          `json:"drive,omitempty"`
	Trigger            *Trigger            `json:"trigger,omitempty"`
	ReadProjects       []string            `json:"readProjects,omitempty"`
	Native             *NativeTask         `json:"native,omitempty"`
	Alert              *AlertPolicy        `json:"alert,omitempty"`
	NotifyChannel      string              `json:"notifyChannel,omitempty"`
	Operations         map[string][]string `json:"operations,omitempty"`
	MaxTokens          int                 `json:"maxTokens,omitempty"`
	Schedule           *Schedule           `json:"schedule,omitempty"`
	Name               string              `json:"name,omitempty"`
	Description        string              `json:"description,omitempty"`
	Disabled           bool                `json:"disabled,omitempty"`
	Generation         uint64              `json:"generation,omitempty"`
	ID                 string              `json:"id"`
	Kind               string              `json:"kind"`
	Actor              string              `json:"actor"`
	Project            string              `json:"project"`
	IntervalSeconds    int                 `json:"intervalSeconds,omitempty"`
	Connections        []string            `json:"connections,omitempty"`
	Recipe             string              `json:"recipe,omitempty"`
	Output             string              `json:"output,omitempty"`
	Calls              []Call              `json:"calls,omitempty"`
	Checks             []Check             `json:"checks,omitempty"`
	AIConnection       string              `json:"aiConnection,omitempty"`
	Model              string              `json:"model,omitempty"`
	Instruction        string              `json:"instruction,omitempty"`
	ExternalProcessing bool                `json:"externalProcessing,omitempty"`
	MaxRounds          int                 `json:"maxRounds,omitempty"`
	TimeoutSeconds     int                 `json:"timeoutSeconds,omitempty"`
	Delivery           *Delivery           `json:"delivery,omitempty"`
	Usage              *UsageTrigger       `json:"usage,omitempty"`
	NotifySlackUser    string              `json:"notifySlackUser,omitempty"`
	Condition          *Condition          `json:"condition,omitempty"`
	Learning           *LearningPolicy     `json:"learning,omitempty"`
}
type Config struct {
	Version int    `json:"version"`
	Jobs    []Spec `json:"jobs"`
}
type Publication struct {
	Hash           string   `json:"hash,omitempty"`
	Path           string   `json:"path"`
	Content        string   `json:"content"`
	Revision       string   `json:"revision"`
	LearningKey    string   `json:"learningKey,omitempty"`
	LearningShapes []string `json:"learningShapes,omitempty"`
}
type Run struct {
	ChatGuidanceHash  string               `json:"chatGuidanceHash,omitempty"`
	Script            *ScriptCheckpoint    `json:"script,omitempty"`
	ChatDocuments     []chat.Attachment    `json:"chatDocuments,omitempty"`
	ChatContext       []chat.Message       `json:"chatContext,omitempty"`
	ChatPrepared      bool                 `json:"chatPrepared,omitempty"`
	ChatResults       []connectors.Result  `json:"chatResults,omitempty"`
	Artifacts         []messaging.Artifact `json:"artifacts,omitempty"`
	CheckReport       *CheckReport         `json:"checkReport,omitempty"`
	RetryAt           time.Time            `json:"retryAt,omitempty"`
	RetryErrors       []string             `json:"retryErrors,omitempty"`
	Batch             *EventBatch          `json:"batch,omitempty"`
	Alert             *AlertDecision       `json:"alert,omitempty"`
	GenerationStarted bool                 `json:"generationStarted,omitempty"`
	DryRun            bool                 `json:"dryRun,omitempty"`
	Result            string               `json:"result,omitempty"`
	Version           int                  `json:"version"`
	ID                string               `json:"id"`
	Spec              Spec                 `json:"spec"`
	State             string               `json:"state"`
	Created           time.Time            `json:"created"`
	Updated           time.Time            `json:"updated"`
	Error             string               `json:"error,omitempty"`
	Attempts          int                  `json:"attempts"`
	Receipts          []connectors.Receipt `json:"receipts,omitempty"`
	Citations         []chat.Citation      `json:"citations,omitempty"`
	Publication       *Publication         `json:"publication,omitempty"`
	Answer            string               `json:"answer,omitempty"`
	DeliveryState     string               `json:"deliveryState,omitempty"`
	InputTokens       int                  `json:"inputTokens,omitempty"`
	OutputTokens      int                  `json:"outputTokens,omitempty"`
	Observation       *Observation         `json:"observation,omitempty"`
	Learning          *LearningBatch       `json:"learning,omitempty"`
	// WriteProposals are this run's unconfirmed writes. They are committed with
	// the answer to the chat conversation, which owns them from then on.
	WriteProposals []chat.Operation `json:"-"`
	// MemoryOperations record that this run recalled or remembered the
	// person's memory. They are committed with the answer, like WriteProposals.
	MemoryOperations []chat.Operation `json:"-"`
	// Claimant is the gateway running the run; while it is present, no other
	// gateway queues the run again.
	Claimant string `json:"claimant,omitempty"`
	// stored is the version of the record this copy was read from or last
	// written as, 0 for a run not stored yet. A write over any other version
	// conflicts.
	stored int64
	// summary marks a copy a scheduling scan kept only metadata of; whole
	// reads the rest.
	summary bool
}
type Queue struct {
	effectIndex  map[string]map[string]string
	effectExpiry map[string]time.Time
	mu           sync.Mutex
	release      func()
	records      records
	active       map[string]context.CancelFunc
	summaries    map[string]runSummary
	scanned      time.Time // when summaries were last listed
	// announce is set by a critical section that changed what workers may
	// claim; unlock then tells this gateway's claimer and the other gateways.
	announce bool
	// wake signals this gateway's claimer, and changes counts the changes to
	// queued work this gateway made or heard of, which its scheduler follows.
	wake    chan struct{}
	changes atomic.Uint64
}

// QueueChannel is where gateways announce changes to queued work and to the
// configured jobs, so the others claim and schedule without polling.
const QueueChannel = "automation_queue"

// lock enters a queue critical section: q.mu within this gateway, and the
// instance's queue lock across gateways, so changes are serialized as one
// process serialized them. When the lock cannot be taken, every record access
// in the section fails instead. Each record write is fenced by the version it
// read, so a section whose lock connection dropped cannot overwrite another
// gateway's change. Sections that only read take q.mu alone.
func (q *Queue) lock() {
	q.mu.Lock()
	ctx, cancel := storageContext()
	defer cancel()
	release, err := q.records.db.Exclusive(ctx, "automation-queue")
	if err != nil {
		q.records.blocked = fmt.Errorf("automation queue unavailable: %w", err)
		return
	}
	q.release = release
}

func (q *Queue) unlock() {
	if q.release != nil {
		q.release()
		q.release = nil
	}
	q.records.blocked = nil
	announce := q.announce
	q.announce = false
	q.mu.Unlock()
	if announce {
		q.heard()
		// Announcements are hints: a gateway that misses one finds the change
		// on its next fallback scan.
		ctx, cancel := storageContext()
		defer cancel()
		_ = q.records.db.Announce(ctx, QueueChannel, q.records.db.Holder)
	}
}

// heard records a change to queued work and wakes this gateway's claimer.
func (q *Queue) heard() {
	q.changes.Add(1)
	q.signal()
}

func (q *Queue) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// claimed reports a run being run here or on another gateway.
func (q *Queue) claimed(r Run) bool { return q.active[r.ID] != nil || r.State == "running" }

// Polling needs scheduling metadata, not retained inputs or generated content.
// Record versions keep this bounded cache derived from the stored runs.
type runSummary struct {
	version     int64
	run         Run
	stopMessage bool
}

func summarizeRun(r Run) Run {
	s := Run{stored: r.stored, summary: true, ID: r.ID, State: r.State, Created: r.Created, Updated: r.Updated, RetryAt: r.RetryAt, DryRun: r.DryRun, Spec: Spec{ID: r.Spec.ID, Kind: r.Spec.Kind, AIConnection: r.Spec.AIConnection, IntervalSeconds: r.Spec.IntervalSeconds, Schedule: r.Spec.Schedule}}
	if r.Spec.Kind == "assistant" && r.Spec.Delivery != nil && r.Spec.Delivery.Client == "slack" {
		d := *r.Spec.Delivery
		d.Files = nil
		s.Spec.Delivery = &d
	}
	if r.Spec.Script != nil {
		s.Spec.Script = &Script{RetentionHours: r.Spec.Script.RetentionHours}
		if r.Script != nil {
			s.Script = &ScriptCheckpoint{Expired: r.Script.Expired}
		}
	}
	if r.Observation != nil {
		s.Observation = &Observation{}
	}
	if r.Spec.Usage != nil {
		s.Spec.Usage = &UsageTrigger{CooldownSeconds: r.Spec.Usage.CooldownSeconds}
	}
	if r.Learning != nil {
		s.Learning = &LearningBatch{ReadyAt: r.Learning.ReadyAt, Expires: r.Learning.Expires}
	}
	if r.Batch != nil {
		s.Batch = &EventBatch{ReadyAt: r.Batch.ReadyAt, Expires: r.Batch.Expires}
	}
	return s
}

var identifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,95}$`)

func Hash(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func (s Spec) Validate() error {
	if s.Retry != nil && (s.UsesAI() || s.Retry.MaxAttempts < 2 || s.Retry.MaxAttempts > 3 || s.Retry.DelaySeconds < 5 || s.Retry.DelaySeconds > 3600) {
		return errors.New("retries require a deterministic task, 2–3 attempts and 5–3600 seconds delay")
	}
	if err := s.Trigger.validate(); err != nil {
		return err
	}
	if s.Trigger != nil && (s.Kind != "task" && s.Kind != "script" || s.Schedule != nil || s.IntervalSeconds != 0) {
		return errors.New("events require a task or script with no calendar or interval")
	}
	if err := s.Alert.validate(); err != nil {
		return err
	}
	if s.Alert != nil && s.Kind != "monitor" && s.Kind != "native" && s.Kind != "script" {
		return errors.New("alert policy requires a monitor, native task or script")
	}
	for _, call := range s.Calls {
		if _, err := call.At(time.Now()); err != nil {
			return err
		}
	}
	if s.MaxTokens < 0 {
		return errors.New("invalid automation token limit")
	}
	if err := s.Schedule.validate(); err != nil {
		return err
	}
	if s.Schedule != nil && (s.IntervalSeconds != 0 || s.Kind == "learning") {
		return errors.New("calendar conflicts with interval or usage trigger")
	}
	if !identifier.MatchString(s.ID) || s.Actor == "" || s.Project == "" || s.IntervalSeconds < 0 || s.IntervalSeconds > 31*86400 || s.IntervalSeconds > 0 && s.IntervalSeconds < 60 || s.TimeoutSeconds < 0 || int64(s.TimeoutSeconds) > math.MaxInt64/int64(time.Second) || s.MaxRounds < 0 || len(s.Instruction) > capacity.StateBytes.Limit() || len(s.Calls) > 32 || len(s.Checks) > 32 {
		return errors.New("invalid automation configuration")
	}
	if err := s.validatePersonal(); err != nil {
		return err
	}
	switch s.Kind {
	case "personal":
	case "script":
		if s.Retry != nil && s.Script != nil && s.Script.HasExternalEffects() {
			return errors.New("script retries require read-only deterministic capabilities")
		}
		if err := s.Script.validate(); err != nil {
			return err
		}
		if s.Output != "" || len(s.Calls) > 0 || len(s.Checks) > 0 || s.TimeoutSeconds < 1 {
			return errors.New("script requires an explicit timeout and uses only capability calls")
		}
	case "drive-sync":
		if err := s.Drive.validate(); err != nil {
			return err
		}
		if s.UsesAI() || s.Output != "" || len(s.Calls) > 0 || len(s.Checks) > 0 {
			return errors.New("Drive sync uses deterministic source extraction")
		}
	case "native":
		if err := s.Native.validate(); err != nil {
			return err
		}
		if s.AIConnection != "" || len(s.Calls) > 0 || len(s.Checks) > 0 {
			return errors.New("native tasks do not run models or external calls")
		}
		if strings.HasSuffix(s.Native.Task, "-health") && s.Output != "" {
			return errors.New("health reports are retained with runs; output notes are only supported for maintenance tasks")
		}
	case "checks":
		if len(s.Checks) == 0 || s.AIConnection != "" && s.Repair == nil {
			return errors.New("check suites require independent checks; a model requires repair policy")
		}
	case "task":
		if s.Instruction == "" || s.Output == "" {
			return errors.New("AI task requires provider, instruction and output")
		}
	case "learning":
		if s.Learning == nil || len(s.Connections) == 0 || s.IntervalSeconds != 0 || s.Recipe != "" || s.Output != "" || len(s.Checks) != 0 || len(s.Calls) != 0 {
			return errors.New("learning requires a provider, connections and learning policy; no fixed recipe or schedule")
		}
		if err := s.Learning.validate(); err != nil {
			return err
		}
	case "digest":
		if len(s.Calls) == 0 || s.Output == "" {
			return errors.New("digest requires calls and output")
		}
	case "recipe":
		if s.Recipe == "" || len(s.Checks) == 0 {
			return errors.New("recipe requires a note and independent checks")
		}
	case "monitor":
		if s.Condition == nil || len(s.Calls) != 1 || s.Output != "" {
			return errors.New("monitor requires one call and one condition")
		}
	case "assistant":
		if s.Instruction == "" || s.Delivery == nil {
			return errors.New("assistant requires a task and chat destination")
		}
	default:
		return errors.New("unsupported automation kind")
	}
	if s.Learning != nil && s.Kind != "learning" {
		return errors.New("learning policy requires a learning job")
	}
	if s.UsesAI() && !s.ExternalProcessing {
		return errors.New("automation requires explicit AI processing consent")
	}
	if raw, err := json.Marshal(s); err != nil || len(raw) > capacity.StateBytes.Limit() {
		return errors.New("automation definition exceeds configured state capacity")
	}
	for _, project := range s.ReadProjects {
		if !identifier.MatchString(project) {
			return errors.New("invalid input notebook ID")
		}
	}
	for connection, ops := range s.Operations {
		if !identifier.MatchString(connection) || len(ops) == 0 || len(ops) > 128 {
			return errors.New("invalid operation allowlist")
		}
		for _, op := range ops {
			if !identifier.MatchString(op) {
				return errors.New("invalid operation name")
			}
		}
	}
	if s.Trigger != nil && s.Trigger.Kind == "repository.updated" && s.Trigger.Project != s.Project {
		found := false
		for _, project := range s.ReadProjects {
			found = found || project == s.Trigger.Project
		}
		if !found {
			return errors.New("repository trigger must be an allowed input notebook")
		}
	}
	if s.Native != nil && s.Kind != "native" {
		return errors.New("native configuration requires a native task")
	}
	if s.Drive != nil && s.Kind != "drive-sync" || s.Script != nil && s.Kind != "script" {
		return errors.New("task configuration does not match kind")
	}
	if s.NotifySlackUser != "" && (s.Kind != "digest" && s.Kind != "monitor" && s.Kind != "script" || !identifier.MatchString(s.NotifySlackUser)) {
		return errors.New("only digests, monitors and scripts can notify Slack")
	}
	if s.NotifyChannel != "" && (!identifier.MatchString(s.NotifyChannel) || s.NotifySlackUser != "" || s.Kind == "assistant" || s.Kind == "learning") {
		return errors.New("invalid shared notification destination")
	}
	if s.Script != nil && slices.Contains(s.Script.Capabilities, "messages.post") && s.NotifyChannel == "" {
		return errors.New("messages.post requires a shared notifyChannel")
	}
	if s.Condition != nil {
		c := s.Condition
		n := 0
		if len(c.Equals) > 0 {
			n++
			if !json.Valid(c.Equals) {
				return errors.New("invalid condition value")
			}
		}
		if c.GreaterThan != nil {
			n++
		}
		if c.LessThan != nil {
			n++
		}
		if s.Kind != "monitor" || n != 1 || c.Pointer != "" && !strings.HasPrefix(c.Pointer, "/") {
			return errors.New("monitor requires exactly one valid condition")
		}
	}
	if s.Usage != nil && (s.Kind != "recipe" || !s.Usage.Errors && s.Usage.SlowMS == 0 || s.Usage.SlowMS < 0 || s.Usage.CooldownSeconds < 60 || s.Usage.CooldownSeconds > 86400) {
		return errors.New("usage triggers require a recipe, errors/slowMs and a 60–86400 second cooldown")
	}
	for _, p := range []string{s.Recipe, s.Output} {
		if p != "" && (!filepath.IsLocal(p) || strings.Contains(p, "\\") || filepath.Ext(p) != ".md") {
			return errors.New("automation note paths must be local Markdown paths")
		}
	}
	for _, c := range s.Checks {
		if c.Name == "" || s.Kind == "recipe" && (c.Block == "" || c.Call.Operation != "query") || len(c.Assertions) == 0 && c.Reference == nil || c.MaxDurationMS < 0 || s.Kind != "recipe" && s.Kind != "checks" {
			return errors.New("recipe checks require named SQL examples and assertions")
		}
		if _, err := c.Call.At(time.Now()); err != nil {
			return err
		}
		if c.Reference != nil {
			if _, err := c.Reference.At(time.Now()); err != nil {
				return err
			}
		}
		for _, a := range c.Assertions {
			if a.Pointer != "" && !strings.HasPrefix(a.Pointer, "/") || !json.Valid(a.Equals) {
				return errors.New("invalid check assertion")
			}
		}
	}
	if err := s.validateRepair(); err != nil {
		return err
	}
	return nil
}
func (s Spec) ValidateConfigured() error {
	if err := s.Validate(); err != nil {
		return err
	}
	if s.Delivery != nil || s.Kind == "assistant" || s.Kind == "personal" {
		return errors.New("reserved automation kind or delivery")
	}
	if len(s.Name) > 160 || len(s.Description) > 2000 {
		return errors.New("automation label is too long")
	}
	return nil
}
func Load(file string) (Config, error) {
	var c Config
	if err := jsonfile.Load(file, int64(capacity.StateBytes.Limit()), &c); err != nil {
		return c, errors.New("invalid automation config")
	}
	return c, c.Validate()
}
func (c Config) Validate() error {
	if c.Version != 1 && c.Version != 2 {
		return errors.New("invalid automation config")
	}
	var e error
	seen := map[string]bool{}
	for _, s := range c.Jobs {
		if e = s.Validate(); e != nil {
			return e
		}
		if s.ValidateConfigured() != nil || seen[s.ID] {
			return errors.New("duplicate or reserved automation")
		}
		seen[s.ID] = true
	}
	return nil
}

// Open returns the run queue an instance's gateways share in db. Runs this
// gateway left running, as a previous queue in this process would, and runs of
// gateways that are gone are queued again; runs another live gateway claimed
// keep running there.
func Open(db *pg.DB) (*Queue, error) {
	if db == nil {
		return nil, errors.New("automations need the instance database")
	}
	q := &Queue{records: records{db: db}, active: map[string]context.CancelFunc{}, wake: make(chan struct{}, 1)}
	return q, q.Recover()
}

// Recover queues again the running runs whose gateway is gone, and this
// gateway's own runs that no worker here is running. With no such run it costs
// one scan and takes no cross-gateway lock.
func (q *Queue) Recover() error { return q.recover(0) }

// recover is Recover, deciding whether any run may need it from the last scan
// when that listed the runs within fresh.
func (q *Queue) recover(fresh time.Duration) error {
	q.mu.Lock()
	var runs []Run
	var err error
	if fresh > 0 && time.Since(q.scanned) < fresh {
		for _, s := range q.summaries {
			runs = append(runs, s.run)
		}
	} else {
		runs, err = q.scan(false)
	}
	orphan := false
	for _, v := range runs {
		orphan = orphan || v.State == "running" && q.active[v.ID] == nil
	}
	q.mu.Unlock()
	if err != nil || !orphan {
		return err
	}
	q.lock()
	defer q.unlock()
	if runs, err = q.scan(false); err != nil {
		return err
	}
	ctx, cancel := storageContext()
	defer cancel()
	for _, v := range runs {
		if v.State != "running" || q.active[v.ID] != nil {
			continue
		}
		r, err := q.read(v.ID)
		if err != nil {
			return err
		}
		if r.State != "running" {
			continue
		}
		if r.Claimant != q.records.db.Holder {
			gone, err := q.records.db.Gone(ctx, q.records.db.Pool, r.Claimant)
			if err != nil {
				return err
			}
			if !gone {
				continue
			}
		}
		r.State, r.Claimant = "queued", ""
		if err = q.write(&r); err != nil {
			return err
		}
	}
	return nil
}

// write stores r over the version it was read at and records the new one. A
// run another writer changed meanwhile is not written: write reports
// pg.ErrConflict.
func (q *Queue) write(r *Run) error {
	// All new checkpoints require the generation, preview and incident-aware
	// reader. Continue reading earlier versions for persisted production work.
	r.Version = 5
	if r.Spec.Kind == "assistant" {
		// Chat checkpoints include bounded file replies and source results. Older
		// workers must not ignore their client, audience or delivery policy.
		r.Version = 6
	}
	if r.Spec.Kind == "script" {
		r.Version = 7
		if r.Spec.Script != nil && r.Spec.Script.RetentionHours > 0 {
			r.Version = 8
		}
	}
	b, e := json.MarshalIndent(r, "", "  ")
	if e != nil {
		return e
	}
	if len(b) > capacity.StateBytes.Limit() {
		return capacity.StateBytes.Error()
	}
	version, e := q.records.write(r.ID+".json", b, r.stored)
	if e != nil {
		return e
	}
	r.stored = version
	// A checkpoint of running work changes nothing another worker may claim.
	q.announce = q.announce || r.State != "running"
	return nil
}
func (q *Queue) read(id string) (Run, error) {
	if len(id) != 64 || strings.Trim(id, "0123456789abcdef") != "" {
		return Run{}, os.ErrNotExist
	}
	b, version, e := q.records.read(id+".json", capacity.StateBytes.Limit())
	if e != nil {
		return Run{}, e
	}
	r, e := decodeRun(id, b)
	r.stored = version
	return r, e
}

// decodeRun checks a stored run as every reader of the queue does.
func decodeRun(id string, b []byte) (Run, error) {
	var r Run
	if json.Unmarshal(b, &r) != nil || r.Version < 1 || r.Version > 8 || r.ID != id || len(b) > capacity.StateBytes.Limit() || r.Version == 6 && r.Spec.Kind != "assistant" || r.Version >= 7 && r.Spec.Kind != "script" {
		return r, errors.New("invalid automation state")
	}
	// Terminal runs are historical snapshots, not current job definitions.
	// Only recoverable work must satisfy today's execution contract.
	if !terminal(r.State) {
		if err := r.Spec.Validate(); err != nil {
			return r, fmt.Errorf("invalid automation run %s: %w", id, err)
		}
	}
	return r, nil
}
func (q *Queue) list() ([]Run, error) { return q.scan(true) }

// whole returns the full record of a run a scheduling scan returned. What
// pools and policies weigh of a spec, as a personal binding, an instruction or
// a script's capabilities, is not in a summary. Caller holds mu.
func (q *Queue) whole(r Run) (Run, error) {
	if !r.summary {
		return r, nil
	}
	return q.read(r.ID)
}

// Caller holds mu. Public reads return full records; scheduling reads reuse
// only metadata and load a full record when actually selecting work.
func (q *Queue) scan(details bool) ([]Run, error) {
	stored, e := q.records.list(".json")
	if e != nil {
		return nil, e
	}
	out := []Run{}
	cache := make(map[string]runSummary, len(stored))
	for _, f := range stored {
		id := strings.TrimSuffix(f.name, ".json")
		prior, cached := q.summaries[id]
		r := prior.run
		if details || !cached || prior.version != f.version {
			r, e = q.read(id)
			if errors.Is(e, os.ErrNotExist) {
				// Reads take no cross-gateway lock, so another gateway may
				// have pruned the run since the listing.
				continue
			}
			if e != nil {
				return nil, e
			}
			prior.stopMessage = slackStop(r)
		}
		cache[id] = runSummary{version: r.stored, run: summarizeRun(r), stopMessage: prior.stopMessage}
		out = append(out, r)
	}
	q.summaries, q.scanned = cache, time.Now()
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out, nil
}

// List and Get only read, so they take no cross-gateway lock.
func (q *Queue) List() ([]Run, error) { q.mu.Lock(); defer q.mu.Unlock(); return q.list() }
func (q *Queue) Enqueue(spec Spec, key string) (Run, error) {
	if spec.Kind == "learning" || spec.Trigger != nil {
		return Run{}, errors.New("event jobs are queued automatically from observed input")
	}
	return q.enqueue(spec, key, nil, false)
}
func (q *Queue) Observe(spec Spec, observation Observation) (Run, error) {
	if spec.Usage == nil {
		return Run{}, errors.New("usage trigger is not configured")
	}
	return q.enqueue(spec, usageKey(spec, time.Now()), &observation, false)
}
func (q *Queue) EnqueuePreview(spec Spec, key string) (Run, error) {
	if spec.Kind == "learning" || spec.Trigger != nil {
		return Run{}, errors.New("event previews require captured evidence")
	}
	return q.enqueue(spec, key, nil, true)
}
func (q *Queue) Get(id string) (Run, error) { q.mu.Lock(); defer q.mu.Unlock(); return q.read(id) }
func (q *Queue) enqueue(spec Spec, key string, observation *Observation, preview bool) (Run, error) {
	q.lock()
	defer q.unlock()
	var zero Run
	if e := spec.Validate(); e != nil {
		return zero, e
	}
	id := Hash([]string{spec.ID, key})
	if old, e := q.read(id); e == nil {
		if slackStop(old) {
			runs, err := q.scan(false)
			if err != nil {
				return old, err
			}
			return old, q.cancelMessageRuns(old, runs)
		}
		return old, nil
	} else if !errors.Is(e, os.ErrNotExist) {
		return zero, e
	}
	runs, e := q.scan(false)
	if e != nil {
		return zero, e
	}
	for _, r := range runs {
		if r.Spec.ID == spec.ID && r.State == "action-required" {
			return zero, errors.New("reconcile and cancel action-required run " + r.ID + " before starting this job")
		}
	}
	if observation != nil {
		for _, r := range runs {
			if r.Spec.ID == spec.ID && (!terminal(r.State) || r.Observation != nil && time.Since(r.Created) < time.Duration(spec.Usage.CooldownSeconds)*time.Second) {
				return q.read(r.ID)
			}
		}
	}
	now := time.Now().UTC()
	if err := q.makeRoom(runs, now); err != nil {
		return zero, err
	}
	r := Run{DryRun: preview, Version: 5, ID: id, Spec: spec, State: "queued", Created: now, Updated: now, Observation: observation}
	if slackStop(r) || q.messageStopped(r, runs) {
		r.State = "cancelled"
	}
	if err := q.write(&r); errors.Is(err, pg.ErrConflict) {
		// Another gateway queued the same work first.
		return q.read(id)
	} else if err != nil {
		return r, err
	}
	if slackStop(r) {
		return r, q.cancelMessageRuns(r, runs)
	}
	return r, nil
}

// Caller holds mu. Every enqueue path uses the same bounded history cleanup.
func (q *Queue) makeRoom(runs []Run, now time.Time) error {
	remaining := len(runs)
	for _, r := range runs {
		if terminal(r.State) && !currentScheduledSlot(r, now) && now.Sub(r.Updated) > 7*24*time.Hour {
			if e := q.records.remove(r.ID+".json", r.stored); e == nil {
				remaining--
			} else if !errors.Is(e, pg.ErrConflict) {
				return e
			}
		}
	}
	// Keep active jobs, current scheduled slots, and the signed-event retry
	// window. Retention must not cause a completed interval to execute again.
	if remaining >= capacity.AutomationRuns.Limit() {
		for _, r := range runs {
			if terminal(r.State) && !currentScheduledSlot(r, now) && now.Sub(r.Updated) > 5*time.Minute {
				if e := q.records.remove(r.ID+".json", r.stored); e == nil {
					remaining--
				} else if !errors.Is(e, pg.ErrConflict) {
					return e
				}
				if remaining < capacity.AutomationRuns.Limit() {
					break
				}
			}
		}
	}
	if remaining >= capacity.AutomationRuns.Limit() {
		return capacity.AutomationRuns.Error()
	}
	return nil
}

func terminal(s string) bool { return s == "completed" || s == "failed" || s == "cancelled" }

// Unresolved reports a run that still keeps its job from being scheduled.
func (r Run) Unresolved() bool { return !terminal(r.State) }
func scheduledKey(s Spec, now time.Time) string {
	return fmt.Sprintf("scheduled-%d", now.Unix()/int64(s.IntervalSeconds))
}
func currentScheduledSlot(r Run, now time.Time) bool {
	key, _, due := r.Spec.Slot(now)
	return due && r.ID == Hash([]string{r.Spec.ID, key}) || r.Observation != nil && r.Spec.Usage != nil && now.Sub(r.Created) < time.Duration(r.Spec.Usage.CooldownSeconds)*time.Second
}
func usageKey(s Spec, now time.Time) string {
	return fmt.Sprintf("usage-%d", now.Unix()/int64(max(60, s.Usage.CooldownSeconds)))
}
func (q *Queue) Schedule(specs []Spec, now time.Time) error {
	q.mu.Lock()
	runs, e := q.scan(false)
	// A personal run is compared with its job by its whole spec.
	for i := 0; e == nil && i < len(runs); i++ {
		if runs[i].Spec.Kind == "personal" && !terminal(runs[i].State) {
			runs[i], e = q.whole(runs[i])
		}
	}
	q.mu.Unlock()
	if e != nil {
		return e
	}
	busy := map[string]bool{}
	personal := map[string]string{}
	for _, spec := range specs {
		if spec.Kind == "personal" {
			personal[spec.ID] = Hash(spec)
		}
	}
	for _, r := range runs {
		if !terminal(r.State) {
			if r.Spec.Kind == "personal" && personal[r.Spec.ID] != Hash(r.Spec) {
				if err := q.Cancel(r.ID); err != nil {
					return err
				}
				continue
			}
			busy[r.Spec.ID] = true
		}
	}
	for _, s := range specs {
		key, _, due := s.Slot(now)
		if !due || busy[s.ID] {
			continue
		}
		if _, e = q.Enqueue(s, key); e != nil {
			return e
		}
	}
	return nil
}
func (q *Queue) Cancel(id string) error {
	q.lock()
	defer q.unlock()
	return q.cancel(id)
}

func (q *Queue) cancel(id string) error {
	_, e := q.update(id, func(r *Run) bool {
		if terminal(r.State) {
			return false
		}
		if r.State != "action-required" && r.Script.Uncertain() {
			r.State = "action-required"
			r.Error = "Cancelled during a paid call or write; reconcile retained script steps before cancelling this run"
		} else {
			r.State = "cancelled"
		}
		r.Updated = time.Now().UTC()
		return true
	})
	if e != nil {
		return e
	}
	if cancel := q.active[id]; cancel != nil {
		cancel()
	}
	return nil
}

// update applies change to the stored run id and writes it when change asks
// to. Caller holds the queue lock. When another gateway wrote the run since it
// was read, as one can once the lock's connection dropped, update reads the
// run again and reapplies change rather than overwrite that write.
func (q *Queue) update(id string, change func(*Run) bool) (Run, error) {
	for attempt := 1; ; attempt++ {
		r, err := q.read(id)
		if err != nil || !change(&r) {
			return r, err
		}
		if err = q.write(&r); !errors.Is(err, pg.ErrConflict) || attempt == 3 {
			return r, err
		}
	}
}

// Definitions own configuration; queue records keep their immutable execution
// snapshot. Cancel superseded snapshots without deleting diagnostic history.
func (q *Queue) CancelSuperseded(job, hash string) error {
	q.lock()
	defer q.unlock()
	return q.cancelSuperseded(job, hash)
}

// UpdateIdleDefinition fences automatic package activation against worker claims.
// In-flight and unresolved effects retain their original package and evidence.
func (q *Queue) UpdateIdleDefinition(definitions *Definitions, spec Spec, revision string) (bool, error) {
	q.lock()
	defer q.unlock()
	runs, err := q.scan(false)
	if err != nil {
		return false, err
	}
	for _, run := range runs {
		if run.Spec.ID == spec.ID && (q.active[run.ID] != nil || run.State == "running" || run.State == "action-required") {
			return false, nil
		}
	}
	d, err := definitions.Save(spec, revision)
	if err != nil {
		return false, err
	}
	return true, q.cancelSuperseded(spec.ID, Hash(d.Spec))
}

func (q *Queue) cancelSuperseded(job, hash string) error {
	runs, err := q.list()
	if err != nil {
		return err
	}
	for _, r := range runs {
		if r.Spec.ID != job || terminal(r.State) || r.State == "action-required" || Hash(r.Spec) == hash {
			continue
		}
		if _, err = q.update(r.ID, func(r *Run) bool {
			if terminal(r.State) || r.State == "action-required" {
				return false
			}
			r.State = "cancelled"
			r.Error = "Automation definition changed"
			if r.Script.Uncertain() {
				r.State = "action-required"
				r.Error = "Automation definition changed; reconcile retained script steps before cancelling this run"
			}
			r.Updated = time.Now()
			return true
		}); err != nil {
			return err
		}
		if cancel := q.active[r.ID]; cancel != nil {
			cancel()
		}
	}
	return nil
}

// watch stops a claimed run's work once another gateway cancels it or takes it
// over, without waiting for its next checkpoint. Each second it reads only the
// record's version, and the record itself only after a write by someone other
// than this run's worker, whose last written version is in written.
func (q *Queue) watch(ctx context.Context, cancel context.CancelFunc, id string, written *atomic.Int64) {
	// The watcher reads outside critical sections, so it never sees a section's
	// lock failure.
	stored := records{db: q.records.db}
	seen := written.Load()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		version, err := stored.version(id + ".json")
		if err != nil || version == seen {
			continue
		}
		seen = version
		if version == written.Load() {
			continue
		}
		b, _, err := stored.read(id+".json", capacity.StateBytes.Limit())
		var current struct {
			State    string `json:"state"`
			Claimant string `json:"claimant"`
		}
		if err == nil && json.Unmarshal(b, &current) == nil && (current.State == "cancelled" || current.State == "action-required" || current.Claimant != q.records.db.Holder) {
			cancel()
			return
		}
	}
}

func (q *Queue) RunNext(ctx context.Context, fn func(context.Context, *Run, func() error) error) (bool, error) {
	return q.RunMatching(ctx, nil, fn)
}

// RunMatching claims the first ready queued run accept takes, runs fn on it
// and stores the outcome, reporting whether there was such a run.
func (q *Queue) RunMatching(ctx context.Context, accept func(Spec) bool, fn func(context.Context, *Run, func() error) error) (bool, error) {
	c, err := q.claim(ctx, accept)
	if c == nil {
		return false, err
	}
	return true, q.execute(c, fn)
}

// A claim is a run this gateway marked running, with the context its work
// runs in.
type claim struct {
	run    Run
	ctx    context.Context
	cancel context.CancelFunc
}

// readyAt is when a queued run may start: after its retry delay and once its
// learning or event batch is ready.
func (r *Run) readyAt() time.Time {
	at := r.RetryAt
	if r.Learning != nil && r.Learning.ReadyAt.After(at) {
		at = r.Learning.ReadyAt
	}
	if r.Batch != nil && r.Batch.ReadyAt.After(at) {
		at = r.Batch.ReadyAt
	}
	return at
}

// busyJobs returns the jobs whose queued runs wait: those with a run claimed
// on any gateway or awaiting reconciliation. Caller holds mu.
func (q *Queue) busyJobs(runs []Run) map[string]bool {
	busy := map[string]bool{}
	for _, v := range runs {
		if q.claimed(v) || v.State == "action-required" {
			busy[v.Spec.ID] = true
		}
	}
	return busy
}

// waiting scans the queue without the cross-gateway lock and reports, for each
// of accepts, whether there may be a run it takes to claim now, and when the
// earliest queued run that is not ready yet becomes ready, zero when none
// waits for time. A queued run of a busy job waits for that job's run instead,
// whose end announces itself. A nil accept asks nothing, as for a pool with no
// idle worker. Accepts weigh a ready run's whole record, read only while some
// accept has found no run yet.
func (q *Queue) waiting(accepts []func(Spec) bool, now time.Time) ([]bool, time.Time, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	runs, err := q.scan(false)
	if err != nil {
		return nil, time.Time{}, err
	}
	ready := make([]bool, len(accepts))
	var next time.Time
	busy := q.busyJobs(runs)
	for _, v := range runs {
		if v.State != "queued" || q.active[v.ID] != nil {
			continue
		}
		// Any claim first cancels a run that a later message stopped.
		stopped := q.messageStopped(v, runs)
		if !stopped && busy[v.Spec.ID] {
			continue
		}
		if at := v.readyAt(); !stopped && now.Before(at) {
			if next.IsZero() || at.Before(next) {
				next = at
			}
			continue
		}
		asked := false
		for i, accept := range accepts {
			ready[i] = ready[i] || accept != nil && stopped
			asked = asked || accept != nil && !ready[i]
		}
		if !asked {
			continue
		}
		r, err := q.whole(v)
		if errors.Is(err, os.ErrNotExist) {
			// Another gateway removed it since the scan.
			continue
		} else if err != nil {
			return nil, time.Time{}, err
		}
		if r.State != "queued" {
			continue
		}
		for i, accept := range accepts {
			ready[i] = ready[i] || accept != nil && accept(r.Spec)
		}
	}
	return ready, next, nil
}

// claim marks the first ready queued run accept takes as running on this
// gateway and returns it, or nil when there is none, after cancelling queued
// runs that a later message stopped. The claim is written over the version the
// run was chosen at, so when another gateway claimed or changed the run first,
// even past a dropped queue lock, claim fails with pg.ErrConflict instead of
// letting two gateways run it.
func (q *Queue) claim(ctx context.Context, accept func(Spec) bool) (*claim, error) {
	q.lock()
	defer q.unlock()
	runs, e := q.scan(false)
	if e != nil {
		return nil, e
	}
	busy := q.busyJobs(runs)
	now := time.Now()
	var r Run
	for _, v := range runs {
		if v.State == "queued" && q.messageStopped(v, runs) {
			if err := q.cancel(v.ID); err != nil {
				return nil, err
			}
			continue
		}
		if v.State != "queued" || now.Before(v.readyAt()) || q.active[v.ID] != nil || busy[v.Spec.ID] {
			continue
		}
		whole, err := q.whole(v)
		if err != nil {
			return nil, err
		}
		if whole.stored != v.stored || whole.State != "queued" {
			return nil, fmt.Errorf("automation run %s changed while it was claimed: %w", v.ID, pg.ErrConflict)
		}
		if accept == nil || accept(whole.Spec) {
			r = whole
			break
		}
	}
	if r.ID == "" {
		return nil, nil
	}
	r.State = "running"
	r.Claimant = q.records.db.Holder
	r.Attempts++
	r.Updated = time.Now().UTC()
	if e = q.write(&r); errors.Is(e, pg.ErrConflict) {
		return nil, fmt.Errorf("automation run %s was claimed on another gateway: %w", r.ID, e)
	} else if e != nil {
		return nil, e
	}
	ctx, cancel := context.WithCancel(ctx)
	q.active[r.ID] = cancel
	return &claim{run: r, ctx: ctx, cancel: cancel}, nil
}

// execute runs a claimed run's work and stores its outcome. A run cancelled
// meanwhile keeps its cancellation. A run another gateway changed otherwise,
// as one that took it over from a gateway it thought gone does, keeps that
// change: its checkpoints and outcome here fail with pg.ErrConflict. A claim
// whose context ended before its work began is queued again unrun.
func (q *Queue) execute(c *claim, fn func(context.Context, *Run, func() error) error) error {
	defer c.cancel()
	r, ctx := c.run, c.ctx
	var written atomic.Int64
	written.Store(r.stored)
	go q.watch(ctx, c.cancel, r.ID, &written)
	save := func() error {
		q.lock()
		defer q.unlock()
		r.Updated = time.Now().UTC()
		if err := q.write(&r); err != nil {
			return q.overwritten(r.ID, err)
		}
		written.Store(r.stored)
		return nil
	}
	err := ctx.Err()
	if err == nil {
		err = fn(ctx, &r, save)
	} else {
		// Unrun work is no attempt.
		r.Attempts--
	}
	q.lock()
	defer q.unlock()
	delete(q.active, r.ID)
	r.State = "completed"
	r.Error = ""
	r.Claimant = ""
	if err != nil {
		r.State = "failed"
		r.Error = err.Error()
		if len(r.Error) > 500 {
			r.Error = r.Error[:500]
		}
		if ctx.Err() != nil {
			r.State = "queued"
		} else if r.Script.Uncertain() {
			r.State = "action-required"
			r.Error = "Reconcile retained script steps before cancelling this run: " + r.Error
		} else if r.Spec.Retry != nil && !r.Spec.UsesAI() && r.Attempts < r.Spec.Retry.MaxAttempts && errors.Is(err, connectors.ErrUnavailable) && r.DeliveryState == "" && r.Answer == "" && (r.Publication == nil || r.Publication.Content == "") {
			r.State = "queued"
			r.RetryAt = time.Now().Add(time.Duration(r.Spec.Retry.DelaySeconds*r.Attempts) * time.Second)
			r.RetryErrors = append(r.RetryErrors, r.Error)
		}
	}
	if terminal(r.State) {
		r.Publication = nil
		r.Answer = ""
	}
	r.Updated = time.Now().UTC()
	if e := q.write(&r); e != nil {
		if e = q.overwritten(r.ID, e); !errors.Is(e, context.Canceled) {
			return e
		}
	}
	return nil
}

// overwritten explains a claimed run's failed write: context.Canceled when the
// run was cancelled meanwhile, so its work stops as after a local cancel, and
// otherwise the write's own error. Caller holds the queue lock.
func (q *Queue) overwritten(id string, err error) error {
	if !errors.Is(err, pg.ErrConflict) {
		return err
	}
	if current, e := q.read(id); e == nil && (current.State == "cancelled" || current.State == "action-required") {
		return context.Canceled
	}
	return fmt.Errorf("automation run %s changed on another gateway: %w", id, err)
}
