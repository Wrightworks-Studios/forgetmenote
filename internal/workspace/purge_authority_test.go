package workspace

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"forgetmenote/internal/pg/pgtest"
)

// stalledGatewayPurge has gateway B check and start a purge of a key from a
// note large enough that the rewrite stages its redacted version before its
// transaction, and holds the purge in that staging, as a slow blob store
// would, until release. It returns a's head.
func stalledGatewayPurge(t *testing.T, a *Store) (head string, release func(), done <-chan error) {
	t.Helper()
	ctx := context.Background()
	key := "sk-fence-" + strings.Repeat("7", 24)
	saved, err := a.Save(ctx, "keys.md", []byte(strings.Repeat("A long note line.\n", 2000)+"The key is "+key+".\n"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := OpenDB(ctx, openTestNotes(t, testEnv(t, a)), "notes", t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	for {
		if current, err := b.Status(ctx); err == nil && current.Revision == saved.Revision {
			break
		}
		time.Sleep(time.Millisecond)
	}
	plan, err := b.PreparePurge(ctx, PurgeScope{Kind: PurgeText, Text: key}, nil)
	if err != nil || plan.Blocked != "" || len(plan.Notes) != 1 {
		t.Fatalf("plan: %+v %v", plan, err)
	}
	waiting, release := pgtest.HoldWrites(t, testEnv(t, a).db, "content_blocks")
	result := make(chan error, 1)
	go func() {
		_, err := b.ExecutePurge(ctx, plan, plan.Confirmation, "owner", "a leaked key", nil)
		result <- err
	}()
	waiting(1)
	return saved.Revision, release, result
}

// purgeCount counts the purges the notebook recorded.
func purgeCount(t *testing.T, s *Store) int {
	t.Helper()
	env := testEnv(t, s)
	var n int
	if err := env.db.Pool.QueryRow(context.Background(), `SELECT count(*) FROM purges WHERE org = $1`, env.db.Org).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A purge another gateway checked before a handoff was prepared fails
// rather than rewriting the history the handoff publishes, and the handoff
// completes at the revision it prepared.
func TestPurgeCheckedBeforeAHandoffCannotRewriteItsPreparation(t *testing.T) {
	a := newTestStore(t)
	ctx := context.Background()
	head, release, purged := stalledGatewayPurge(t, a)
	if _, err := a.PrepareAuthority(ctx, AuthorityRequest{ID: filingHandoff, Generation: 1, From: "forgetmenote", To: "github", Revision: head}); err != nil {
		t.Fatal(err)
	}
	release()
	if err := <-purged; !errors.Is(err, ErrForbidden) {
		t.Fatal("a purge rewrote a notebook whose handoff was prepared:", err)
	}
	if n := purgeCount(t, a); n != 0 {
		t.Fatalf("%d purges recorded", n)
	}
	state, err := a.FinishAuthority(ctx, filingHandoff, true)
	if err != nil || state.Type != "github" || state.Committed != head {
		t.Fatalf("the handoff after the refused purge: %+v %v", state, err)
	}
	if current := headOf(t, reopenTestStore(t, a)); current != head {
		t.Fatalf("the head moved from %s to %s", head, current)
	}
}

// A purge another gateway checked before a handoff to GitHub completed
// fails as read-only rather than rewriting the GitHub notebook's history.
func TestPurgeCheckedBeforeAHandoffCannotRewriteAfterIt(t *testing.T) {
	a := newTestStore(t)
	ctx := context.Background()
	head, release, purged := stalledGatewayPurge(t, a)
	if _, err := a.PrepareAuthority(ctx, AuthorityRequest{ID: filingHandoff, Generation: 1, From: "forgetmenote", To: "github", Revision: head}); err != nil {
		t.Fatal(err)
	}
	if state, err := a.FinishAuthority(ctx, filingHandoff, true); err != nil || state.Committed != head {
		t.Fatalf("handoff: %+v %v", state, err)
	}
	release()
	if err := <-purged; !errors.Is(err, ErrReadOnly) {
		t.Fatal("a purge rewrote a notebook handed to GitHub:", err)
	}
	if n := purgeCount(t, a); n != 0 {
		t.Fatalf("%d purges recorded", n)
	}
	if current := headOf(t, reopenTestStore(t, a)); current != head {
		t.Fatalf("the GitHub notebook's head moved from %s to %s", head, current)
	}
}
