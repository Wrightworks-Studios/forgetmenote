package auth

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"forgetmenote/internal/pg/pgtest"
)

type allowAuthority struct{}

func (allowAuthority) CheckSession(string, uint64) error { return nil }

func TestMain(m *testing.M) { pgtest.Main(m) }

func openTestState(t *testing.T) *State {
	t.Helper()
	state, err := OpenState(pgtest.Database(t))
	if err != nil {
		t.Fatal(err)
	}
	return state
}

// storedState opens state from a raw document, as an earlier release stored it.
func storedState(t *testing.T, raw string) (*State, error) {
	t.Helper()
	db := pgtest.Database(t)
	if _, err := db.WriteDocument(t.Context(), db.Pool, stateKind, stateID, 0, []byte(raw)); err != nil {
		t.Fatal(err)
	}
	return OpenState(db)
}

// register bypasses only the per-minute registration rate.
func register(t *testing.T, state *State, name string) Client {
	t.Helper()
	state.Window = time.Now().Add(-2 * time.Minute)
	c, err := state.RegisterClient(name, []string{"http://127.0.0.1:5555/cb"})
	if err != nil {
		t.Fatal(name, err)
	}
	return c
}

func age(state *State, id string, by time.Duration) {
	c := state.Clients[id]
	c.Created = time.Now().Add(-by).Unix()
	state.Clients[id] = c
}

func exchangeCode(t *testing.T, state *State, client, subject string) TokenResponse {
	t.Helper()
	verifier := strings.Repeat("v", 43)
	sum := sha256.Sum256([]byte(verifier))
	code, err := state.IssueCode(GrantRequest{Client: client, Redirect: "http://127.0.0.1:5555/cb", Challenge: base64.RawURLEncoding.EncodeToString(sum[:]), Scope: "notes:read", Resource: "https://notes.example/mcp", Subject: subject, Expires: time.Now().Add(time.Minute).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	out, err := state.Exchange(allowAuthority{}, url.Values{"grant_type": {"authorization_code"}, "client_id": {client}, "redirect_uri": {"http://127.0.0.1:5555/cb"}, "code": {code}, "code_verifier": {verifier}})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestUnexchangedClientRegistrationsExpireAfterAnHour(t *testing.T) {
	state := openTestState(t)
	connected := register(t, state, "connected")
	exchangeCode(t, state, connected.ID, "alice")
	age(state, connected.ID, 2*time.Hour)
	for i := 0; i < 255; i++ {
		age(state, register(t, state, fmt.Sprint("junk-", i)).ID, 2*time.Hour)
	}
	legit := register(t, state, "legit")
	if len(state.Clients) != 2 {
		t.Fatalf("unexchanged registrations held capacity: %d clients", len(state.Clients))
	}
	if _, ok := state.Clients[connected.ID]; !ok {
		t.Fatal("a client that completed authorization was pruned")
	}
	if !state.Clients[legit.ID].Pending {
		t.Fatal("new registration is not marked pending")
	}
	reopened, err := OpenState(state.db)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Clients[connected.ID].Pending || !reopened.Clients[legit.ID].Pending {
		t.Fatal("pending registration marker did not persist")
	}
}

func TestLegacyClientRegistrationsAreNotTreatedAsPending(t *testing.T) {
	created := time.Now().Add(-2 * time.Hour).Unix()
	raw := fmt.Sprintf(`{"clients":{"legacy":{"client_id":"legacy","client_name":"Old","redirect_uris":["http://127.0.0.1:5555/cb"],"created":%d}},"access":{},"refresh":{}}`, created)
	state, err := storedState(t, raw)
	if err != nil {
		t.Fatal(err)
	}
	register(t, state, "new")
	if _, ok := state.Clients["legacy"]; !ok {
		t.Fatal("pre-existing registration was pruned as unexchanged")
	}
}

func TestFullRegistrationTableEvictsOldestUnexchangedClient(t *testing.T) {
	state := openTestState(t)
	first := register(t, state, "first")
	age(state, first.ID, 10*time.Minute)
	for i := 1; i < 256; i++ {
		register(t, state, fmt.Sprint("pending-", i))
	}
	newest := register(t, state, "newest")
	if _, ok := state.Clients[first.ID]; ok || len(state.Clients) != 256 {
		t.Fatalf("oldest pending kept=%v size=%d", ok, len(state.Clients))
	}
	if _, ok := state.Clients[newest.ID]; !ok {
		t.Fatal("new registration missing")
	}
	for id, c := range state.Clients {
		c.Pending = false
		state.Clients[id] = c
	}
	state.Window = time.Now().Add(-2 * time.Minute)
	if _, err := state.RegisterClient("refused", []string{"http://127.0.0.1:5555/cb"}); !errors.Is(err, ErrRegistrationCapacity) {
		t.Fatal("connected clients were evicted", err)
	}
}

func TestOneAccountCannotHoldEveryRefreshToken(t *testing.T) {
	state := openTestState(t)
	now := time.Now().Unix()
	for i := 0; i < 64; i++ {
		state.Refresh[Digest(fmt.Sprint("alice-", i))] = Grant{Subject: "alice", Client: "c", Family: fmt.Sprint("family-", i), FamilyExpires: now + 86400 + int64(i), Expires: now + 3600}
	}
	state.Access[Digest("alice-access-0")] = Grant{Subject: "alice", Client: "c", Family: "family-0", FamilyExpires: now + 86400, Expires: now + 3600}
	state.Credentials[Digest("fmn_minted")] = APICredential{CredentialSpec: CredentialSpec{Name: "minted", Projects: []string{"default"}, Scopes: []string{"notes:read"}, Audiences: []string{"api"}, Days: 1}, ID: "minted", Subject: "alice", Created: now, Expires: now + 3600, FamilyDigest: Digest("family-0")}
	state.Refresh[Digest("bob")] = Grant{Subject: "bob", Client: "c", Family: "bob-family", FamilyExpires: now + 10, Expires: now + 10}
	client := register(t, state, "client")
	exchangeCode(t, state, client.ID, "alice")
	held := 0
	for _, g := range state.Refresh {
		if g.Subject == "alice" {
			held++
		}
		if g.Family == "family-0" {
			t.Fatal("oldest authorization kept beyond the account limit")
		}
	}
	if held != 64 {
		t.Fatalf("account holds %d refresh tokens", held)
	}
	if _, ok := state.Refresh[Digest("bob")]; !ok {
		t.Fatal("another account's authorization was evicted")
	}
	if _, ok := state.Access[Digest("alice-access-0")]; ok {
		t.Fatal("evicted authorization kept its access token")
	}
	if _, ok := state.Credentials[Digest("fmn_minted")]; ok {
		t.Fatal("evicted authorization kept its minted credential")
	}
	if _, ok := state.Used[Digest("alice-0")]; ok {
		t.Fatal("capacity eviction recorded a refresh replay")
	}
}
