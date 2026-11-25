package auth

import (
	"context"
	"errors"
	"strings"
	"testing"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/pg/pgtest"
	"forgetmenote/internal/workspace"
)

func TestTrustedProxyRequiresSocketPeerSecretAndValidIdentity(t *testing.T) {
	secret := strings.Repeat("synthetic-proxy-secret", 2)
	proxy, err := NewProxy(secret, []string{"192.0.2.0/24"})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		peer, secret, user string
		allowed            bool
	}{
		{"192.0.2.3:1234", secret, "alice", true},
		{"[::ffff:192.0.2.3]:1234", secret, "alice", true},
		{"198.51.100.3:1234", secret, "alice", false},
		{"192.0.2.3:1234", secret + "x", "alice", false},
		{"192.0.2.3:1234", secret, "../alice", false},
		{"192.0.2.3", secret, "alice", false},
	} {
		identity := proxy.Identity(test.peer, test.secret, test.user)
		if (identity != "") != test.allowed {
			t.Fatalf("peer=%q user=%q allowed=%v", test.peer, test.user, identity != "")
		}
	}
}

func TestCredentialScopesAudienceIsolationAndDurableRevocation(t *testing.T) {
	dir := t.TempDir()
	projects, err := catalog.Open(dir, nil, pgtest.Dir(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	defer projects.Close()
	state, err := OpenState(pgtest.Database(t))
	if err != nil {
		t.Fatal(err)
	}
	spec := CredentialSpec{Name: "reader", Projects: []string{"default"}, Scopes: []string{"notes:read"}, Audiences: []string{"api"}, Days: 1}
	issued, err := IssueCredentialFor(t.Context(), projects, state, catalog.Bootstrap, catalog.Bootstrap, spec)
	if err != nil {
		t.Fatal(err)
	}
	// Neither caller-owned inputs nor returned discovery slices may widen a grant.
	spec.Projects[0] = "outside"
	issued.Scopes[0] = "notes:write"
	if _, ok := state.CredentialToken(projects, issued.Token, "mcp"); ok {
		t.Fatal("API grant accepted at MCP audience")
	}
	grant, ok := state.CredentialToken(projects, issued.Token, "api")
	if !ok {
		t.Fatal("issued credential unavailable")
	}
	ctx := context.WithValue(t.Context(), CredentialKey{}, grant)
	ctx = context.WithValue(ctx, ScopeKey{}, strings.Join(grant.Grant.Scopes, " "))
	if !HasScope(ctx, "notes:read") || HasScope(ctx, "notes:write") || !ProjectAllowed(ctx, "default") || ProjectAllowed(ctx, "outside") {
		t.Fatal("credential restrictions changed")
	}
	grant.Grant.Projects[0] = "outside"
	current, _ := state.CredentialToken(projects, issued.Token, "api")
	if current.Grant.Projects[0] != "default" {
		t.Fatal("discovery widened persisted grant")
	}
	if err := state.RevokeCredential(projects, catalog.Bootstrap, catalog.Bootstrap, issued.ID); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(state.CheckCredential(ctx, projects), workspace.ErrForbidden) {
		t.Fatal("admitted context survived revocation")
	}
	reopened, err := OpenState(state.db)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reopened.CredentialToken(projects, issued.Token, "api"); ok {
		t.Fatal("credential revived after restart")
	}
}
