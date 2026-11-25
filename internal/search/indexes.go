package search

import (
	"container/list"
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"forgetmenote/internal/capacity"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/knowledge"
	"forgetmenote/internal/pg"
	"forgetmenote/internal/workspace"
)

type indexSlot struct {
	validated  bool
	generation atomic.Uint64
	checkpoint atomic.Pointer[idleIndexCheckpoint]
	store      *workspace.Store
	policyMu   sync.Mutex
	watchMu    sync.Mutex
	engine     *knowledge.Engine
	processing *knowledge.Processing
	transition chan struct{}
	references int
	bytes      int64
	idle       *list.Element
	hints      <-chan struct{}
	cancel     func()
	retired    bool
	background atomic.Bool
	pending    atomic.Bool
	reset      atomic.Bool
}

type idleIndexCheckpoint struct {
	knowledge        *knowledge.BackgroundCheckpoint
	generation       uint64
	policy           catalog.AISettings
	managed, allowed bool
	endpoint         *knowledge.Endpoint
}

// Indexes owns notebook engine leases, idle retention and one fair background
// processor. Active and nested leases can exceed the idle budget. Opening and
// closing are fenced per notebook without holding the registry during owner I/O.
type Indexes struct {
	mu              sync.Mutex
	configureMu     sync.Mutex
	slots           map[*workspace.Store]*indexSlot
	order           []*indexSlot
	idle            list.List
	idleBytes       int64
	active          int
	changed         chan struct{}
	endpoint        *knowledge.Endpoint
	started, closed bool
	done            chan struct{}
	workerDone      chan struct{}
	workerCancel    context.CancelFunc
	wake            chan struct{}
	catalog         *catalog.Catalog
	db              *pg.DB
	permitted       func(*knowledge.Endpoint) *knowledge.Endpoint
	// recovery is how often background notebooks are queued again, which
	// retries failed embeddings and storage; tests shorten it.
	recovery time.Duration
}

func NewIndexes(db *pg.DB, projects *catalog.Catalog, permitted func(*knowledge.Endpoint) *knowledge.Endpoint) *Indexes {
	if permitted == nil {
		permitted = func(*knowledge.Endpoint) *knowledge.Endpoint { return nil }
	}
	return &Indexes{catalog: projects, db: db, permitted: permitted, recovery: 30 * time.Second, slots: map[*workspace.Store]*indexSlot{}, changed: make(chan struct{}), done: make(chan struct{}), wake: make(chan struct{}, 1)}
}
func (r *Indexes) Endpoint() *knowledge.Endpoint { r.mu.Lock(); defer r.mu.Unlock(); return r.endpoint }
func (r *Indexes) signal() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}
func (r *Indexes) changeLocked() { close(r.changed); r.changed = make(chan struct{}) }
func (r *Indexes) removeIdleLocked(slot *indexSlot) {
	if slot.idle != nil {
		r.idle.Remove(slot.idle)
		slot.idle = nil
		r.idleBytes -= slot.bytes
	}
}

func (r *Indexes) Acquire(ctx context.Context, store *workspace.Store) (*knowledge.Engine, func(), error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		r.mu.Lock()
		if r.closed {
			r.mu.Unlock()
			return nil, nil, errors.New("search indexes are closed")
		}
		slot := r.slots[store]
		if slot == nil {
			slot = &indexSlot{store: store}
			slot.processing = knowledge.NewProcessing(func(reset bool) {
				slot.generation.Add(1)
				slot.pending.Store(true)
				slot.background.Store(true)
				if reset {
					slot.reset.Store(true)
				}
				r.signal()
			})
			r.slots[store] = slot
			r.order = append(r.order, slot)
			slot.pending.Store(true)
			slot.reset.Store(true)
		}
		if slot.retired {
			r.mu.Unlock()
			return nil, nil, errors.New("notebook index is closed")
		}
		if slot.transition != nil {
			changed := slot.transition
			r.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			case <-changed:
				continue
			}
		}
		r.removeIdleLocked(slot)
		slot.references++
		r.active++
		engine := slot.engine
		if engine == nil {
			slot.transition = make(chan struct{})
		}
		r.mu.Unlock()
		slot.policyMu.Lock()
		var err error
		if engine == nil {
			engine, err = knowledge.OpenWithProcessing(r.db, store, slot.processing)
		}
		if err == nil {
			err = ctx.Err()
		}
		if err == nil {
			err = r.applyPolicy(ctx, engine)
		}
		r.mu.Lock()
		started, endpoint := r.started, r.endpoint
		r.mu.Unlock()
		validated := err == nil
		if err == nil && started {
			err = engine.SetEndpoint(r.permitted(endpoint))
		}
		if err == nil && started {
			err = r.watch(slot)
		}
		slot.policyMu.Unlock()
		r.mu.Lock()
		if slot.transition != nil {
			slot.engine = engine
			close(slot.transition)
			slot.transition = nil
			r.changeLocked()
		}
		if validated {
			slot.validated = true
		}
		closed := r.closed
		r.mu.Unlock()
		release := func() { r.release(slot) }
		var once sync.Once
		done := func() { once.Do(release) }
		if err != nil || closed {
			done()
			if err == nil {
				err = errors.New("search indexes are closed")
			}
			return nil, nil, err
		}
		r.signal()
		return engine, done, nil
	}
}

// Register validates a notebook and schedules processing without retaining a
// borrowed engine. Startup and policy reconciliation use the same lease path.
func (r *Indexes) Register(ctx context.Context, store *workspace.Store) error {
	_, release, err := r.Acquire(ctx, store)
	if err == nil {
		release()
	}
	return err
}
func (r *Indexes) watch(slot *indexSlot) error {
	slot.watchMu.Lock()
	defer slot.watchMu.Unlock()
	r.mu.Lock()
	watched, closed := slot.hints != nil, r.closed
	r.mu.Unlock()
	if closed {
		return errors.New("search indexes are closed")
	}
	if watched {
		return nil
	}
	hints, cancel, err := slot.store.Watch()
	if err != nil {
		return err
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		cancel()
		return errors.New("search indexes are closed")
	}
	slot.hints, slot.cancel = hints, cancel
	r.mu.Unlock()
	r.signal()
	return nil
}
func (r *Indexes) release(slot *indexSlot) {
	r.mu.Lock()
	slot.references--
	r.active--
	if slot.references == 0 && slot.engine != nil {
		slot.bytes = slot.engine.EstimatedBytes()
		slot.background.Store(slot.engine.BackgroundRequired())
		if slot.engine.RecoveryRequired() {
			slot.checkpoint.Store(nil)
		}
		slot.idle = r.idle.PushBack(slot)
		r.idleBytes += slot.bytes
	}
	var evict []*indexSlot
	if !r.closed {
		for r.idle.Len() > capacity.IdleEngines.Limit() || r.idleBytes > int64(capacity.IdleIndexBytes.Limit()) {
			oldest := r.idle.Front()
			if oldest == nil {
				break
			}
			idle := oldest.Value.(*indexSlot)
			r.removeIdleLocked(idle)
			idle.transition = make(chan struct{})
			evict = append(evict, idle)
		}
	}
	r.changeLocked()
	r.mu.Unlock()
	for _, idle := range evict {
		idle.engine.Close()
		r.mu.Lock()
		idle.engine = nil
		close(idle.transition)
		idle.transition = nil
		r.changeLocked()
		r.mu.Unlock()
	}
}
func (r *Indexes) Start(ctx context.Context) error {
	r.configureMu.Lock()
	defer r.configureMu.Unlock()
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return errors.New("search indexes are closed")
	}
	if r.started {
		r.mu.Unlock()
		return nil
	}
	r.started = true
	r.mu.Unlock()
	if err := r.restart(ctx); err != nil {
		r.mu.Lock()
		r.started = false
		r.mu.Unlock()
		return err
	}
	workerCtx, cancel := context.WithCancel(context.Background())
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		cancel()
		return errors.New("search indexes are closed")
	}
	r.workerCancel = cancel
	r.workerDone = make(chan struct{})
	r.mu.Unlock()
	go r.process(workerCtx)
	return nil
}
func (r *Indexes) Configure(endpoint *knowledge.Endpoint) error {
	r.configureMu.Lock()
	defer r.configureMu.Unlock()
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return errors.New("search indexes are closed")
	}
	r.endpoint = endpoint
	r.mu.Unlock()
	return r.restart(context.Background())
}
func (r *Indexes) Restart() error {
	r.configureMu.Lock()
	defer r.configureMu.Unlock()
	return r.restart(context.Background())
}
func (r *Indexes) restart(ctx context.Context) error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return errors.New("search indexes are closed")
	}
	if !r.started {
		r.mu.Unlock()
		return nil
	}
	stores := make([]*workspace.Store, 0, len(r.order))
	for _, slot := range r.order {
		if !slot.retired && slot.validated {
			slot.generation.Add(1)
			slot.background.Store(true)
			stores = append(stores, slot.store)
			slot.pending.Store(true)
			slot.reset.Store(true)
		}
	}
	r.mu.Unlock()
	for _, store := range stores {
		if err := r.Register(ctx, store); err != nil {
			return err
		}
	}
	r.signal()
	return nil
}
func (r *Indexes) refreshPending() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, slot := range r.order {
		if slot.background.Load() && !slot.retired {
			slot.pending.Store(true)
			slot.reset.Store(true)
		}
	}
}
func (r *Indexes) process(ctx context.Context) {
	defer close(r.workerDone)
	listening := make(chan struct{})
	go func() { defer close(listening); r.listenPurges(ctx) }()
	defer func() { <-listening }()
	ticker := time.NewTicker(r.recovery)
	defer ticker.Stop()
	cursor := 0
	for ctx.Err() == nil {
		select {
		case <-ticker.C:
			r.refreshPending()
		default:
		}
		r.mu.Lock()
		order := append([]*indexSlot(nil), r.order...)
		r.mu.Unlock()
		var selected *indexSlot
		for i := 0; i < len(order); i++ {
			index := (cursor + i) % len(order)
			slot := order[index]
			r.mu.Lock()
			hints, retired := slot.hints, slot.retired
			r.mu.Unlock()
			if retired {
				continue
			}
			if hints != nil {
				select {
				case _, ok := <-hints:
					if !ok {
						r.mu.Lock()
						slot.retired = true
						r.mu.Unlock()
						continue
					}
					slot.generation.Add(1)
					slot.pending.Store(true)
					slot.reset.Store(true)
				default:
				}
			}
			if selected == nil && slot.pending.Swap(false) {
				selected = slot
				cursor = (index + 1) % len(order)
			}
		}
		if selected != nil {
			reset := selected.reset.Swap(false)
			probeCtx, probeDone := context.WithTimeout(ctx, 35*time.Second)
			unchanged := r.coldUnchanged(probeCtx, selected)
			probeDone()
			if unchanged {
				continue
			}
			generation := selected.generation.Load()
			policy, managed := r.catalog.AISettings()
			allowed := r.catalog.BackgroundIndexAllowed(selected.store)
			endpoint := r.permitted(r.Endpoint())
			engine, release, err := r.Acquire(ctx, selected.store)
			if err == nil {
				more := engine.ProcessBatch(ctx, reset)
				var checkpoint *idleIndexCheckpoint
				if !more {
					if marker := engine.BackgroundCheckpoint(); marker != nil {
						checkpoint = &idleIndexCheckpoint{knowledge: marker, generation: generation, policy: policy, managed: managed, allowed: allowed, endpoint: endpoint}
					}
				}
				selected.checkpoint.Store(checkpoint)
				release()
				if more {
					selected.pending.Store(true)
				}
			}
			continue
		}
		cases := []reflect.SelectCase{{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(ctx.Done())}, {Dir: reflect.SelectRecv, Chan: reflect.ValueOf(r.wake)}, {Dir: reflect.SelectRecv, Chan: reflect.ValueOf(ticker.C)}}
		watchers := []*indexSlot{}
		for _, slot := range order {
			r.mu.Lock()
			hints, retired := slot.hints, slot.retired
			r.mu.Unlock()
			if hints != nil && !retired {
				cases = append(cases, reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(hints)})
				watchers = append(watchers, slot)
			}
		}
		index, _, ok := reflect.Select(cases)
		if index == 0 {
			return
		}
		if index == 2 {
			r.refreshPending()
		} else if index >= 3 {
			slot := watchers[index-3]
			if ok {
				slot.generation.Add(1)
				slot.pending.Store(true)
				slot.reset.Store(true)
			} else {
				r.mu.Lock()
				slot.retired = true
				r.mu.Unlock()
			}
		}
	}
}
func (r *Indexes) coldUnchanged(ctx context.Context, slot *indexSlot) bool {
	r.mu.Lock()
	cold := slot.engine == nil && slot.transition == nil
	r.mu.Unlock()
	checkpoint := slot.checkpoint.Load()
	if !cold || checkpoint == nil || checkpoint.generation != slot.generation.Load() {
		return false
	}
	policy, managed := r.catalog.AISettings()
	if policy != checkpoint.policy || managed != checkpoint.managed || r.catalog.BackgroundIndexAllowed(slot.store) != checkpoint.allowed || r.permitted(r.Endpoint()) != checkpoint.endpoint {
		return false
	}
	return checkpoint.knowledge.Unchanged(ctx, slot.store) && checkpoint.generation == slot.generation.Load()
}

func (r *Indexes) Close() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		<-r.done
		return
	}
	r.closed = true
	cancel, workerDone := r.workerCancel, r.workerDone
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if workerDone != nil {
		<-workerDone
	}
	for {
		r.mu.Lock()
		transitioning := false
		for _, slot := range r.order {
			transitioning = transitioning || slot.transition != nil
		}
		if r.active == 0 && !transitioning {
			break
		}
		changed := r.changed
		r.mu.Unlock()
		<-changed
	}
	engines := []*knowledge.Engine{}
	cancels := []func(){}
	for _, slot := range r.order {
		if slot.engine != nil {
			engines = append(engines, slot.engine)
		}
		if slot.cancel != nil {
			cancels = append(cancels, slot.cancel)
		}
	}
	r.slots = nil
	r.order = nil
	r.idle.Init()
	r.idleBytes = 0
	r.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	for _, engine := range engines {
		engine.Close()
	}
	close(r.done)
}
func (r *Indexes) applyPolicy(ctx context.Context, engine *knowledge.Engine) error {
	settings, managed := r.catalog.AISettings()
	if !managed {
		return nil
	}
	settings.Embeddings = settings.Embeddings && r.catalog.BackgroundIndexAllowed(engine.Store)
	previous := engine.Config()
	config := EmbeddingConfig(previous, settings)
	if config == previous {
		return nil
	}
	_, err := engine.Configure(ctx, config, previous.ID() != config.ID())
	return err
}
func EmbeddingConfig(config knowledge.Config, settings catalog.AISettings) knowledge.Config {
	config.Enabled = settings.Embeddings
	if config.Enabled {
		config.Model, config.Dimensions, config.Endpoint = settings.Model, settings.Dimensions, true
	}
	if config.PassageRunes == 0 {
		config.PassageRunes = 1400
	}
	if config.Splitter == "" {
		config.Splitter = "markdown-heading-v1"
	}
	return config
}
