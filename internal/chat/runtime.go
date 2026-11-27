package chat

import (
	"context"
	"forgetmenote/internal/decision"
	"forgetmenote/internal/pg"
	"forgetmenote/internal/workspace"
	"log/slog"
	"sync"
	"time"
)

type Runtime struct {
	WebsiteCapture *WebsiteCapture
	configMu       sync.Mutex
	Judgments      *decision.Service
	JudgmentLedger *decision.Ledger
	// Fence cached transcript frames and explicit exports against deletion.
	ContentFences  map[string]*ContentFence
	SearchResearch func(context.Context, *Research, string, int) ([]ResearchResult, Usage, error)
	PrepareSource  func(context.Context, string, string, []byte) (workspace.SourceCapture, error)
	Streams        map[string]int
	StreamTotal    int
	Mu             sync.Mutex
	Store          *Store
	Config         *Config
	Research       *Research
	Providers      map[string]*Provider
	Active         map[string]context.CancelFunc
	Connections    map[string]int
	Closed         bool
	wg             sync.WaitGroup
	db             *pg.DB
	stopRecovery   context.CancelFunc
	recovering     sync.WaitGroup
}

// StartRecovery has one gateway interrupt, every 30 seconds, the runs of
// gateways that are gone, until Close.
func (c *Runtime) StartRecovery() {
	c.Mu.Lock()
	defer c.Mu.Unlock()
	if c.Closed || c.stopRecovery != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.stopRecovery = cancel
	c.recovering.Add(1)
	go func() {
		defer c.recovering.Done()
		c.db.Duty(ctx, "chat-recovery", time.Minute, func(ctx context.Context, _ *pg.Lease) {
			tick := time.NewTicker(30 * time.Second)
			defer tick.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
				}
				if err := c.Store.RecoverGone(); err != nil {
					slog.Error("chat recovery", "error", err)
				}
			}
		})
	}()
}

// OpenRuntime opens the instance's chat, with the paid-call ledger it
// configures, in db.
func OpenRuntime(db *pg.DB) (*Runtime, error) {
	store, err := OpenStore(db)
	if err != nil {
		return nil, err
	}
	return &Runtime{db: db, Store: store, Streams: map[string]int{}, Providers: map[string]*Provider{}, Active: map[string]context.CancelFunc{}, Connections: map[string]int{}, SearchResearch: func(ctx context.Context, research *Research, query string, count int) ([]ResearchResult, Usage, error) {
		return research.SearchUsage(ctx, query, count)
	}}, nil
}

// available and track participate in the owner's locked persistence/admission
// transaction; Finish releases capacity only after execution has drained.
func (c *Runtime) available(config *Config, connection Connection) bool {
	return config != nil && !c.Closed && len(c.Active) < config.Limits.MaxConcurrent && c.Connections[connection.ID] < connection.Limits.ConnectionConcurrent
}
func (c *Runtime) track(id, connection string, cancel context.CancelFunc) {
	c.Active[id] = cancel
	c.Connections[connection]++
	c.wg.Add(1)
}
func (c *Runtime) Finish(id, connection string) {
	c.Mu.Lock()
	delete(c.Active, id)
	c.Connections[connection]--
	c.Mu.Unlock()
	c.wg.Done()
}

// Wait joins already admitted runs. Close fences new admission before joining;
// callers using Wait independently must likewise arrange quiescent admission.
func (c *Runtime) Wait() { c.wg.Wait() }
func (c *Runtime) Close() {
	c.configMu.Lock()
	defer c.configMu.Unlock()
	c.Mu.Lock()
	if c.Closed {
		c.Mu.Unlock()
		return
	}
	c.Closed = true
	for _, cancel := range c.Active {
		cancel()
	}
	stopRecovery := c.stopRecovery
	c.Mu.Unlock()
	if stopRecovery != nil {
		stopRecovery()
	}
	c.recovering.Wait()
	c.Judgments.Close()
	c.wg.Wait()
	if c.JudgmentLedger != nil {
		c.JudgmentLedger.Close()
	}
	c.Store.Close()
}

// Each stream retains its fence while connected but holds a read lock only
// while sending a frame. A deleted flag retires already-loaded replay without
// another disk read per token. Unused fences disappear when the last request ends.
type ContentFence struct {
	sync.RWMutex
	Deleted bool
	refs    int
}

func (s *Runtime) RetainContent(user, id string) (*ContentFence, func()) {
	key := user + "/" + id
	s.Mu.Lock()
	if s.ContentFences == nil {
		s.ContentFences = map[string]*ContentFence{}
	}
	fence := s.ContentFences[key]
	if fence == nil {
		fence = &ContentFence{}
		s.ContentFences[key] = fence
	}
	fence.refs++
	s.Mu.Unlock()
	return fence, func() {
		s.Mu.Lock()
		defer s.Mu.Unlock()
		fence.refs--
		if fence.refs == 0 {
			delete(s.ContentFences, key)
		}
	}
}
