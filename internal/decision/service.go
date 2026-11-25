package decision

import (
	"context"
	"sync"
	"time"
)

type Evaluation struct {
	Scope      Scope
	Request    Request
	LocalOnly  bool
	Background bool
	// Check revalidates actor, permission, source classification and exact evidence
	// revision. It runs before every outbound attempt and before returning results.
	Check func(context.Context) error
}
type Service struct {
	provider                               provider
	config                                 *Config
	ledger                                 *Ledger
	mu                                     sync.Mutex
	active, background, waitingInteractive int
	changed                                chan struct{}
	ctx                                    context.Context
	cancel                                 context.CancelFunc
	closed                                 bool
	wg                                     sync.WaitGroup
}

// A provider owns its wire format, validation and transport. The service keeps
// authorization checks, concurrency, retries and durable accounting shared.
type provider interface {
	prepare(Request) ([]byte, int64, error)
	call(context.Context, []byte, Request) (Result, bool, error)
	close()
}

func New(c Config, ledger *Ledger, localOnly bool) (*Service, error) {
	if !c.Enabled {
		return nil, ErrDisabled
	}
	if err := c.Validate(localOnly); err != nil {
		return nil, err
	}
	client, err := newTypeSafeClient(&c)
	if err != nil {
		return nil, err
	}
	if ledger == nil {
		return nil, ErrStorage
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Service{provider: client, config: &c, ledger: ledger, changed: make(chan struct{}), ctx: ctx, cancel: cancel}, nil
}
func (s *Service) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.closed = true
	s.cancel()
	s.signal()
	s.mu.Unlock()
	s.wg.Wait()
	s.provider.close()
}
func (s *Service) signal() { close(s.changed); s.changed = make(chan struct{}) }
func (s *Service) acquire(ctx context.Context, background bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !background {
		s.waitingInteractive++
		defer func() { s.waitingInteractive--; s.signal() }()
	}
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if s.closed {
			return ErrDisabled
		}
		ceiling := s.config.Limits.Concurrent
		if s.active < ceiling && (!background || s.waitingInteractive == 0 && s.background < max(1, ceiling-1)) {
			s.active++
			if background {
				s.background++
			}
			s.wg.Add(1)
			return nil
		}
		changed := s.changed
		s.mu.Unlock()
		select {
		case <-ctx.Done():
		case <-changed:
		}
		s.mu.Lock()
	}
}
func (s *Service) release(background bool) {
	s.mu.Lock()
	s.active--
	if background {
		s.background--
	}
	s.signal()
	s.mu.Unlock()
	s.wg.Done()
}
func (s *Service) Evaluate(ctx context.Context, e Evaluation) (Result, error) {
	if e.LocalOnly {
		return Result{}, ErrLocalOnly
	}
	if s == nil {
		return Result{}, ErrDisabled
	}
	if e.Check == nil || !validScope(e.Scope) {
		return Result{}, failure("scope", ErrInvalid)
	}
	cfg := *s.config
	raw, estimate, err := s.provider.prepare(e.Request)
	if err != nil {
		return Result{}, failure("prepare", err)
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(cfg.Limits.TimeoutSeconds)*time.Second)
	defer cancel()
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	if err = s.acquire(ctx, e.Background); err != nil {
		return Result{}, failure("queue", err)
	}
	defer s.release(e.Background)
	for attempt := 1; attempt <= cfg.Limits.Retries+1; attempt++ {
		if ctx.Err() != nil {
			return Result{}, failure("deadline", ctx.Err())
		}
		if err = e.Check(ctx); err != nil {
			return Result{}, failure("check", err)
		}
		id, err := s.ledger.reserve(e.Scope, attempt, raw, estimate, cfg)
		if err != nil {
			return Result{}, failure("reserve", err)
		}
		result, retry, callErr := s.provider.call(ctx, raw, e.Request)
		if err = s.ledger.settle(id, result); err != nil {
			return Result{}, failure("settle", err)
		}
		if callErr == nil {
			if ctx.Err() != nil {
				return Result{}, failure("deadline", ctx.Err())
			}
			if err = e.Check(ctx); err != nil {
				return Result{}, failure("check", err)
			}
			if ctx.Err() != nil {
				return Result{}, failure("deadline", ctx.Err())
			}
			return result, nil
		}
		if !retry || attempt > cfg.Limits.Retries {
			return Result{}, failure("provider", callErr)
		}
		timer := time.NewTimer(time.Duration(250*(1<<(attempt-1))) * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return Result{}, failure("deadline", ctx.Err())
		case <-timer.C:
		}
	}
	return Result{}, failure("provider", ErrUnavailable)
}
