// Package citationchecks owns private citation receipts and admitted check lifetimes.
// Notebook content, current authority and provider policy remain at their owners.
package citationchecks

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"sync"
	"time"

	"forgetmenote/internal/decision"
	"forgetmenote/internal/knowledge"
)

type Item struct {
	Claim      string                       `json:"claim"`
	Status     string                       `json:"status"`
	Evidence   []knowledge.JudgmentEvidence `json:"evidence"`
	Confidence float64                      `json:"confidence,omitempty"`
}
type Check struct {
	Fingerprint string `json:"fingerprint,omitempty"`
	ID          string `json:"id"`
	Turn        string `json:"turn"`
	State       string `json:"state"`
	Checked     int    `json:"checked"`
	Total       int    `json:"total"`
	Items       []Item `json:"items"`
	Message     string `json:"message"`
	UpdatedAt   string `json:"updatedAt"`
}
type Record struct {
	Version int              `json:"version"`
	Owner   string           `json:"owner"`
	Project string           `json:"project"`
	Chat    string           `json:"chat"`
	Checks  map[string]Check `json:"checks"`
}
type work struct {
	user, chat, run, check string
	execute                func(context.Context, string)
	cancel                 context.CancelFunc
}

type Runtime struct {
	mu      sync.Mutex
	records *decision.Records
	ctx     context.Context
	cancel  context.CancelFunc
	reads   chan struct{}
	queue   []work
	active  map[string]work
	closed  bool
	wg      sync.WaitGroup
}

func New(parent context.Context, records *decision.Records) *Runtime {
	ctx, cancel := context.WithCancel(parent)
	return &Runtime{ctx: ctx, cancel: cancel, records: records, reads: make(chan struct{}, 1), active: map[string]work{}}
}
func RecordName(user, chat string) string {
	hash := sha256.Sum256([]byte(user + "\x00" + chat))
	return "citations/" + hex.EncodeToString(hash[:]) + ".json"
}
func taskKey(user, chat, run string) string { return user + "\x00" + chat + "\x00" + run }
func newRecord(user, project, chat string) Record {
	return Record{Version: 1, Owner: user, Project: project, Chat: chat, Checks: map[string]Check{}}
}
func validRecord(record Record, user, project, chat string) error {
	if record.Version != 1 || record.Owner != user || record.Project != project || record.Chat != chat || record.Checks == nil {
		return decision.ErrStorage
	}
	return nil
}
func (r *Runtime) read(user, project, chat string) (Record, error) {
	var record Record
	err := r.records.Read(RecordName(user, chat), &record)
	if errors.Is(err, os.ErrNotExist) {
		return newRecord(user, project, chat), nil
	}
	if err != nil {
		return record, decision.ErrStorage
	}
	return record, validRecord(record, user, project, chat)
}

// update changes the conversation's record while holding it, so checks saved
// or started through another gateway are kept, and a conversation forgotten
// meanwhile stays forgotten.
func (r *Runtime) update(user, project, chat string, change func(*Record) error) error {
	record := newRecord(user, project, chat)
	return r.records.Update(RecordName(user, chat), &record, func(bool) error {
		if err := validRecord(record, user, project, chat); err != nil {
			return err
		}
		return change(&record)
	})
}
func (r *Runtime) pending(key string) bool {
	if _, ok := r.active[key]; ok {
		return true
	}
	for _, task := range r.queue {
		if taskKey(task.user, task.chat, task.run) == key {
			return true
		}
	}
	return false
}

func (r *Runtime) Snapshot(user, project, chat, run string) (Record, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	record, err := r.read(user, project, chat)
	return record, r.pending(taskKey(user, chat, run)), err
}
func (r *Runtime) Save(user, project, chat, run string, check Check) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.ctx.Err() != nil {
		return context.Canceled
	}
	return r.update(user, project, chat, func(record *Record) error {
		current, ok := record.Checks[run]
		if !ok || current.ID != check.ID {
			return context.Canceled
		}
		check.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
		record.Checks[run] = check
		return nil
	})
}

// Start publishes pending identity before queuing. Only the bound in-memory
// callback retains session authority; restart requires a fresh explicit request.
func (r *Runtime) Start(user, project, chat, run string, explicit bool, execute func(context.Context, string)) error {
	if user == "" || project == "" || chat == "" || run == "" || execute == nil {
		return decision.ErrInvalid
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.ctx.Err() != nil {
		return decision.ErrDisabled
	}
	key := taskKey(user, chat, run)
	if r.pending(key) {
		return nil
	}
	if len(r.queue) >= 1000 {
		return decision.ErrLimit
	}
	var check Check
	err := r.update(user, project, chat, func(record *Record) error {
		if _, ok := record.Checks[run]; ok && !explicit {
			return nil
		}
		if len(record.Checks) >= 1000 {
			if _, ok := record.Checks[run]; !ok {
				return decision.ErrLimit
			}
		}
		var id [16]byte
		rand.Read(id[:])
		check = Check{ID: hex.EncodeToString(id[:]), State: "pending", Items: []Item{}, Message: "Queued to check clearly associated citations against their pinned sources.", UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
		record.Checks[run] = check
		return nil
	})
	if err != nil || check.ID == "" {
		return err
	}
	r.queue = append(r.queue, work{user: user, chat: chat, run: run, check: check.ID, execute: execute})
	r.launch()
	return nil
}

// Caller holds mu; callbacks always run after releasing it.
func (r *Runtime) launch() {
	if r.closed || r.ctx.Err() != nil {
		return
	}
	for len(r.active) < 2 && len(r.queue) > 0 {
		task := r.queue[0]
		r.queue[0] = work{}
		r.queue = r.queue[1:]
		ctx, cancel := context.WithTimeout(r.ctx, 60*time.Second)
		task.cancel = cancel
		key := taskKey(task.user, task.chat, task.run)
		r.active[key] = task
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			defer cancel()
			defer func() { r.mu.Lock(); delete(r.active, key); r.launch(); r.mu.Unlock() }()
			task.execute(ctx, task.check)
		}()
	}
}
func (r *Runtime) AdmitRead(ctx context.Context) (func(), error) {
	select {
	case r.reads <- struct{}{}:
		return func() { <-r.reads }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-r.ctx.Done():
		return nil, decision.ErrDisabled
	}
}
func (r *Runtime) ForgetConversation(user, chat string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	pending := r.queue[:0]
	for _, task := range r.queue {
		if task.user != user || task.chat != chat {
			pending = append(pending, task)
		}
	}
	clear(r.queue[len(pending):])
	r.queue = pending
	for _, task := range r.active {
		if task.user == user && task.chat == chat {
			task.cancel()
		}
	}
	return r.records.Remove(RecordName(user, chat))
}
func (r *Runtime) Active() int { r.mu.Lock(); defer r.mu.Unlock(); return len(r.active) }
func (r *Runtime) Close() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.closed = true
	r.cancel()
	clear(r.queue)
	r.queue = nil
	for _, task := range r.active {
		task.cancel()
	}
	r.mu.Unlock()
	r.wg.Wait()
}
