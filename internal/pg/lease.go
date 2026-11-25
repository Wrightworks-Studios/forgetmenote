package pg

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrLeaseLost reports a duty another gateway took over, or one that expired
// before its holder renewed it.
var ErrLeaseLost = errors.New("this gateway no longer holds the lease")

// PresenceTTL is how long a gateway that stops renewing its presence keeps the
// work it claimed. A gateway that shuts down cleanly gives it up at once.
const PresenceTTL = 30 * time.Second

const leaseTimeout = 10 * time.Second

// NewHolder returns a random identity for one gateway process.
func NewHolder() string {
	var b [16]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func leaseContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), leaseTimeout)
}

func millis(d time.Duration) int64 { return d.Milliseconds() }

type presence struct {
	count int
	stop  context.CancelFunc
	done  chan struct{}
}

var (
	presencesMu sync.Mutex
	presences   = map[string]*presence{}
)

// KeepPresence records this handle's holder as a live gateway until every
// returned leave has run, which removes the record. Presence is renewed every
// third of PresenceTTL; work a holder claimed is recovered by other gateways
// only once its presence expires or is removed.
func (db *DB) KeepPresence() (leave func(), err error) {
	if db.Holder == "" {
		return nil, errors.New("database handle has no holder identity")
	}
	key := db.Org + "/" + db.Holder
	presencesMu.Lock()
	defer presencesMu.Unlock()
	p := presences[key]
	if p == nil {
		ctx, cancel := leaseContext(context.Background())
		err = db.renewPresence(ctx)
		cancel()
		if err != nil {
			return nil, err
		}
		ctx, stop := context.WithCancel(context.Background())
		p = &presence{stop: stop, done: make(chan struct{})}
		presences[key] = p
		go db.presence(ctx, p.done)
	}
	p.count++
	var once sync.Once
	return func() {
		once.Do(func() {
			presencesMu.Lock()
			defer presencesMu.Unlock()
			if p.count--; p.count > 0 {
				return
			}
			delete(presences, key)
			p.stop()
			<-p.done
			ctx, cancel := leaseContext(context.Background())
			defer cancel()
			db.Pool.Exec(ctx, `DELETE FROM leases WHERE org = $1 AND name = $2 AND holder = $3`, db.Org, "gateway/"+db.Holder, db.Holder)
		})
	}, nil
}

func (db *DB) presence(ctx context.Context, done chan struct{}) {
	defer close(done)
	tick := time.NewTicker(PresenceTTL / 3)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		renew, cancel := context.WithTimeout(ctx, leaseTimeout)
		db.renewPresence(renew)
		cancel()
	}
}

func (db *DB) renewPresence(ctx context.Context) error {
	_, err := db.Pool.Exec(ctx, `INSERT INTO leases (org, name, holder, generation, expires)
		VALUES ($1, $2, $3, 1, now() + $4 * interval '1 millisecond')
		ON CONFLICT (org, name) DO UPDATE SET expires = EXCLUDED.expires`, db.Org, "gateway/"+db.Holder, db.Holder, millis(PresenceTTL))
	return err
}

// Gone reports whether holder is another gateway whose presence has expired or
// been removed, so the work it claimed is no longer being done. This handle's
// own holder is never gone: the caller knows its own work.
func (db *DB) Gone(ctx context.Context, q Querier, holder string) (bool, error) {
	if holder == db.Holder {
		return false, nil
	}
	var present bool
	err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM leases WHERE org = $1 AND name = $2 AND holder = $3 AND expires > now())`,
		db.Org, "gateway/"+holder, holder).Scan(&present)
	return !present, err
}

// Lease is one gateway's hold on a named duty, at the generation it acquired.
type Lease struct {
	db         *DB
	Name       string
	Generation int64
}

// Fence locks the lease row against takeover until q's transaction ends, and
// returns ErrLeaseLost when this holder no longer holds it, so a write in the
// same transaction commits only while the duty is still this gateway's.
func (l *Lease) Fence(ctx context.Context, q Querier) error {
	var held bool
	err := q.QueryRow(ctx, `SELECT true FROM leases WHERE org = $1 AND name = $2 AND holder = $3 AND generation = $4 AND expires > now() FOR SHARE`,
		l.db.Org, l.Name, l.db.Holder, l.Generation).Scan(&held)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrLeaseLost
	}
	return err
}

// acquire takes the duty when nobody holds it or its lease expired, returning
// nil when another gateway holds it.
func (db *DB) acquire(ctx context.Context, name string, ttl time.Duration) (*Lease, error) {
	var generation int64
	err := db.Pool.QueryRow(ctx, `INSERT INTO leases (org, name, holder, generation, expires)
		VALUES ($1, $2, $3, 1, now() + $4 * interval '1 millisecond')
		ON CONFLICT (org, name) DO UPDATE SET holder = EXCLUDED.holder, generation = leases.generation + 1, expires = EXCLUDED.expires
		WHERE leases.expires <= now()
		RETURNING generation`, db.Org, name, db.Holder, millis(ttl)).Scan(&generation)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &Lease{db: db, Name: name, Generation: generation}, nil
}

// renew extends a held lease, reporting false once another gateway took it.
func (l *Lease) renew(ctx context.Context, ttl time.Duration) (bool, error) {
	tag, err := l.db.Pool.Exec(ctx, `UPDATE leases SET expires = now() + $5 * interval '1 millisecond'
		WHERE org = $1 AND name = $2 AND holder = $3 AND generation = $4 AND expires > now()`, l.db.Org, l.Name, l.db.Holder, l.Generation, millis(ttl))
	return tag.RowsAffected() == 1, err
}

// release ends a held lease now, so another gateway need not wait for it.
func (l *Lease) release(ctx context.Context) {
	l.db.Pool.Exec(ctx, `UPDATE leases SET expires = now() WHERE org = $1 AND name = $2 AND holder = $3 AND generation = $4`,
		l.db.Org, l.Name, l.db.Holder, l.Generation)
}

// Duty runs fn whenever this gateway holds the lease name, until ctx ends. fn
// receives a context that ends when the lease is lost, which happens when
// another gateway takes it over or when renewal fails for half of ttl, before
// the lease expires in the database. fn must return when its context ends; the
// lease is released only after it does, and Duty then tries again. fn may also
// return early, which releases the lease.
func (db *DB) Duty(ctx context.Context, name string, ttl time.Duration, fn func(context.Context, *Lease)) {
	for ctx.Err() == nil {
		attempt, cancel := leaseContext(ctx)
		lease, err := db.acquire(attempt, name, ttl)
		cancel()
		if err == nil && lease != nil {
			lease.hold(ctx, ttl, fn)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(ttl / 3):
		}
	}
}

func (l *Lease) hold(ctx context.Context, ttl time.Duration, fn func(context.Context, *Lease)) {
	held, lose := context.WithCancel(ctx)
	defer lose()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn(held, l)
	}()
	// The database expires the lease ttl after the last renewal it applied; this
	// gateway stops at half of that, so clock rate and latency cannot overlap
	// two holders.
	deadline := time.Now().Add(ttl / 2)
	tick := time.NewTicker(ttl / 4)
	defer tick.Stop()
	for {
		select {
		case <-done:
			release, cancel := leaseContext(ctx)
			l.release(release)
			cancel()
			return
		case <-tick.C:
		}
		if held.Err() != nil {
			continue
		}
		sent := time.Now()
		renew, cancel := context.WithTimeout(held, ttl/4)
		ok, err := l.renew(renew, ttl)
		cancel()
		switch {
		case ok:
			deadline = sent.Add(ttl / 2)
		case err == nil:
			lose()
		}
		if !time.Now().Before(deadline) {
			lose()
		}
	}
}

// Exclusive holds a transaction-scoped advisory lock on key for this org until
// release runs, on a connection of its own, so critical sections that once
// shared a process mutex are serialized across gateways. It is no fence: if
// that connection drops, the lock is released while the caller continues, so
// writes that must not overlap still check their own versions.
func (db *DB) Exclusive(ctx context.Context, key string) (release func(), err error) {
	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1 || ' ' || $2, 0))`, key, db.Org); err != nil {
		tx.Rollback(context.WithoutCancel(ctx))
		return nil, err
	}
	return func() {
		end, cancel := leaseContext(ctx)
		defer cancel()
		tx.Rollback(end)
	}, nil
}

// Shared holds key as Exclusive does, but alongside other Shared holders: it
// waits for an Exclusive holder and keeps one out until release runs.
func (db *DB) Shared(ctx context.Context, key string) (release func(), err error) {
	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock_shared(hashtextextended($1 || ' ' || $2, 0))`, key, db.Org); err != nil {
		tx.Rollback(context.WithoutCancel(ctx))
		return nil, err
	}
	return func() {
		end, cancel := leaseContext(ctx)
		defer cancel()
		tx.Rollback(end)
	}, nil
}

// TryExclusive is Exclusive without waiting: ok is false when another
// connection holds key, and release is then nil.
func (db *DB) TryExclusive(ctx context.Context, key string) (release func(), ok bool, err error) {
	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	if err = tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended($1 || ' ' || $2, 0))`, key, db.Org).Scan(&ok); err != nil || !ok {
		tx.Rollback(context.WithoutCancel(ctx))
		return nil, false, err
	}
	return func() {
		end, cancel := leaseContext(ctx)
		defer cancel()
		tx.Rollback(end)
	}, true, nil
}
