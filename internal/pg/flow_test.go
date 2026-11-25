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

func TestFlowsCompleteOnceOnAnyGatewayWithinTheirGroupBound(t *testing.T) {
	a := pgtest.Org(t)
	b, _ := pgtest.Gateway(t, a)
	ctx := context.Background()
	if err := a.BeginFlow(ctx, "oidc/start", "state-one", "alice", []byte(`{"n":1}`), time.Minute, pg.FlowBound{Limit: 2, Replace: true}); err != nil {
		t.Fatal(err)
	}
	// A subject's new flow replaces its earlier one in the group.
	if err := a.BeginFlow(ctx, "oidc/start", "state-two", "alice", []byte(`{"n":2}`), time.Minute, pg.FlowBound{Limit: 2, Replace: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.TakeFlow(ctx, "oidc/start", "state-one"); !errors.Is(err, pg.ErrNoFlow) {
		t.Fatal("a replaced flow completed", err)
	}
	if err := a.BeginFlow(ctx, "oidc/start", "state-three", "bob", []byte(`{}`), time.Minute, pg.FlowBound{Limit: 2, Replace: true}); err != nil {
		t.Fatal(err)
	}
	if err := a.BeginFlow(ctx, "oidc/link", "state-four", "carol", []byte(`{}`), time.Minute, pg.FlowBound{Limit: 2, Replace: true}); !errors.Is(err, pg.ErrFlowsFull) {
		t.Fatal("a group exceeded its bound", err)
	}
	body, err := b.TakeFlow(ctx, "oidc/start", "state-two")
	if err != nil || string(body) != `{"n": 2}` && string(body) != `{"n":2}` {
		t.Fatalf("another gateway could not complete the flow: %s %v", body, err)
	}
	if _, err = a.TakeFlow(ctx, "oidc/start", "state-two"); !errors.Is(err, pg.ErrNoFlow) {
		t.Fatal("a flow completed twice", err)
	}
	if err = a.PutFlow(ctx, "oidc/grant", "binding", "bob", []byte(`{"step":2}`), time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err = b.ReadFlow(ctx, "oidc/grant", "binding"); !errors.Is(err, pg.ErrNoFlow) {
		t.Fatal("an expired flow was read", err)
	}
	if _, err = b.TakeFlow(ctx, "oidc/grant", "binding"); !errors.Is(err, pg.ErrNoFlow) {
		t.Fatal("an expired flow completed", err)
	}
	// Anonymous starts evict the soonest-expiring flow instead of refusing.
	evict := pg.FlowBound{Limit: 2, Evict: true}
	if err = a.BeginFlow(ctx, "login/start", "soon", "", []byte(`{}`), time.Minute, evict); err != nil {
		t.Fatal(err)
	}
	if err = a.BeginFlow(ctx, "login/start", "later", "", []byte(`{}`), time.Hour, evict); err != nil {
		t.Fatal(err)
	}
	if err = b.BeginFlow(ctx, "login/start", "newest", "", []byte(`{}`), time.Hour, evict); err != nil {
		t.Fatal(err)
	}
	if _, err = a.ReadFlow(ctx, "login/start", "soon"); !errors.Is(err, pg.ErrNoFlow) {
		t.Fatal("a full anonymous group kept its soonest-expiring flow", err)
	}
	for _, key := range []string{"later", "newest"} {
		if _, err = a.ReadFlow(ctx, "login/start", key); err != nil {
			t.Fatal("eviction removed a later flow", key, err)
		}
	}
	// Steps applied on two gateways serialize on the row.
	if err = a.BeginFlow(ctx, "setup/ticket", "ticket", "dana", []byte(`0`), time.Minute, pg.FlowBound{Limit: 4}); err != nil {
		t.Fatal(err)
	}
	var steps sync.WaitGroup
	for i := range 6 {
		steps.Add(1)
		go func() {
			defer steps.Done()
			db := a
			if i%2 == 1 {
				db = b
			}
			if err := db.UpdateFlow(ctx, "setup/ticket", "ticket", func(body []byte) ([]byte, error) {
				return []byte{body[0] + 1}, nil
			}); err != nil {
				t.Error(err)
			}
		}()
	}
	steps.Wait()
	if body, err := b.ReadFlow(ctx, "setup/ticket", "ticket"); err != nil || string(body) != "6" {
		t.Fatalf("concurrent steps were lost: %q %v", body, err)
	}
	if err = a.UpdateFlow(ctx, "setup/missing", "ticket", func(body []byte) ([]byte, error) { return body, nil }); !errors.Is(err, pg.ErrNoFlow) {
		t.Fatal("a missing flow was updated", err)
	}
	if err = a.EndFlows(ctx, "oidc/start", "bob"); err != nil {
		t.Fatal(err)
	}
	if _, err = a.TakeFlow(ctx, "oidc/start", "state-three"); !errors.Is(err, pg.ErrNoFlow) {
		t.Fatal("an ended flow completed", err)
	}
}

func TestRateWindowsHoldAcrossGateways(t *testing.T) {
	a := pgtest.Org(t)
	b, _ := pgtest.Gateway(t, a)
	ctx := context.Background()
	var allowed atomic.Int32
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			db := a
			if i%2 == 1 {
				db = b
			}
			ok, err := db.Take(ctx, "password", []string{"all", "user:alice"}, []int{100, 3}, 16)
			if err != nil {
				t.Error(err)
			}
			if ok {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()
	if allowed.Load() != 3 {
		t.Fatalf("%d requests passed a limit of 3 across gateways", allowed.Load())
	}
	if err := b.Refund(ctx, "password", []string{"all", "user:alice"}); err != nil {
		t.Fatal(err)
	}
	if ok, err := a.Take(ctx, "password", []string{"all", "user:alice"}, []int{100, 3}, 16); err != nil || !ok {
		t.Fatal("a refunded charge was not available again", ok, err)
	}
	if ok, err := a.Take(ctx, "password", []string{"all", "user:bob"}, []int{100, 3}, 16); err != nil || !ok {
		t.Fatal("another account was limited by alice's window", ok, err)
	}
	// A full table refuses new windows without charging.
	if ok, err := a.Take(ctx, "password", []string{"user:carol"}, []int{3}, 3); err != nil || ok {
		t.Fatal("a request was charged past the window capacity", ok, err)
	}
	if _, err := a.Pool.Exec(ctx, `UPDATE rate_windows SET started = now() - interval '61 seconds' WHERE org = $1`, a.Org); err != nil {
		t.Fatal(err)
	}
	if ok, err := b.Take(ctx, "password", []string{"all", "user:alice"}, []int{100, 3}, 16); err != nil || !ok {
		t.Fatal("an elapsed window still limited requests", ok, err)
	}
}

// An expired flow nobody finishes is removed by the sweep, whatever its
// kind, without another flow of its group beginning; one found expired when
// read is removed then. Live flows and other orgs' flows stay.
func TestExpiredFlowsAreSweptWithoutAnotherBeginning(t *testing.T) {
	a := pgtest.Org(t)
	b, _ := pgtest.Gateway(t, a)
	ctx := context.Background()
	expired := time.Now().Add(-time.Second)
	for _, kind := range []string{"purge-plan-default", "oidc/start"} {
		if err := a.PutFlow(ctx, kind, "old", "alice", []byte(`{"text":"secret"}`), expired); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.PutFlow(ctx, "purge-plan-default", "live", "alice", []byte(`{}`), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := a.PutFlow(ctx, "login/start", "read", "", []byte(`{}`), expired); err != nil {
		t.Fatal(err)
	}
	if _, err := b.ReadFlow(ctx, "login/start", "read"); !errors.Is(err, pg.ErrNoFlow) {
		t.Fatal("an expired flow was read", err)
	}
	count := func(where string) int {
		t.Helper()
		var n int
		if err := a.Pool.QueryRow(ctx, `SELECT count(*) FROM pending_flows WHERE org = $1 AND `+where, a.Org).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(`kind = 'login/start'`); n != 0 {
		t.Fatal("an expired flow that was read stayed stored")
	}
	removed, err := b.SweepFlows(ctx)
	if err != nil || removed != 2 {
		t.Fatalf("swept %d flows: %v", removed, err)
	}
	if n := count(`true`); n != 1 || count(`key = 'live'`) != 1 {
		t.Fatalf("%d flows remain after the sweep", n)
	}
}
