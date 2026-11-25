package chat

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"forgetmenote/internal/pg"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var ErrNotFound = errors.New("conversation not found")
var ErrConflict = errors.New("conversation changed; reload before retrying")
var ErrCapacity = errors.New("chat capacity reached; start another conversation or ask the operator to review limits")
var chatID = regexp.MustCompile(`^[a-f0-9]{32}$`)

const maxRecordBytes = 64 << 20
const maxEvents = 20000

type Options struct {
	Corpus       *Corpus  `json:"corpus,omitempty"`
	Connection   string   `json:"connection"`
	Model        string   `json:"model"`
	Mode         string   `json:"mode"`
	Actions      []string `json:"actions,omitempty"`
	Research     bool     `json:"research,omitempty"`
	Review       bool     `json:"review,omitempty"`
	BudgetTokens int      `json:"budgetTokens,omitempty"` // Legacy field; ignored for execution.
}
type Attachment struct {
	Kind         string `json:"kind"`
	Path         string `json:"path"`
	Content      string `json:"content,omitempty"`
	Revision     string `json:"revision,omitempty"`
	LiveRevision string `json:"liveRevision,omitempty"`
	Hash         string `json:"hash,omitempty"`
}
type Citation struct {
	Project     string `json:"project,omitempty"`
	Source      string `json:"source,omitempty"`
	StartLine   int    `json:"startLine,omitempty"`
	EndLine     int    `json:"endLine,omitempty"`
	StartByte   int    `json:"startByte,omitempty"`
	EndByte     int    `json:"endByte,omitempty"`
	Scope       string `json:"scope,omitempty"`
	CapturedAt  string `json:"capturedAt,omitempty"`
	PassageHash string `json:"passageHash,omitempty"`
	References  string `json:"references,omitempty"`
	ID          string `json:"id"`
	Path        string `json:"path,omitempty"`
	URL         string `json:"url"`
	Title       string `json:"title,omitempty"`
	Text        string `json:"text,omitempty"`
	Revision    string `json:"revision,omitempty"`
	Hash        string `json:"hash,omitempty"`
	Kind        string `json:"kind"`
}

// Provenance records whether all notebook inputs for an answer were tracked;
// it does not certify the answer's factual accuracy or full notebook coverage.
type AttachmentReference struct {
	TurnID       string `json:"turnId"`
	Kind         string `json:"kind"`
	Path         string `json:"path"`
	Revision     string `json:"revision,omitempty"`
	LiveRevision string `json:"liveRevision,omitempty"`
	Hash         string `json:"hash"`
	References   string `json:"references,omitempty"`
}

type ExternalCapture struct {
	URL        string `json:"url"`
	CapturedAt string `json:"capturedAt"`
	Hash       string `json:"hash"`
}

type HistoricalDependency struct {
	Path       string `json:"path"`
	Revision   string `json:"revision"`
	Hash       string `json:"hash"`
	References string `json:"references"`
}

type Provenance struct {
	HistoricalDependencies []HistoricalDependency `json:"historicalDependencies,omitempty"`
	CorpusID               string                 `json:"corpusId,omitempty"`
	// External captures are dated evidence, never committed dependencies or proof of current web freshness.
	ExternalSources      []ExternalCapture     `json:"externalSources,omitempty"`
	ExternalAllowance    string                `json:"externalAllowance,omitempty"`
	AttachmentsTracked   bool                  `json:"attachmentsTracked,omitempty"`
	AttachmentReferences []AttachmentReference `json:"attachmentReferences,omitempty"`
	// Dependencies include sources inherited through retained assistant history.
	Dependencies     map[string]string `json:"dependencies,omitempty"`
	SnapshotBound    bool              `json:"snapshotBound,omitempty"`
	InstructionsHash string            `json:"instructionsHash,omitempty"`
	Complete         bool              `json:"complete"`
	Revision         string            `json:"revision,omitempty"`
	Visibility       string            `json:"visibility,omitempty"`
}
type Turn struct {
	// API projection derived from Provider.ToolCalls; leave zero in durable turns
	// so records remain readable by earlier strict-decoding versions.
	Intermediate bool         `json:"intermediate,omitempty"`
	Provenance   Provenance   `json:"provenance"`
	Streaming    bool         `json:"streaming,omitempty"`
	ID           string       `json:"id"`
	RunID        string       `json:"runId,omitempty"`
	Role         string       `json:"role"`
	Content      string       `json:"content"`
	Attachments  []Attachment `json:"attachments,omitempty"`
	Citations    []Citation   `json:"citations,omitempty"`
	Provider     *Message     `json:"provider,omitempty"`
}
type Accounting struct {
	UncertainTokens  int64 `json:"uncertainTokens,omitempty"`
	InputTokens      int64 `json:"inputTokens"`
	OutputTokens     int64 `json:"outputTokens"`
	CachedTokens     int64 `json:"cachedTokens"`
	ReasoningTokens  int64 `json:"reasoningTokens"`
	CacheWriteTokens int64 `json:"cacheWriteTokens"`
	Estimated        bool  `json:"estimated"`
	Requests         int   `json:"requests"`
	Reserved         int64 `json:"reserved"` // Legacy unused allowance; excluded from usage totals.
}

func (a *Accounting) Add(u Usage) {
	a.InputTokens += u.InputTokens
	a.OutputTokens += u.OutputTokens
	a.CachedTokens += u.CachedTokens
	a.ReasoningTokens += u.ReasoningTokens
	a.CacheWriteTokens += u.CacheWriteTokens
	a.Estimated = a.Estimated || u.Estimated
	a.Requests++
}
func (a Accounting) Tokens() int64 { return a.InputTokens + a.OutputTokens + a.UncertainTokens }

type Operation struct {
	Citations          []Citation      `json:"citations,omitempty"`
	ProvenanceComplete bool            `json:"provenanceComplete,omitempty"`
	ToolCallID         string          `json:"toolCallId,omitempty"`
	Round              int             `json:"round,omitempty"`
	Approved           bool            `json:"approved,omitempty"`
	ID                 string          `json:"id"`
	Name               string          `json:"name"`
	Arguments          json.RawMessage `json:"arguments"`
	Hash               string          `json:"hash"`
	State              string          `json:"state"`
	Result             json.RawMessage `json:"result,omitempty"`
	Error              string          `json:"error,omitempty"`
	Confirmation       string          `json:"confirmation,omitempty"`
}
type ReviewNote struct {
	Sources    []Citation `json:"sources,omitempty"`
	Provenance Provenance `json:"provenance"`
	Cursor     int        `json:"cursor"`
	Total      int        `json:"total"`
	Path       string     `json:"path"`
	Revision   string     `json:"revision"`
	Hash       string     `json:"hash"`
	State      string     `json:"state"`
	Summary    string     `json:"summary,omitempty"`
	References string     `json:"references,omitempty"`
}
type Coverage struct {
	HistoryLimited bool         `json:"historyLimited,omitempty"`
	CorpusID       string       `json:"corpusId,omitempty"`
	Finalizing     bool         `json:"finalizing,omitempty"`
	InputHash      string       `json:"inputHash,omitempty"`
	BatchPath      string       `json:"batchPath,omitempty"`
	BatchCursor    int          `json:"batchCursor,omitempty"`
	Visibility     string       `json:"visibility"`
	Model          string       `json:"model"`
	Revision       string       `json:"revision"`
	Purpose        string       `json:"purpose"`
	Notes          []ReviewNote `json:"notes"`
	Complete       bool         `json:"complete"`
}
type Run struct {
	PersonalConsent  string                `json:"personalConsent,omitempty"`
	ReviewAllowance  uint64                `json:"reviewAllowance,omitempty"`
	Provenance       Provenance            `json:"provenance"`
	UsageDays        map[string]Accounting `json:"usageDays,omitempty"`
	ReservationDay   string                `json:"reservationDay,omitempty"`
	InFlightDay      string                `json:"inFlightDay,omitempty"`
	RoundLimit       int                   `json:"roundLimit,omitempty"`
	ToolLimit        int                   `json:"toolLimit,omitempty"`
	InFlightTokens   int64                 `json:"inFlightTokens,omitempty"`
	ToolNames        []string              `json:"toolNames,omitempty"`
	Citations        []Citation            `json:"citations,omitempty"`
	ResearchSearches int                   `json:"researchSearches,omitempty"`
	ResearchFetches  int                   `json:"researchFetches,omitempty"`
	ID               string                `json:"id"`
	SubmissionID     string                `json:"submissionId"`
	RequestHash      string                `json:"requestHash"`
	State            string                `json:"state"`
	Options          Options               `json:"options"`
	Epoch            uint64                `json:"epoch"`
	CreatedAt        string                `json:"createdAt"`
	UpdatedAt        string                `json:"updatedAt"`
	Error            string                `json:"error,omitempty"`
	CancelRequested  bool                  `json:"cancelRequested,omitempty"`
	Rounds           int                   `json:"rounds"`
	ToolCount        int                   `json:"toolCount"`
	Usage            Accounting            `json:"usage"`
	Operations       []Operation           `json:"operations,omitempty"`
	Review           *Coverage             `json:"review,omitempty"`
	Messages         []Message             `json:"messages,omitempty"`
	// Holder is the gateway executing the run. While it is present, no other
	// gateway interrupts the run.
	Holder string `json:"holder,omitempty"`
}
type Memory struct {
	Provenance  Provenance        `json:"provenance"`
	Text        string            `json:"text"`
	ThroughTurn int               `json:"throughTurn"`
	Revisions   map[string]string `json:"revisions,omitempty"`
}
type EventRecord struct {
	Sequence int64           `json:"sequence"`
	Run      string          `json:"run"`
	Type     string          `json:"type"`
	Data     json.RawMessage `json:"data"`
}
type Conversation struct {
	Unavailable bool          `json:"unavailable,omitempty"`
	Error       string        `json:"error,omitempty"`
	Version     int           `json:"version"`
	ID          string        `json:"id"`
	Owner       string        `json:"owner"`
	Project     string        `json:"project"`
	Title       string        `json:"title"`
	Options     Options       `json:"options"`
	CreatedAt   string        `json:"createdAt"`
	UpdatedAt   string        `json:"updatedAt"`
	Turns       []Turn        `json:"turns"`
	Runs        []Run         `json:"runs"`
	Events      []EventRecord `json:"events"`
	Memory      Memory        `json:"memory"`
}

func (c *Conversation) AddEvent(run, kind string, data any) {
	b, _ := json.Marshal(data)
	c.Events = append(c.Events, EventRecord{Sequence: int64(len(c.Events) + 1), Run: run, Type: kind, Data: b})
}
func (c *Conversation) Run(id string) *Run {
	for i := range c.Runs {
		if c.Runs[i].ID == id {
			return &c.Runs[i]
		}
	}
	return nil
}
func (c *Conversation) Usage() Accounting {
	var a Accounting
	for _, r := range c.Runs {
		a.InputTokens += r.Usage.InputTokens
		a.UncertainTokens += r.Usage.UncertainTokens
		a.OutputTokens += r.Usage.OutputTokens
		a.CachedTokens += r.Usage.CachedTokens
		a.ReasoningTokens += r.Usage.ReasoningTokens
		a.CacheWriteTokens += r.Usage.CacheWriteTokens
		a.Estimated = a.Estimated || r.Usage.Estimated
		a.Requests += r.Usage.Requests
	}
	return a
}
func NewID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
func Hash(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func Now() string          { return time.Now().UTC().Format(time.RFC3339Nano) }
func Active(state string) bool {
	return state == "queued" || state == "running" || state == "awaiting_confirmation"
}

type Store struct {
	limits           storageCaps
	connectionLimits map[string]storageCaps
	mu               sync.Mutex
	db               *pg.DB
	closed           bool
}

// storageContext bounds one conversation read or write; records can reach
// maxRecordBytes.
func storageContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 30*time.Second)
}

// activeRuns reports whether a conversation has an unfinished run, which
// admission and paused-run reconciliation look up by the row's active flag.
func activeRuns(c Conversation) bool {
	if deletedProject(c.Project) {
		return false
	}
	for _, r := range c.Runs {
		if Active(r.State) {
			return true
		}
	}
	return false
}

// anyActive reports whether any of the user's conversations, on any gateway,
// has an unfinished run.
func (s *Store) anyActive(user string) (bool, error) {
	ctx, cancel := storageContext()
	defer cancel()
	var running bool
	err := s.db.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM chat_conversations WHERE org = $1 AND account = $2 AND active)`, s.db.Org, Hash([]byte(user))).Scan(&running)
	return running, err
}

// Paused-run reconciliation needs only unfinished conversations, never the
// completed transcript archive. Returned records are detached validated reads.
func (s *Store) activeConversations(user string) ([]Conversation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errors.New("chat store closed")
	}
	ctx, cancel := storageContext()
	defer cancel()
	rows, err := s.db.Pool.Query(ctx, `SELECT id FROM chat_conversations WHERE org = $1 AND account = $2 AND active ORDER BY id`, s.db.Org, Hash([]byte(user)))
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	result := make([]Conversation, 0, len(ids))
	for _, id := range ids {
		record, _, _, err := s.fetch(user, id)
		if err != nil {
			return nil, err
		}
		var meta struct{ Project string }
		if json.Unmarshal(record, &meta) != nil {
			return nil, errors.New("chat record is unreadable; restore a full backup")
		}
		c, err := s.load(user, meta.Project, id)
		if err != nil {
			return nil, err
		}
		if activeRuns(c) {
			result = append(result, c)
		}
	}
	return result, nil
}

// OpenStore opens the instance's conversations and interrupts the unfinished
// runs of this gateway's previous store and of gateways that are gone. Runs
// another live gateway executes keep running there.
func OpenStore(db *pg.DB) (*Store, error) {
	s := &Store{db: db, limits: capsFrom(Limits{})}
	if err := s.recover(true); err != nil {
		return nil, err
	}
	return s, nil
}

// RecoverGone interrupts the unfinished runs of gateways that are gone.
func (s *Store) RecoverGone() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recover(false)
}

// elsewhere reports a run that another live gateway executes, or, when not
// opening, that this gateway executes.
func (s *Store) elsewhere(ctx context.Context, r Run, opening bool) bool {
	if r.Holder == s.db.Holder {
		return !opening
	}
	if r.Holder == "" {
		return false
	}
	gone, err := s.db.Gone(ctx, s.db.Pool, r.Holder)
	return err != nil || !gone
}
func (s *Store) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
}

// fetch returns a conversation's stored bytes, its recovery overlay and its
// row version, or ErrNotFound.
func (s *Store) fetch(user, id string) (record, recovery []byte, version int64, err error) {
	ctx, cancel := storageContext()
	defer cancel()
	err = s.db.Pool.QueryRow(ctx, `SELECT version, record, recovery FROM chat_conversations WHERE org = $1 AND account = $2 AND id = $3`, s.db.Org, Hash([]byte(user)), id).Scan(&version, &record, &recovery)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, 0, ErrNotFound
	}
	if err != nil {
		return nil, nil, 0, err
	}
	if len(record) > s.readCapacity() {
		return nil, nil, 0, errors.New("chat record exceeds capacity; restore a full backup")
	}
	return record, recovery, version, nil
}
func (s *Store) load(user, project, id string) (Conversation, error) {
	c, _, err := s.loadVersion(user, project, id)
	return c, err
}

// loadVersion returns a conversation with the row version a write must find.
func (s *Store) loadVersion(user, project, id string) (Conversation, int64, error) {
	var c Conversation
	if s.closed {
		return c, 0, errors.New("chat store closed")
	}
	if user == "" || project == "" || !chatID.MatchString(id) {
		return c, 0, ErrNotFound
	}
	record, recovery, version, err := s.fetch(user, id)
	if err != nil {
		return c, 0, err
	}
	c, err = s.decode(user, project, id, record, recovery)
	return c, version, err
}
func (s *Store) decode(user, project, id string, record, recovery []byte) (Conversation, error) {
	var c Conversation
	d := json.NewDecoder(bytes.NewReader(record))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return c, errors.New("chat record is unreadable; restore a full backup")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return c, errors.New("chat record has trailing data")
	}
	if (c.Version != 1 && c.Version != 2) || c.ID != id || c.Owner != user || c.Project != project {
		return Conversation{}, ErrNotFound
	}
	for i, e := range c.Events {
		if e.Sequence != int64(i+1) {
			return c, errors.New("chat event sequence is corrupt")
		}
	}
	if !deletedProject(c.Project) {
		if err := s.applyRecovery(&c, record, recovery); err != nil {
			return c, err
		}
	}
	return c, nil
}

// persistRecord writes c over the row version its caller loaded; version 0
// creates it. A conversation another gateway wrote meanwhile is ErrConflict.
func (s *Store) persist(c Conversation, version int64) error {
	return s.persistRecord(c, version, false)
}
func (s *Store) persistReserved(c Conversation, version int64) error {
	return s.persistRecord(c, version, true)
}
func (s *Store) persistRecord(c Conversation, version int64, reserved bool) error {
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	limit := s.caps(c.Options.Connection)
	if (!reserved && !s.fits(c, b)) || len(b) > limit.Bytes || len(c.Events) > limit.Events {
		return ErrCapacity
	}
	ctx, cancel := storageContext()
	defer cancel()
	account := Hash([]byte(c.Owner))
	var tag pgconn.CommandTag
	if version == 0 {
		tag, err = s.db.Pool.Exec(ctx, `INSERT INTO chat_conversations (org, account, id, version, active, record) VALUES ($1, $2, $3, 1, $4, $5)
			ON CONFLICT DO NOTHING`, s.db.Org, account, c.ID, activeRuns(c), b)
	} else {
		// A new record supersedes any recovery overlay bound to the old bytes.
		tag, err = s.db.Pool.Exec(ctx, `UPDATE chat_conversations SET version = version + 1, active = $5, record = $6, recovery = NULL
			WHERE org = $1 AND account = $2 AND id = $3 AND version = $4`, s.db.Org, account, c.ID, version, activeRuns(c), b)
	}
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrConflict
	}
	return nil
}
func (s *Store) Create(user, project string, opts Options) (Conversation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.create(user, project, NewID(), opts)
}

// OpenBound gives an external conversation a stable identity within the same
// private store, capacity limits, deletion and backup lifecycle as browser chat.
func (s *Store) OpenBound(user, project, binding string, opts Options) (Conversation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if binding == "" || len(binding) > 1024 {
		return Conversation{}, ErrNotFound
	}
	id := Hash([]byte("external\x00" + project + "\x00" + binding))[:32]
	if c, err := s.load(user, project, id); err == nil {
		return c, nil
	} else if !errors.Is(err, ErrNotFound) {
		return Conversation{}, err
	}
	return s.create(user, project, id, opts)
}
func (s *Store) create(user, project, id string, opts Options) (Conversation, error) {
	if s.closed {
		return Conversation{}, errors.New("chat store closed")
	}
	if user == "" || project == "" || deletedProject(project) || len(user) > 512 || len(project) > 128 {
		return Conversation{}, ErrNotFound
	}
	if opts.Mode == "" {
		opts.Mode = "ask"
	}
	if opts.Mode != "ask" && opts.Mode != "suggest" && opts.Mode != "edit" {
		return Conversation{}, errors.New("choose Ask, Suggest or Edit")
	}
	all, err := s.list(user, "")
	if err != nil {
		return Conversation{}, err
	}
	count := 0
	for _, other := range all {
		if !deletedProject(other.Project) {
			count++
		}
	}
	if count >= s.caps(opts.Connection).Count {
		return Conversation{}, ErrCapacity
	}
	now := Now()
	c := Conversation{Version: 2, ID: id, Owner: user, Project: project, Title: "New conversation", Options: opts, CreatedAt: now, UpdatedAt: now, Turns: []Turn{}, Runs: []Run{}, Events: []EventRecord{}}
	err = s.persist(c, 0)
	return c, err
}
func (s *Store) Read(user, project, id string) (Conversation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load(user, project, id)
}
func (s *Store) Update(user, project, id string, fn func(*Conversation) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, revision, err := s.loadVersion(user, project, id)
	if err != nil {
		return err
	}
	version := c.Version
	prior := map[string]Run{}
	for _, r := range c.Runs {
		prior[r.ID] = r
	}
	if err = fn(&c); err != nil {
		return err
	}
	for i := range c.Runs {
		r := &c.Runs[i]
		before, ok := prior[r.ID]
		if !ok {
			ensureUsageDays(r)
			continue
		}
		if err = trackUsage(before, r); err != nil {
			return err
		}
	}
	if c.Owner != user || c.Project != project || c.ID != id || c.Version != version {
		return errors.New("conversation identity is immutable")
	}
	c.UpdatedAt = Now()
	return s.persist(c, revision)
}
func (s *Store) list(user, project string) ([]Conversation, error) {
	result := []Conversation{}
	ctx, cancel := storageContext()
	defer cancel()
	rows, err := s.db.Pool.Query(ctx, `SELECT id, record, recovery FROM chat_conversations WHERE org = $1 AND account = $2 ORDER BY id`, s.db.Org, Hash([]byte(user)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var record, recovery []byte
		if err := rows.Scan(&id, &record, &recovery); err != nil {
			return nil, err
		}
		placeholder := Conversation{ID: id, Owner: user, Title: "Conversation needs restoration", Unavailable: true, Error: "Conversation is damaged; restore its state from a complete backup. Usage from this record is unknown."}
		appendDamaged := func() {
			if project == "" || placeholder.Project == project {
				result = append(result, placeholder)
			}
		}
		if !chatID.MatchString(id) || len(record) > s.readCapacity() {
			appendDamaged()
			continue
		}
		var meta struct{ Owner, ID, Project string }
		if json.Unmarshal(record, &meta) != nil || meta.Owner != user || meta.ID != id {
			appendDamaged()
			continue
		}
		placeholder.Project = meta.Project
		if project != "" && meta.Project != project {
			continue
		}
		c, err := s.decode(user, meta.Project, id, record, recovery)
		if err != nil {
			appendDamaged()
			continue
		}
		summary := Conversation{Version: c.Version, ID: c.ID, Owner: c.Owner, Project: c.Project, Title: c.Title, Options: c.Options, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt}
		for _, r := range c.Runs {
			summary.Runs = append(summary.Runs, Run{ID: r.ID, State: r.State, Epoch: r.Epoch, CreatedAt: r.CreatedAt, Usage: r.Usage, Options: r.Options, UsageDays: r.UsageDays, ReservationDay: r.ReservationDay, InFlightDay: r.InFlightDay})
		}
		result = append(result, summary)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(result, func(i, j int) bool { return result[i].UpdatedAt > result[j].UpdatedAt })
	return result, nil
}

func (s *Store) List(user, project string) ([]Conversation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := s.list(user, project)
	if err != nil {
		return nil, err
	}
	visible := all[:0]
	for _, c := range all {
		if !deletedProject(c.Project) {
			visible = append(visible, c)
		}
	}
	return visible, nil
}

// Start atomically checks submission replay and active-run concurrency.
func (s *Store) Start(user, project, id string, run Run, turn Turn) (Conversation, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, revision, err := s.loadVersion(user, project, id)
	if err != nil {
		return c, false, err
	}
	for _, r := range c.Runs {
		if r.SubmissionID == run.SubmissionID {
			if r.RequestHash != run.RequestHash {
				return c, false, ErrConflict
			}
			return c, true, nil
		}
	}
	if err := run.Options.Corpus.Validate(); err != nil {
		return c, false, err
	}
	running, err := s.anyActive(user)
	if err != nil {
		return c, false, err
	}
	if running {
		return c, false, errors.New("a conversation is already running; stop or finish it first")
	}
	run.Options.BudgetTokens = 0
	run.Usage.Reserved = 0
	run.Holder = s.db.Holder
	ensureUsageDays(&run)
	run.ReservationDay = ""
	c.Version = 2
	c.Runs = append(c.Runs, run)
	c.Turns = append(c.Turns, turn)
	c.Options = run.Options
	if c.Title == "New conversation" {
		r := []rune(strings.Join(strings.Fields(turn.Content), " "))
		if len(r) > 70 {
			r = r[:70]
		}
		c.Title = string(r)
	}
	c.AddEvent(run.ID, "user", turn)
	c.AddEvent(run.ID, "state", map[string]string{"state": run.State})
	c.UpdatedAt = Now()
	err = s.persist(c, revision)
	return c, false, err
}
func (s *Store) recover(opening bool) error {
	// Only records with unfinished runs, unsettled tokens or a recovery overlay
	// need recovery; the completed archive is never read here.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	rows, err := s.db.Pool.Query(ctx, `SELECT account, id FROM chat_conversations
		WHERE org = $1 AND (active OR recovery IS NOT NULL OR position('"inFlightTokens":'::bytea IN record) > 0) ORDER BY account, id`, s.db.Org)
	if err != nil {
		return err
	}
	type candidate struct{ account, id string }
	candidates, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (candidate, error) {
		var c candidate
		return c, row.Scan(&c.account, &c.id)
	})
	if err != nil {
		return err
	}
	for _, candidate := range candidates {
		var record []byte
		if err := s.db.Pool.QueryRow(ctx, `SELECT record FROM chat_conversations WHERE org = $1 AND account = $2 AND id = $3`, s.db.Org, candidate.account, candidate.id).Scan(&record); err != nil {
			return err
		}
		var c Conversation
		if !chatID.MatchString(candidate.id) || len(record) > s.readCapacity() || json.Unmarshal(record, &c) != nil || Hash([]byte(c.Owner)) != candidate.account {
			continue
		}
		loaded, revision, err := s.loadVersion(c.Owner, c.Project, candidate.id)
		if err != nil {
			continue
		}
		if deletedProject(loaded.Project) {
			if err := s.cleanDeleted(loaded); err != nil {
				return err
			}
			continue
		}
		changed := false
		for i := range loaded.Runs {
			r := &loaded.Runs[i]
			before := *r
			consumed := false
			if r.State == "awaiting_confirmation" {
				for _, op := range r.Operations {
					if op.State == "running" && op.Confirmation == "" {
						consumed = true
					}
				}
			}
			if (r.State == "running" || r.State == "queued" || consumed || r.InFlightTokens > 0) && !s.elsewhere(ctx, *r, opening) {
				if consumed {
					for j := range r.Operations {
						o := &r.Operations[j]
						if o.State == "running" && o.Confirmation == "" && !o.Approved {
							o.State = "completed"
							o.Result = json.RawMessage(`{"message":"The user declined this action."}`)
							r.Messages = append(r.Messages, Message{Role: "tool", ToolCallID: o.ToolCallID, Content: string(o.Result)})
							loaded.AddEvent(r.ID, "tool_result", *o)
						}
					}
				}
				r.State = "interrupted"
				settleUnresolved(r)
				if err = trackUsage(before, r); err != nil {
					return err
				}
				r.InFlightDay = ""
				r.Usage.Reserved = 0
				r.Error = "The server restarted. Review progress and continue explicitly."
				r.UpdatedAt = Now()
				loaded.AddEvent(r.ID, "state", map[string]string{"state": r.State, "message": r.Error})
				changed = true
			}
		}
		if changed {
			if err = s.persist(loaded, revision); err != nil {
				if errors.Is(err, ErrCapacity) {
					if err = s.recoverFull(loaded, revision); err == nil {
						continue
					}
				}
				return fmt.Errorf("recover interrupted chat: %w", err)
			}
		}
	}
	return nil
}

// ReadVersion atomically pairs a durable snapshot with its row version. SSE
// polls the cheap version between authorization checks, not the complete
// transcript, so a stream on any gateway sees another gateway's writes.
func (s *Store) ReadVersion(user, project, id string) (Conversation, uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, version, err := s.loadVersion(user, project, id)
	return c, uint64(version), err
}

// Generation returns the conversation's row version, or 0 when it cannot be
// read, which no snapshot has.
func (s *Store) Generation(user, id string) uint64 {
	ctx, cancel := storageContext()
	defer cancel()
	var version int64
	if s.db.Pool.QueryRow(ctx, `SELECT version FROM chat_conversations WHERE org = $1 AND account = $2 AND id = $3`, s.db.Org, Hash([]byte(user)), id).Scan(&version) != nil {
		return 0
	}
	return uint64(version)
}

type UsageSummary struct {
	Total                    Accounting `json:"total"`
	UnavailableConversations int        `json:"unavailableConversations"`
}

// PersonalUsage exposes only the stable owner's totals, never conversation
// identities, source content, or another user's accounting.
func (s *Store) PersonalUsage(user string) (UsageSummary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := s.list(user, "")
	if err != nil {
		return UsageSummary{}, err
	}
	summary := UsageSummary{}
	aggregate := Conversation{}
	for _, c := range all {
		if c.Unavailable {
			summary.UnavailableConversations++
			continue
		}
		aggregate.Runs = append(aggregate.Runs, c.Runs...)
	}
	summary.Total = aggregate.Usage()
	return summary, nil
}
