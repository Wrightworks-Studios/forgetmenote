package automation

import (
	"context"
	"errors"
	"forgetmenote/internal/capacity"
	"forgetmenote/internal/pg"
	"log/slog"
	"sync"
	"time"
)

type Runtime struct {
	Queue       *Queue
	Definitions *Definitions
	mu          sync.Mutex
	cancel      context.CancelFunc
	wg          sync.WaitGroup
	started     bool
	errorsAt    map[string]time.Time

	// PackageRefresh is how often installed packages follow their source;
	// zero means every 30 seconds. Tests shorten it.
	PackageRefresh time.Duration
}
type FenceKey struct{}
type Fence struct {
	ID, Hash string
	Preview  bool
}

// Hooks bind the existing workers to current notebook policy and authorized
// application operations; the runtime owns scheduling, recovery and draining.
type Hooks struct {
	RefreshPackages func(context.Context)
	Personal        func() []Spec
	Policy          func(Spec) error
	RepositoryEvent func(Spec) (Event, bool, error)
	Run             func(context.Context, *Run, func() error) error
}

// OpenRuntime opens the automation state an instance's gateways share in db.
func OpenRuntime(db *pg.DB) (*Runtime, error) {
	queue, err := Open(db)
	if err != nil {
		return nil, err
	}
	definitions, err := OpenDefinitions(db)
	if err != nil {
		return nil, err
	}
	return &Runtime{Queue: queue, Definitions: definitions}, nil
}
func (a *Runtime) Start(ctx context.Context, hooks Hooks) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.started {
		return
	}
	a.started = true
	ctx, a.cancel = context.WithCancel(ctx)
	a.wg.Add(1)
	// One gateway schedules, observes events and recovers the runs of gateways
	// that are gone; every gateway's workers claim runs.
	go func() {
		defer a.wg.Done()
		a.Queue.records.db.Duty(ctx, "automation-scheduler", SchedulerLease, func(ctx context.Context, _ *pg.Lease) { a.schedule(ctx, hooks) })
	}()
	// Preparing package environments must not delay deterministic scheduling.
	if hooks.RefreshPackages != nil {
		every := a.PackageRefresh
		if every <= 0 {
			every = 30 * time.Second
		}
		a.wg.Add(1)
		go func() {
			defer a.wg.Done()
			tick := time.NewTicker(every)
			defer tick.Stop()
			for {
				if ctx.Err() != nil {
					return
				}
				hooks.RefreshPackages(ctx)
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
				}
			}
		}()
	}
	db := a.Queue.records.db
	// Other gateways announce their changes to queued work and to the jobs; a
	// change this gateway makes wakes its own claimer directly.
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		db.Listen(ctx, QueueChannel, func(subject string) {
			if subject != db.Holder {
				a.Queue.heard()
			}
		})
	}()
	// Separate pools keep independent jobs moving while reserving deterministic
	// capacity. Claims still exclude overlapping runs of one job.
	var pools []*pool
	for _, ai := range []bool{false, true} {
		workers := capacity.AutomationWorkers.Limit()
		if ai {
			workers = capacity.AutomationAIWorkers.Limit()
		}
		p := &pool{ai: ai, idle: make(chan struct{}, workers), runs: make(chan *claim, workers)}
		pools = append(pools, p)
		for range workers {
			p.idle <- struct{}{}
			a.wg.Add(1)
			go func() {
				defer a.wg.Done()
				for c := range p.runs {
					if err := a.Queue.execute(c, func(ctx context.Context, r *Run, save func() error) error { return a.run(ctx, hooks, r, save) }); err != nil {
						a.Report("runner", err)
					}
					p.idle <- struct{}{}
					// A finished run frees a worker and its job's next run.
					a.Queue.signal()
				}
			}()
		}
	}
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		a.claim(ctx, hooks, pools)
	}()
}

// claimFallback is how long an idle claimer goes without hearing of a change
// before it scans the queue anyway, for an announcement it missed or a policy
// that now admits a waiting run.
const claimFallback = 5 * time.Second

// A pool is one kind of worker, deterministic or AI. idle holds a token for
// each of its workers that waits for a run, so the claimer claims only runs a
// worker starts at once.
type pool struct {
	ai   bool
	idle chan struct{}
	runs chan *claim
}

// claim hands ready queued runs to idle workers, one gateway's single claimer.
// It looks again when this gateway changes the queue or a worker finishes,
// when another gateway announces a change, when the earliest waiting run
// becomes ready, and otherwise after claimFallback, so an idle queue costs one
// indexed scan per fallback.
func (a *Runtime) claim(ctx context.Context, hooks Hooks, pools []*pool) {
	defer func() {
		// Workers drain what was handed to them: a claim whose context ended
		// first is queued again unrun.
		for _, p := range pools {
			close(p.runs)
		}
	}()
	accepts := make([]func(Spec) bool, len(pools))
	for i, p := range pools {
		accepts[i] = func(spec Spec) bool { return spec.UsesAI() == p.ai && hooks.Policy(spec) == nil }
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-a.Queue.wake:
		case <-timer.C:
		}
		timer.Reset(time.Until(a.dispatch(ctx, pools, accepts)))
	}
}

// dispatch claims ready runs for the pools' idle workers and returns when to
// look again.
func (a *Runtime) dispatch(ctx context.Context, pools []*pool, accepts []func(Spec) bool) time.Time {
	now := time.Now()
	next := now.Add(claimFallback)
	// A pool whose workers are all busy waits for one to finish, which wakes
	// the claimer again, so waiting weighs no run for it.
	asked := make([]func(Spec) bool, len(pools))
	for i, p := range pools {
		if len(p.idle) > 0 {
			asked[i] = accepts[i]
		}
	}
	ready, at, err := a.Queue.waiting(asked, now)
	if err != nil {
		a.Report("runner", err)
		return next
	}
	if !at.IsZero() && at.Before(next) {
		next = at
	}
	for i, p := range pools {
		for ready[i] && ctx.Err() == nil && take(p.idle) {
			c, err := a.Queue.claim(ctx, accepts[i])
			if errors.Is(err, pg.ErrConflict) {
				// Only a gateway whose queue lock was lost meets another's
				// claim; the run stays with the gateway that wrote first.
				slog.Warn("automation claim lost to another gateway after the queue lock was lost", "error", err)
			} else if err != nil {
				a.Report("runner", err)
			}
			if c == nil {
				p.idle <- struct{}{}
				break
			}
			p.runs <- c
		}
	}
	return next
}

// take takes an idle worker's token when there is one.
func take(idle chan struct{}) bool {
	select {
	case <-idle:
		return true
	default:
		return false
	}
}

// run runs a claimed run's work, bound to the configured job it was queued
// from unless it is a chat or personal assistant run.
func (a *Runtime) run(ctx context.Context, hooks Hooks, r *Run, save func() error) error {
	if r.Spec.Kind != "assistant" && r.Spec.Kind != "personal" {
		configured, ok := a.Definitions.Get(r.Spec.ID)
		if !ok || configured.Spec.Disabled && !r.DryRun || Hash(configured.Spec) != Hash(r.Spec) {
			return errors.New("automation configuration changed")
		}
		ctx = context.WithValue(ctx, FenceKey{}, Fence{ID: r.Spec.ID, Hash: Hash(r.Spec), Preview: r.DryRun})
	}
	return hooks.Run(ctx, r, save)
}

func (a *Runtime) Close() {
	a.mu.Lock()
	if a.cancel != nil {
		a.cancel()
	}
	a.mu.Unlock()
	a.wg.Wait()
}
func (a *Runtime) Report(stage string, err error) {
	a.mu.Lock()
	if a.errorsAt == nil {
		a.errorsAt = map[string]time.Time{}
	}
	now := time.Now()
	last := a.errorsAt[stage]
	if now.Sub(last) < time.Minute {
		a.mu.Unlock()
		return
	}
	a.errorsAt[stage] = now
	a.mu.Unlock()
	slog.Error("automation infrastructure", "stage", stage, "error", err)
}

// SchedulerLease is how long a gateway that stops renewing keeps the scheduler.
const SchedulerLease = 30 * time.Second

// scheduleFallback is how long the scheduler trusts what it last read of the
// jobs and the queue while it hears of no change to either.
const scheduleFallback = time.Minute

func (a *Runtime) schedule(ctx context.Context, hooks Hooks) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	var eventsAt, recoveredAt, prunedAt, scheduledAt time.Time
	var heard uint64
	var planned string
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		now := time.Now()
		if now.Sub(recoveredAt) >= 10*time.Second {
			recoveredAt = now
			// The claimer's scans, at least one each claimFallback, show
			// whether a run is running nowhere here.
			if err := a.Queue.recover(10 * time.Second); err != nil {
				a.Report("recovery", err)
			}
		}
		if now.Sub(prunedAt) >= time.Minute {
			prunedAt = now
			if err := a.Queue.PruneLearning(now); err != nil {
				a.Report("retention", err)
			}
		}
		// Due slots are worked out every second, but the jobs and the queue are
		// read again only once this gateway changed or heard of a change, or
		// after scheduleFallback. An unchanged plan over an unchanged queue
		// would enqueue nothing new.
		changes := a.Queue.changes.Load()
		stale := changes != heard || now.Sub(scheduledAt) >= scheduleFallback
		specs := hooks.Personal()
		for _, spec := range a.Definitions.specs(stale) {
			if hooks.Policy(spec) == nil {
				specs = append(specs, spec)
			}
		}
		if plan := dueSlots(specs, now); stale || plan != planned {
			if err := a.Queue.Schedule(specs, now); err != nil {
				a.Report("schedule", err)
			} else {
				heard, planned, scheduledAt = changes, plan, now
			}
		}
		if now.Sub(eventsAt) >= 30*time.Second {
			eventsAt = now
			for _, spec := range specs {
				if spec.Disabled || spec.Trigger == nil || spec.Trigger.Kind != "repository.updated" {
					continue
				}
				event, ready, err := hooks.RepositoryEvent(spec)
				if err != nil {
					a.Report("source trigger", err)
					continue
				}
				if !ready {
					continue
				}
				event.At = eventsAt
				_, err = a.Queue.ObserveEvent(spec, event, eventsAt)
				if err != nil {
					a.Report("source event", err)
				}
			}
		}
	}
}

// dueSlots identifies specs and the slot due for each at now, which with an
// unchanged queue decide everything Schedule does.
func dueSlots(specs []Spec, now time.Time) string {
	due := make([]string, 0, 2*len(specs))
	for _, spec := range specs {
		key, _, ok := spec.Slot(now)
		if !ok {
			key = ""
		}
		due = append(due, Hash(spec), key)
	}
	return Hash(due)
}
