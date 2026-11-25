package auth

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"forgetmenote/internal/pg"
	"forgetmenote/internal/pg/pgtest"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// queries counts the statements a gateway's pool sends.
type queries struct{ n atomic.Int64 }

func (q *queries) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	q.n.Add(1)
	return ctx
}
func (*queries) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// countedGateways opens two OAuth states on one database, as two gateway
// processes do, each counting the queries it sends.
func countedGateways(t *testing.T) (a, b *State, aQueries, bQueries *queries) {
	t.Helper()
	address := pgtest.URL(t, true)
	open := func() (*State, *queries) {
		config, err := pgxpool.ParseConfig(address)
		if err != nil {
			t.Fatal(err)
		}
		counted := &queries{}
		config.ConnConfig.Tracer = counted
		pool, err := pgxpool.NewWithConfig(t.Context(), config)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pool.Close)
		state, err := OpenState(&pg.DB{Pool: pool, Org: pg.DefaultOrg, Holder: pg.NewHolder()})
		if err != nil {
			t.Fatal(err)
		}
		return state, counted
	}
	a, aQueries = open()
	b, bQueries = open()
	return a, b, aQueries, bQueries
}

func TestLookupsWithinTheFreshnessWindowStayInMemory(t *testing.T) {
	a, b, _, bQueries := countedGateways(t)
	client := register(t, a, "cached")
	session, err := a.CreateSession(Grant{Subject: "alice", Expires: time.Now().Add(time.Hour).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	// A session issued elsewhere is never refused for being newer than the cache.
	if _, ok := b.Session(Digest(session)); !ok {
		t.Fatal("a session just issued on one gateway is unknown to another")
	}
	before := bQueries.n.Load()
	for range 50 {
		if _, ok := b.Session(Digest(session)); !ok {
			t.Fatal("a cached session was lost")
		}
		b.CredentialList("alice", "")
		b.Client(client.ID)
	}
	if sent := bQueries.n.Load() - before; sent > 1 {
		t.Fatalf("150 lookups within the freshness window sent %d queries", sent)
	}
}

func TestRevocationReachesAnotherGatewayWithinTheFreshnessWindow(t *testing.T) {
	a, b, _, _ := countedGateways(t)
	client := register(t, a, "shared")
	tokens := exchangeCode(t, a, client.ID, "alice")
	session, err := a.CreateSession(Grant{Subject: "alice", Expires: time.Now().Add(time.Hour).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := b.AccessGrant(Digest(tokens.Access)); !ok {
		t.Fatal("an access token issued on one gateway is unknown to another")
	}
	if _, ok := b.Session(Digest(session)); !ok {
		t.Fatal("a session issued on one gateway is unknown to another")
	}
	revoked := time.Now()
	if err := b.RevokeToken(tokens.Refresh); err != nil {
		t.Fatal(err)
	}
	if err := b.DeleteSession(Digest(session)); err != nil {
		t.Fatal(err)
	}
	// The revoking gateway's own cache changes with its write.
	if _, ok := b.AccessGrant(Digest(tokens.Access)); ok {
		t.Fatal("the revoking gateway still admits its revoked token")
	}
	if _, ok := b.Session(Digest(session)); ok {
		t.Fatal("the signing-out gateway still admits its deleted session")
	}
	for {
		_, access := a.AccessGrant(Digest(tokens.Access))
		_, signedIn := a.Session(Digest(session))
		if !access && !signedIn {
			break
		}
		if time.Since(revoked) > stateFreshness+100*time.Millisecond {
			t.Fatalf("a revocation took longer than the %v freshness window to reach another gateway", stateFreshness)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestConcurrentLookupsShareOneStaleRead(t *testing.T) {
	a, b, _, bQueries := countedGateways(t)
	session, err := a.CreateSession(Grant{Subject: "alice", Expires: time.Now().Add(time.Hour).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := b.Session(Digest(session)); !ok {
		t.Fatal("session unknown to the other gateway")
	}
	time.Sleep(stateFreshness)
	before := bQueries.n.Load()
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			if _, ok := b.Session(Digest(session)); !ok {
				t.Error("a concurrent lookup lost the session")
			}
		})
	}
	wg.Wait()
	if sent := bQueries.n.Load() - before; sent > 2 {
		t.Fatalf("32 concurrent lookups of a stale cache sent %d queries", sent)
	}
}

func TestRevokingATokenJustIssuedElsewhereRevokesIt(t *testing.T) {
	a, b, _, _ := countedGateways(t)
	client := register(t, a, "quick")
	if _, ok := b.Client(client.ID); !ok {
		t.Fatal("a registration just made on one gateway is unknown to another")
	}
	tokens := exchangeCode(t, a, client.ID, "alice")
	// The other gateway's cache predates the token; revoking must not take
	// it for an unknown one and skip the write.
	if err := b.RevokeToken(tokens.Refresh); err != nil {
		t.Fatal(err)
	}
	stored, err := OpenState(b.db)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := stored.Refresh[Digest(tokens.Refresh)]; ok {
		t.Fatal("a revocation of a just-issued token was not stored")
	}
	if _, ok := stored.Access[Digest(tokens.Access)]; ok {
		t.Fatal("a revocation of a just-issued token left its access token")
	}
}
