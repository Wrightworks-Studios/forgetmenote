package auth

import (
	"errors"
	"net/url"
	"sync"
	"testing"
	"time"

	"forgetmenote/internal/pg"
	"forgetmenote/internal/pg/pgtest"
)

// gateways opens two OAuth states on one database, as two gateway processes do.
func gateways(t *testing.T) (*State, *State) {
	t.Helper()
	address := pgtest.URL(t, true)
	open := func() *State {
		db, err := pg.Open(t.Context(), address, "")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(db.Close)
		state, err := OpenState(db)
		if err != nil {
			t.Fatal(err)
		}
		return state
	}
	return open(), open()
}

func TestAuthorizationStartedOnOneGatewayFinishesAndRevokesOnAnother(t *testing.T) {
	a, b := gateways(t)
	client := register(t, a, "shared")
	if _, ok := b.Client(client.ID); !ok {
		t.Fatal("a registration on one gateway is unknown to another")
	}
	tokens := exchangeCode(t, b, client.ID, "alice")
	// Another gateway's changes reach a cached registration within the window.
	time.Sleep(stateFreshness)
	if c, ok := a.Client(client.ID); !ok || c.Pending {
		t.Fatal("the other gateway did not see the completed exchange")
	}
	if _, ok := a.AccessGrant(Digest(tokens.Access)); !ok {
		t.Fatal("an access token issued on one gateway is unknown to another")
	}
	session, err := a.CreateSession(Grant{Subject: "alice", Expires: time.Now().Add(time.Hour).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.RevokeToken(tokens.Refresh); err != nil {
		t.Fatal(err)
	}
	if err := b.DeleteSession(Digest(session)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(stateFreshness)
	if _, ok := a.AccessGrant(Digest(tokens.Access)); ok {
		t.Fatal("a revocation on one gateway left the token valid on another")
	}
	if _, ok := a.Session(Digest(session)); ok {
		t.Fatal("a sign-out on one gateway left the session valid on another")
	}
}

func TestConcurrentRefreshOnTwoGatewaysKeepsTheReuseGrace(t *testing.T) {
	a, b := gateways(t)
	client := register(t, a, "racing")
	tokens := exchangeCode(t, a, client.ID, "alice")
	form := url.Values{"grant_type": {"refresh_token"}, "client_id": {client.ID}, "refresh_token": {tokens.Refresh}}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, state := range []*State{a, b} {
		wg.Add(1)
		go func(i int, state *State) { defer wg.Done(); _, errs[i] = state.Exchange(allowAuthority{}, form) }(i, state)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatalf("a retried refresh inside the grace window failed: %v", errs)
		}
	}
	if _, active := b.FamilyGrant(a.Access[Digest(tokens.Access)].Family); !active {
		t.Fatal("a retried refresh revoked its authorization")
	}
}

func TestStateFailsClosedWhileTheDatabaseIsUnavailable(t *testing.T) {
	a, b := gateways(t)
	session, err := a.CreateSession(Grant{Subject: "alice", Expires: time.Now().Add(time.Hour).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := b.Session(Digest(session)); !ok {
		t.Fatal("session unknown to the other gateway")
	}
	b.db.Pool.Close()
	time.Sleep(stateFreshness)
	if _, ok := b.Session(Digest(session)); ok {
		t.Fatal("an unreachable database left a session cached past the freshness window valid")
	}
	if _, err := b.CreateSession(Grant{Subject: "alice", Expires: time.Now().Add(time.Hour).Unix()}); !errors.Is(err, ErrTemporarilyUnavailable) {
		t.Fatalf("a write without a database = %v", err)
	}
	if _, ok := a.Session(Digest(session)); !ok {
		t.Fatal("the reachable gateway lost the session")
	}
}
