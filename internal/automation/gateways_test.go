package automation

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"forgetmenote/internal/pg"
	"forgetmenote/internal/pg/pgtest"
)

func gatewaySpec(id string) Spec {
	return Spec{ID: id, Kind: "assistant", Actor: "robot", Project: "default", Instruction: "Check", ExternalProcessing: true, Delivery: &Delivery{Client: "slack", Channel: "D123", User: "U123", Direct: true}}
}

func TestGatewaysClaimEachRunOnceAndKeepLiveClaims(t *testing.T) {
	db := pgtest.Org(t)
	other, _ := pgtest.Gateway(t, db)
	a, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Open(other)
	if err != nil {
		t.Fatal(err)
	}
	queued, err := a.Enqueue(gatewaySpec("one"), "key")
	if err != nil {
		t.Fatal(err)
	}
	var runs atomic.Int32
	started, release := make(chan string, 2), make(chan struct{})
	var wg sync.WaitGroup
	for name, q := range map[string]*Queue{"a": a, "b": b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := q.RunNext(t.Context(), func(ctx context.Context, _ *Run, _ func() error) error {
				runs.Add(1)
				started <- name
				select {
				case <-release:
				case <-ctx.Done():
				}
				return ctx.Err()
			})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	var runner string
	select {
	case runner = <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("queued run did not start")
	}
	time.Sleep(100 * time.Millisecond)
	if runs.Load() != 1 {
		t.Fatalf("two gateways claimed one run: %d", runs.Load())
	}
	// Opening and recovering on another gateway leaves a live claim running.
	idle := map[string]*Queue{"a": b, "b": a}[runner]
	if _, err = Open(idle.records.db); err != nil {
		t.Fatal(err)
	}
	if err = idle.Recover(); err != nil {
		t.Fatal(err)
	}
	if got, err := idle.Get(queued.ID); err != nil || got.State != "running" {
		t.Fatalf("another gateway took a live run back: %+v %v", got.State, err)
	}
	close(release)
	wg.Wait()
}

func TestGatewayRecoversRunsOfAGatewayThatIsGone(t *testing.T) {
	db := pgtest.Org(t)
	other, leave := pgtest.Gateway(t, db)
	a, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Open(other)
	if err != nil {
		t.Fatal(err)
	}
	queued, err := b.Enqueue(gatewaySpec("one"), "key")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	stalled, stop := context.WithCancel(context.Background())
	defer stop()
	go b.RunNext(stalled, func(ctx context.Context, _ *Run, _ func() error) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})
	<-started
	if err = a.Recover(); err != nil {
		t.Fatal(err)
	}
	if got, _ := a.Get(queued.ID); got.State != "running" {
		t.Fatalf("live claim recovered: %s", got.State)
	}
	// The gateway stops renewing its presence, as a crashed process does.
	leave()
	if err = a.Recover(); err != nil {
		t.Fatal(err)
	}
	got, err := a.Get(queued.ID)
	if err != nil || got.State != "queued" || got.Claimant != "" {
		t.Fatalf("run of a gone gateway was not queued again: %s %q %v", got.State, got.Claimant, err)
	}
}

func TestCancelOnAnotherGatewayStopsTheRun(t *testing.T) {
	db := pgtest.Org(t)
	other, _ := pgtest.Gateway(t, db)
	a, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Open(other)
	if err != nil {
		t.Fatal(err)
	}
	queued, err := a.Enqueue(gatewaySpec("one"), "key")
	if err != nil {
		t.Fatal(err)
	}
	started, stopped := make(chan struct{}), make(chan error, 1)
	go a.RunNext(t.Context(), func(ctx context.Context, _ *Run, _ func() error) error {
		close(started)
		select {
		case <-ctx.Done():
			stopped <- ctx.Err()
		case <-time.After(10 * time.Second):
			stopped <- nil
		}
		return ctx.Err()
	})
	<-started
	if err = b.Cancel(queued.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-stopped:
		if err == nil {
			t.Fatal("run ignored a cancellation from another gateway")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run ignored a cancellation from another gateway")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, err := b.Get(queued.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.State == "cancelled" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cancelled run finished as %s", got.State)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestOneGatewaySchedules(t *testing.T) {
	db := pgtest.Org(t)
	other, _ := pgtest.Gateway(t, db)
	var calls sync.Map
	var count atomic.Int32
	for _, handle := range []*struct{ name string }{{"a"}, {"b"}} {
		h := db
		if handle.name == "b" {
			h = other
		}
		r, err := OpenRuntime(h)
		if err != nil {
			t.Fatal(err)
		}
		name := handle.name
		r.Start(t.Context(), Hooks{Personal: func() []Spec {
			if _, seen := calls.LoadOrStore(name, true); !seen {
				count.Add(1)
			}
			return nil
		}, Policy: func(Spec) error { return nil }, Run: func(context.Context, *Run, func() error) error { return nil }})
		t.Cleanup(r.Close)
	}
	time.Sleep(2500 * time.Millisecond)
	if count.Load() != 1 {
		t.Fatalf("%d gateways ran the scheduler", count.Load())
	}
}

// dropQueueLock ends the connection on which a gateway holds db's queue lock,
// as a network fault does. The lock is released while that gateway carries on
// with its critical section.
func dropQueueLock(t *testing.T, db *pg.DB) {
	t.Helper()
	var dropped bool
	err := db.Pool.QueryRow(t.Context(), `SELECT pg_terminate_backend(l.pid) FROM pg_locks l, hashtextextended('automation-queue ' || $1, 0) AS key
		WHERE l.locktype = 'advisory' AND l.granted AND l.objsubid = 1 AND l.database = (SELECT oid FROM pg_database WHERE datname = current_database())
		AND l.classid::bigint = (key >> 32) & 4294967295 AND l.objid::bigint = key & 4294967295`, db.Org).Scan(&dropped)
	if err != nil || !dropped {
		t.Fatalf("no queue lock to drop: %v", err)
	}
}

func TestGatewaysNeverBothClaimARunWhenTheQueueLockIsLost(t *testing.T) {
	db := pgtest.Org(t)
	other, _ := pgtest.Gateway(t, db)
	a, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Open(other)
	if err != nil {
		t.Fatal(err)
	}
	queued, err := a.Enqueue(gatewaySpec("one"), "key")
	if err != nil {
		t.Fatal(err)
	}
	var runs atomic.Int32
	results := make(chan error, 2)
	claim := func(q *Queue) {
		_, err := q.RunNext(t.Context(), func(context.Context, *Run, func() error) error {
			runs.Add(1)
			return nil
		})
		results <- err
	}
	waiting, release := pgtest.HoldWrites(t, db, "automation_records")
	go claim(a)
	// a has chosen the run and waits to write its claim when its lock goes.
	waiting(1)
	dropQueueLock(t, db)
	go claim(b)
	waiting(2)
	release()
	conflicts := 0
	for range 2 {
		switch err := <-results; {
		case errors.Is(err, pg.ErrConflict):
			conflicts++
		case err != nil:
			t.Fatal(err)
		}
	}
	if runs.Load() != 1 || conflicts != 1 {
		t.Fatalf("one queued run ran %d times; %d claims conflicted", runs.Load(), conflicts)
	}
	got, err := b.Get(queued.ID)
	if err != nil || got.State != "completed" || got.Attempts != 1 || got.Claimant != "" {
		t.Fatalf("claimed run ended as %s after %d attempts, claimant %q: %v", got.State, got.Attempts, got.Claimant, err)
	}
}

func TestARunAnotherGatewayTookOverKeepsItsNewOwner(t *testing.T) {
	db := pgtest.Org(t)
	other, leave := pgtest.Gateway(t, db)
	a, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Open(other)
	if err != nil {
		t.Fatal(err)
	}
	queued, err := b.Enqueue(gatewaySpec("one"), "key")
	if err != nil {
		t.Fatal(err)
	}
	// b claims the run, then looks gone to a: its presence lapses while its
	// worker still runs, as a partitioned gateway's does.
	started, resume := make(chan struct{}), make(chan struct{})
	stale := make(chan error, 2)
	go func() {
		_, err := b.RunNext(context.Background(), func(ctx context.Context, _ *Run, save func() error) error {
			close(started)
			<-resume
			stale <- save()
			return nil
		})
		stale <- err
	}()
	<-started
	leave()
	if err = a.Recover(); err != nil {
		t.Fatal(err)
	}
	var owner atomic.Int32
	if ran, err := a.RunNext(t.Context(), func(context.Context, *Run, func() error) error { owner.Add(1); return nil }); err != nil || !ran {
		t.Fatal("the recovered run was not claimed again", ran, err)
	}
	close(resume)
	for range 2 {
		if err := <-stale; !errors.Is(err, pg.ErrConflict) {
			t.Fatalf("the gateway that lost the run wrote it: %v", err)
		}
	}
	got, err := a.Get(queued.ID)
	if err != nil || got.State != "completed" || got.Attempts != 2 || owner.Load() != 1 {
		t.Fatalf("taken-over run ended as %s after %d attempts: %v", got.State, got.Attempts, err)
	}
}

func TestARunStopsSoonAfterAnotherGatewayTakesItOver(t *testing.T) {
	db := pgtest.Org(t)
	other, leave := pgtest.Gateway(t, db)
	a, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Open(other)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = b.Enqueue(gatewaySpec("one"), "key"); err != nil {
		t.Fatal(err)
	}
	started, stopped := make(chan struct{}), make(chan error, 1)
	go b.RunNext(context.Background(), func(ctx context.Context, _ *Run, save func() error) error {
		close(started)
		// The worker's own checkpoints never stop it.
		for range 3 {
			if err := save(); err != nil {
				stopped <- err
				return err
			}
			time.Sleep(400 * time.Millisecond)
		}
		select {
		case <-ctx.Done():
			stopped <- ctx.Err()
		case <-time.After(10 * time.Second):
			stopped <- nil
		}
		return ctx.Err()
	})
	<-started
	time.Sleep(1500 * time.Millisecond)
	leave()
	if err = a.Recover(); err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	defer close(release)
	go a.RunNext(t.Context(), func(ctx context.Context, _ *Run, _ func() error) error {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil
	})
	select {
	case err := <-stopped:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("the replaced worker stopped with %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a run kept going after another gateway took it over")
	}
}

func TestReadsDoNotWaitForAnotherGatewaysQueueLock(t *testing.T) {
	db := pgtest.Org(t)
	other, _ := pgtest.Gateway(t, db)
	q, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	spec := gatewaySpec("one")
	queued, err := q.Enqueue(spec, "key")
	if err != nil {
		t.Fatal(err)
	}
	// Another gateway sits in a long queue critical section.
	release, err := other.Exclusive(t.Context(), "automation-queue")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	done := make(chan error, 1)
	go func() {
		if runs, err := q.List(); err != nil || len(runs) != 1 {
			done <- errors.Join(err, errors.New("listing lost the queued run"))
			return
		}
		if got, err := q.Get(queued.ID); err != nil || got.State != "queued" {
			done <- errors.Join(err, errors.New("reading lost the queued run"))
			return
		}
		_, err := q.JobState(spec)
		if err == nil {
			_, err = q.ReadDrive(spec.ID)
		}
		if err == nil {
			_, err = q.RepairMemo(spec)
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("reads waited for another gateway's queue lock")
	}
}

// Another gateway prunes old runs while this one lists them without the
// queue's cross-gateway lock, so a run can go between the listing and the read
// of that run. The listing leaves it out instead of failing: the automation
// health task, which lists every run, failed this way whenever the other
// gateway pruned while it looked.
func TestListingLeavesOutARunAnotherGatewayRemovesMeanwhile(t *testing.T) {
	url := pgtest.URL(t, true)
	pruner, err := pg.Open(t.Context(), url, "")
	if err != nil {
		t.Fatal(err)
	}
	defer pruner.Close()
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	trace := &removeOnRead{}
	config.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	lister := &pg.DB{Pool: pool, Org: pruner.Org, Holder: pg.NewHolder()}
	for _, db := range []*pg.DB{pruner, lister} {
		leave, err := db.KeepPresence()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(leave)
	}
	a, err := Open(lister)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Open(pruner)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, job := range []string{"one", "two"} {
		r, err := b.Enqueue(gatewaySpec(job), "key")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, r.ID)
	}
	// Runs are read in name order, so the second goes while the first is read.
	slices.Sort(ids)
	kept, removed := ids[0], ids[1]
	gone, err := b.Get(removed)
	if err != nil {
		t.Fatal(err)
	}
	trace.arm(kept+".json", func() error { return b.records.remove(removed+".json", gone.stored) })

	runs, err := a.List()
	if err != nil {
		t.Fatalf("listing failed when another gateway removed a run: %v", err)
	}
	if err := trace.result(); err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].ID != kept {
		t.Fatalf("listed %d runs, want only %s", len(runs), kept)
	}
	if _, err := a.Get(removed); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the removed run is still read: %v", err)
	}
}

// removeOnRead runs remove once, as its gateway starts to read the record
// named, so the removal falls between that gateway's listing and its reads.
type removeOnRead struct {
	mu     sync.Mutex
	name   string
	remove func() error
	ran    bool
	err    error
}

func (r *removeOnRead) arm(name string, remove func() error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.name, r.remove = name, remove
}

func (r *removeOnRead) result() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.ran {
		return errors.New("the removal never ran during the listing")
	}
	return r.err
}

func (r *removeOnRead) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.remove != nil && !r.ran && strings.HasPrefix(data.SQL, `SELECT version, "body" FROM "automation_records"`) && len(data.Args) == 2 && data.Args[1] == r.name {
		r.ran, r.err = true, r.remove()
	}
	return ctx
}

func (*removeOnRead) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}
