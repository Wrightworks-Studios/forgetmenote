package web

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"forgetmenote/internal/auth"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/pg"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// authQueries counts the queries of the sign-in state document.
type authQueries struct{ n atomic.Int64 }

func (q *authQueries) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "documents") && len(data.Args) > 1 && data.Args[1] == "oauth" {
		q.n.Add(1)
	}
	return ctx
}
func (*authQueries) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// countAuthQueries reopens app's sign-in state on a pool that counts them.
func countAuthQueries(t *testing.T, app *Server) *authQueries {
	t.Helper()
	config := app.db.Pool.Config()
	counted := &authQueries{}
	config.ConnConfig.Tracer = counted
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if app.oauth, err = auth.OpenState(&pg.DB{Pool: pool, Org: app.db.Org, Holder: app.db.Holder}); err != nil {
		t.Fatal(err)
	}
	return counted
}

func TestAPICredentialIsLookedUpAtAdmissionAndDisclosureOnly(t *testing.T) {
	app, store := fixture(t)
	if _, err := store.Save(t.Context(), "note.md", []byte("# Note\n"), "", "test"); err != nil {
		t.Fatal(err)
	}
	for i := range 4 {
		if err := app.catalog.Create(catalog.Bootstrap, fmt.Sprintf("extra%d", i), fmt.Sprintf("Extra %d", i)); err != nil {
			t.Fatal(err)
		}
	}
	reader, err := issueCredential(app.catalog, app.oauth, catalog.Bootstrap, CredentialSpec{Name: "reader", AllProjects: true, Scopes: []string{"notes:read"}, Audiences: []string{"api"}, Days: 1})
	if err != nil {
		t.Fatal(err)
	}
	writer, err := issueCredential(app.catalog, app.oauth, catalog.Bootstrap, CredentialSpec{Name: "writer", AllProjects: true, Scopes: []string{"notes:read", "notes:write"}, Audiences: []string{"api"}, Days: 1})
	if err != nil {
		t.Fatal(err)
	}
	calls := []struct{ name, method, path, body, token string }{
		{"read_note", "POST", "/api/v1/actions/read_note", `{"project":"default","path":"note.md"}`, reader.Token},
		{"capabilities, 5 notebooks, read", "GET", "/api/v1/capabilities", "", reader.Token},
		{"capabilities, 5 notebooks, read+write", "GET", "/api/v1/capabilities", "", writer.Token},
	}
	measure := func(token, method, path, body string, queries *authQueries) int64 {
		before := queries.n.Load()
		if w := apiCall(app, method, path, body, token); w.Code != 200 {
			t.Fatal(path, w.Code, w.Body.String())
		}
		return queries.n.Load() - before
	}
	queries := countAuthQueries(t, app)
	// With no freshness window every lookup reaches the database.
	app.oauth.SetFreshness(0)
	for _, c := range calls {
		lookups := measure(c.token, c.method, c.path, c.body, queries)
		t.Logf("%s: %d credential lookups", c.name, lookups)
		if lookups > 2 {
			t.Errorf("%s looked its credential up %d times, beyond admission and disclosure", c.name, lookups)
		}
	}
	queries = countAuthQueries(t, app)
	for _, c := range calls {
		time.Sleep(250 * time.Millisecond)
		stale := measure(c.token, c.method, c.path, c.body, queries)
		warm := measure(c.token, c.method, c.path, c.body, queries)
		t.Logf("%s: %d auth queries from a stale cache, %d from a warm one", c.name, stale, warm)
		if stale > 2 {
			t.Errorf("%s sent %d auth queries", c.name, stale)
		}
	}
}

func TestRestoredCredentialIsLookedUpAgain(t *testing.T) {
	app, _ := fixture(t)
	grant, err := issueCredential(app.catalog, app.oauth, catalog.Bootstrap, CredentialSpec{Name: "reader", Projects: []string{"default"}, Scopes: []string{"notes:read"}, Audiences: []string{"api"}, Days: 1})
	if err != nil {
		t.Fatal(err)
	}
	admitted, ok := app.oauth.CredentialToken(app.catalog, grant.Token, "api")
	if !ok || !admitted.Admitted() {
		t.Fatal("admission did not resolve the credential")
	}
	// Persisted work keeps the credential's identity, never its admission.
	raw, err := json.Marshal(admitted)
	if err != nil {
		t.Fatal(err)
	}
	var restored credentialContext
	if err = json.Unmarshal(raw, &restored); err != nil || restored.Admitted() || restored.Hash != admitted.Hash {
		t.Fatalf("restored credential %+v, %v", restored, err)
	}
	if err = app.oauth.RevokeCredential(app.catalog, catalog.Bootstrap, catalog.Bootstrap, grant.ID); err != nil {
		t.Fatal(err)
	}
	restoredCtx := context.WithValue(context.WithValue(t.Context(), identityKey{}, catalog.Bootstrap), credentialContextKey{}, restored)
	if app.clientProjectAllowed(restoredCtx, "default") || len(app.clientProjects(restoredCtx, catalog.Bootstrap)) != 0 {
		t.Fatal("persisted work kept using a revoked credential")
	}
	admittedCtx := context.WithValue(context.WithValue(t.Context(), identityKey{}, catalog.Bootstrap), credentialContextKey{}, admitted)
	if app.checkCredential(admittedCtx) == nil {
		t.Fatal("a disclosure accepted a credential revoked during its invocation")
	}
}
