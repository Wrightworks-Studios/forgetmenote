package knowledge

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"forgetmenote/internal/workspace"
	"forgetmenote/internal/workspace/workspacetest"
)

// After a purge, the engine that cleans up drops the notebook's vectors and
// cache files and records the purge; every other engine of the notebook, as
// on another gateway, forgets what it derived when it next reads the state,
// and no cache written before the purge is loaded again.
func TestPurgedClearsWhatEveryEngineDerived(t *testing.T) {
	ctx := context.Background()
	store, _ := workspacetest.New(t, filepath.Join(t.TempDir(), "keys"), nil)
	const secret = "sk-live-knowledge-0123456789"
	first, err := store.Save(ctx, "a.md", []byte("# Alpha "+secret+"\n\nThe key is "+secret+".\n"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save(ctx, "a.md", []byte("# Alpha "+secret+"\n\nThe key is "+secret+", still.\n"), first.Revision, ""); err != nil {
		t.Fatal(err)
	}
	a, err := Open(database(t, store), store)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Open(database(t, store), store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Configure(ctx, testConfig(), false); err != nil {
		t.Fatal(err)
	}
	if fillIndex(t, a, "current") == 0 || fillIndex(t, a, "history") == 0 {
		t.Fatal("nothing was indexed")
	}
	if _, err := b.Map(ctx, MapQuery{}); err != nil {
		t.Fatal(err)
	}
	if pending, err := b.PendingScope(ctx, 64, "history"); err != nil || len(b.history.Blobs) == 0 {
		t.Fatal("b's history:", pending, err)
	}
	cache := filepath.Join(a.dir, "cache", "history.json")
	stale, err := os.ReadFile(cache)
	if err != nil || !bytes.Contains(stale, []byte(secret)) {
		t.Fatal("the history cache was not written:", err)
	}

	scope := workspace.PurgeScope{Kind: workspace.PurgeText, Text: secret}
	plan, err := store.PreparePurge(ctx, scope, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := store.ExecutePurge(ctx, plan, plan.Confirmation, "tester", "leaked", nil)
	if err != nil {
		t.Fatal(err)
	}
	cleanup, err := a.Purged(ctx, result.Purge, nil, true)
	if err != nil || cleanup.Vectors == 0 {
		t.Fatal("purged:", cleanup, err)
	}
	if n := len(storedVectors(t, a)); n != 0 {
		t.Fatalf("%d vectors remain", n)
	}
	for _, name := range []string{cache, filepath.Join(a.dir, "note-map.json")} {
		if _, err := os.Stat(name); !os.IsNotExist(err) {
			t.Fatalf("%s remains: %v", name, err)
		}
	}
	if a.state.Purge != result.Purge || len(a.history.Blobs) != 0 || a.passages != nil {
		t.Fatal("a kept what it derived")
	}
	// b, as another gateway, forgets on its next read of the state.
	if _, err := b.Pending(ctx, 64); err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	held := len(b.history.Blobs) > 0 || b.mapCache != nil
	for _, p := range b.passages {
		held = held || strings.Contains(p.Text, secret)
	}
	b.mu.Unlock()
	if held {
		t.Fatal("another engine kept what it derived before the purge")
	}
	// A cache written before the purge is never loaded.
	if err := os.WriteFile(cache, stale, 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Open(database(t, store), store)
	if err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	visibility, _ := store.HistoryVisibilityID(ctx)
	c.loadHistory(visibility)
	loaded := len(c.history.Blobs)
	c.mu.Unlock()
	if loaded != 0 {
		t.Fatal("a history cache from before the purge was loaded")
	}
	// It is deleted, as a gateway that was down during the purge finds it.
	if _, err := os.Stat(cache); !os.IsNotExist(err) {
		t.Fatal("a history cache from before the purge remains:", err)
	}
	raw, _ := json.Marshal(c.state)
	if bytes.Contains(raw, []byte(secret)) {
		t.Fatal("the state holds the secret")
	}
}
