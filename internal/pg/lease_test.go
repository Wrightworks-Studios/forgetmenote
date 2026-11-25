package pg_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"forgetmenote/internal/pg"
	"forgetmenote/internal/pg/pgtest"
)

func TestDutyRunsOnOneGatewayAndMovesWhenItStops(t *testing.T) {
	a := pgtest.Org(t)
	b, _ := pgtest.Gateway(t, a)
	const ttl = 400 * time.Millisecond
	var running atomic.Int32
	var overlap atomic.Bool
	held := make(chan string, 16)
	duty := func(name string) func(context.Context, *pg.Lease) {
		return func(ctx context.Context, lease *pg.Lease) {
			if running.Add(1) > 1 {
				overlap.Store(true)
			}
			held <- name
			<-ctx.Done()
			running.Add(-1)
		}
	}
	ctxA, stopA := context.WithCancel(context.Background())
	ctxB, stopB := context.WithCancel(context.Background())
	defer stopB()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); a.Duty(ctxA, "fixture", ttl, duty("a")) }()
	go func() { defer wg.Done(); b.Duty(ctxB, "fixture", ttl, duty("b")) }()
	first := <-held
	// Renewal keeps the duty with its holder for several lease periods.
	select {
	case second := <-held:
		t.Fatalf("duty moved from %s to %s while held", first, second)
	case <-time.After(3 * ttl):
	}
	if first == "a" {
		stopA()
	} else {
		stopB()
	}
	select {
	case second := <-held:
		if second == first {
			t.Fatalf("stopped gateway %s took the duty again", first)
		}
	case <-time.After(5 * ttl):
		t.Fatal("released duty was not taken over")
	}
	stopA()
	stopB()
	wg.Wait()
	if overlap.Load() {
		t.Fatal("two gateways ran one duty at once")
	}
}

func TestFenceRefusesAHolderWhoseLeaseWasTakenOver(t *testing.T) {
	a := pgtest.Org(t)
	b, _ := pgtest.Gateway(t, a)
	ctx := context.Background()
	const ttl = 300 * time.Millisecond
	leases := make(chan *pg.Lease, 1)
	stale, stop := context.WithCancel(ctx)
	go a.Duty(stale, "fixture", ttl, func(ctx context.Context, lease *pg.Lease) {
		leases <- lease
		<-ctx.Done()
	})
	lost := <-leases
	tx, err := a.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = lost.Fence(ctx, tx); err != nil {
		t.Fatal("holder could not fence its own write", err)
	}
	tx.Rollback(ctx)
	// The holder stops renewing without releasing, as a stalled process does;
	// another gateway takes over after expiry with a higher generation.
	if _, err = a.Pool.Exec(ctx, `UPDATE leases SET expires = now() WHERE org = $1 AND name = 'fixture'`, a.Org); err != nil {
		t.Fatal(err)
	}
	took := make(chan *pg.Lease, 1)
	next, stopNext := context.WithCancel(ctx)
	defer stopNext()
	go b.Duty(next, "fixture", ttl, func(ctx context.Context, lease *pg.Lease) {
		took <- lease
		<-ctx.Done()
	})
	var current *pg.Lease
	select {
	case current = <-took:
	case <-time.After(5 * time.Second):
		t.Fatal("expired lease was not taken over")
	}
	if current.Generation <= lost.Generation {
		t.Fatalf("takeover kept generation %d after %d", current.Generation, lost.Generation)
	}
	if err = lost.Fence(ctx, a.Pool); !errors.Is(err, pg.ErrLeaseLost) {
		t.Fatal("stale holder passed the fence", err)
	}
	stop()
}

func TestPresenceMarksAGatewayGoneOnlyAfterItLeaves(t *testing.T) {
	a := pgtest.Org(t)
	b, leave := pgtest.Gateway(t, a)
	ctx := context.Background()
	for holder, want := range map[string]bool{a.Holder: false, b.Holder: false, pg.NewHolder(): true} {
		if gone, err := a.Gone(ctx, a.Pool, holder); err != nil || gone != want {
			t.Fatalf("Gone(%s) = %v %v, want %v", holder, gone, err, want)
		}
	}
	leave()
	if gone, err := a.Gone(ctx, a.Pool, b.Holder); err != nil || !gone {
		t.Fatal("a gateway that left still holds its work", gone, err)
	}
	if gone, err := b.Gone(ctx, b.Pool, b.Holder); err != nil || gone {
		t.Fatal("a handle saw its own holder as gone", gone, err)
	}
}

func TestExclusiveSerializesAcrossGateways(t *testing.T) {
	a := pgtest.Org(t)
	b, _ := pgtest.Gateway(t, a)
	other := pgtest.Org(t)
	ctx := context.Background()
	release, err := a.Exclusive(ctx, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	left := make(chan struct{})
	go func() {
		defer close(left)
		release, err := b.Exclusive(ctx, "fixture")
		if err != nil {
			t.Error(err)
			return
		}
		close(entered)
		release()
	}()
	// Another org's lock of the same key is independent.
	if unrelated, err := other.Exclusive(ctx, "fixture"); err != nil {
		t.Fatal(err)
	} else {
		unrelated()
	}
	select {
	case <-entered:
		t.Fatal("two gateways entered one critical section")
	case <-time.After(200 * time.Millisecond):
	}
	if _, ok, err := b.TryExclusive(ctx, "fixture"); err != nil || ok {
		t.Fatal("a held lock was taken without waiting", ok, err)
	}
	release()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("released critical section was not entered")
	}
	// The second holder signals on entry, before its own release finishes.
	<-left
	unlocked, ok, err := b.TryExclusive(ctx, "fixture")
	if err != nil || !ok {
		t.Fatal("a free lock was refused", ok, err)
	}
	unlocked()
}

// Shared holders of a key coexist across gateways and keep an exclusive one
// out, and a shared holder waits for an exclusive one.
func TestSharedExcludesExclusiveAcrossGateways(t *testing.T) {
	a := pgtest.Org(t)
	b, _ := pgtest.Gateway(t, a)
	ctx := context.Background()
	first, err := a.Shared(ctx, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	second, err := b.Shared(ctx, "fixture")
	if err != nil {
		t.Fatal("two gateways could not share a key", err)
	}
	if _, ok, err := b.TryExclusive(ctx, "fixture"); err != nil || ok {
		t.Fatal("an exclusive holder entered beside shared ones", ok, err)
	}
	first()
	if _, ok, err := a.TryExclusive(ctx, "fixture"); err != nil || ok {
		t.Fatal("an exclusive holder entered beside a shared one", ok, err)
	}
	second()
	release, ok, err := a.TryExclusive(ctx, "fixture")
	if err != nil || !ok {
		t.Fatal("the released key stayed held", ok, err)
	}
	entered := make(chan struct{})
	go func() {
		release, err := b.Shared(ctx, "fixture")
		if err != nil {
			t.Error(err)
			return
		}
		close(entered)
		release()
	}()
	select {
	case <-entered:
		t.Fatal("a shared holder entered beside an exclusive one")
	case <-time.After(200 * time.Millisecond):
	}
	release()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the shared holder never entered")
	}
}
