package search

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"forgetmenote/internal/blob"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/notestore"
	"forgetmenote/internal/pg"
	"forgetmenote/internal/pg/pgtest"
	"forgetmenote/internal/workspace"
)

// A gateway that hears of another gateway's purge removes its cache files of
// that notebook at once, without waiting to use the notebook, and keeps
// other notebooks' files.
func TestAGatewayDropsItsDerivedFilesWhenAnotherPurges(t *testing.T) {
	db := pgtest.Org(t)
	projects, err := catalog.Open(t.TempDir(), nil, db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(projects.Close)
	if err := projects.CreateKind(catalog.Bootstrap, "other", "Other", "notes"); err != nil {
		t.Fatal(err)
	}
	gateway := NewIndexes(db, projects, nil)
	t.Cleanup(gateway.Close)
	if err := gateway.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	files := map[string][]string{}
	for _, id := range []string{"default", "other"} {
		store, err := projects.Access(catalog.Bootstrap, id, "read")
		if err != nil {
			t.Fatal(err)
		}
		dir := filepath.Dir(store.Root)
		for _, name := range []string{filepath.Join(dir, "cache", "history.json"), filepath.Join(dir, "note-map.json")} {
			if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(name, []byte(`{"derived":"before the purge"}`), 0600); err != nil {
				t.Fatal(err)
			}
			files[id] = append(files[id], name)
		}
	}
	remaining := func(id string) int {
		n := 0
		for _, name := range files[id] {
			if _, err := os.Stat(name); err == nil {
				n++
			}
		}
		return n
	}
	// The listener may not be connected yet; a purge's announcement is
	// repeated here until it is heard.
	for deadline := time.Now().Add(10 * time.Second); remaining("default") > 0; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the gateway kept its derived files after another gateway's purge")
		}
		if err := db.Announce(t.Context(), purgeChannel, "default"); err != nil {
			t.Fatal(err)
		}
	}
	if remaining("other") != 2 {
		t.Fatal("another notebook's derived files were removed")
	}
}

// gateway opens a catalog and search indexes of the instance in db, as one
// gateway process does, with a notes store of its own over blobs, the
// instance's blob directory, and its own data directory.
func gateway(t *testing.T, db *pg.DB, blobs string) (*catalog.Catalog, *Indexes) {
	t.Helper()
	dir, err := blob.OpenDir(blobs)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dir.Close() })
	notes, err := notestore.Open(t.Context(), db, dir, notestore.Options{ImportEntryID: workspace.ImportEntryID})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(notes.Close)
	projects, err := catalog.OpenWithNotes(t.TempDir(), nil, db, notes)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(projects.Close)
	indexes := NewIndexes(db, projects, nil)
	t.Cleanup(indexes.Close)
	if err := indexes.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	return projects, indexes
}

// Two gateways serve one notebook. When one purges, the other stops serving
// what it cached of the purged revisions as soon as it hears of the purge,
// as a pinned read of an old revision would otherwise answer from its cache.
func TestAnotherGatewayForgetsWhatItCachedOfAPurgedRevision(t *testing.T) {
	db, blobs := pgtest.Org(t), t.TempDir()
	a, _ := gateway(t, db, blobs)
	b, _ := gateway(t, db, blobs)
	storeA, err := a.Access(catalog.Bootstrap, "default", "edit")
	if err != nil {
		t.Fatal(err)
	}
	storeB, err := b.Access(catalog.Bootstrap, "default", "read")
	if err != nil {
		t.Fatal(err)
	}
	const secret = "sk-live-gateway-0123456789abcdef"
	doc, err := storeA.Save(t.Context(), "a.md", []byte("# A\n\nThe key is "+secret+".\n"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := storeB.Read(t.Context(), "a.md", doc.Revision); err != nil || !bytes.Contains(got.Content, []byte(secret)) {
		t.Fatal("gateway b reads the revision:", err)
	}
	plan, err := storeA.PreparePurge(t.Context(), workspace.PurgeScope{Kind: workspace.PurgeText, Text: secret}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storeA.ExecutePurge(t.Context(), plan, plan.Confirmation, "tester", "leaked", nil); err != nil {
		t.Fatal(err)
	}
	// The listener may not be connected yet; the announcement is repeated
	// until it is heard.
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		got, err := storeB.Read(t.Context(), "a.md", doc.Revision)
		if err != nil || !bytes.Contains(got.Content, []byte(secret)) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("gateway b still serves the purged revision from its cache")
		}
		if err := db.Announce(t.Context(), purgeChannel, "default"); err != nil {
			t.Fatal(err)
		}
	}
}

// A gateway whose listener reconnects may have missed a purge: every store
// it serves forgets what it cached of the old revisions, not only the stores
// of its open engines. A store whose notes store has followed the purge's new
// history incarnation already serves nothing it cached before it.
func TestAGatewayForgetsEveryStoresCacheWhenItsListenerReconnects(t *testing.T) {
	db, blobs := pgtest.Org(t), t.TempDir()
	a, _ := gateway(t, db, blobs)
	b, indexesB := gateway(t, db, blobs)
	storeA, err := a.Access(catalog.Bootstrap, "default", "edit")
	if err != nil {
		t.Fatal(err)
	}
	storeB, err := b.Access(catalog.Bootstrap, "default", "read")
	if err != nil {
		t.Fatal(err)
	}
	// Gateway b's listener first connects, which counts as a reconnection,
	// before the purge: it is listening once it hears an announcement.
	derived := filepath.Join(filepath.Dir(storeB.Root), "note-map.json")
	if err := os.WriteFile(derived, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if _, err := os.Stat(derived); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("gateway b never listened")
		}
		if err := db.Announce(t.Context(), purgeChannel, "default"); err != nil {
			t.Fatal(err)
		}
	}
	const secret = "sk-live-reconnect-0123456789abcdef"
	doc, err := storeA.Save(t.Context(), "a.md", []byte("# A\n\nThe key is "+secret+".\n"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := storeB.Read(t.Context(), "a.md", doc.Revision); err != nil || !bytes.Contains(got.Content, []byte(secret)) {
		t.Fatal("gateway b reads the revision:", err)
	}
	plan, err := storeA.PreparePurge(t.Context(), workspace.PurgeScope{Kind: workspace.PurgeText, Text: secret}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The purge is never announced, as when gateway b's listener was down.
	result, err := storeA.ExecutePurge(t.Context(), plan, plan.Confirmation, "tester", "leaked", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Gateway b's notes store follows the new head by itself, and with it
	// the purge's new history incarnation: the store's cache of the old
	// revision is no longer served, before it is told to forget it.
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if status, err := storeB.Status(t.Context()); err == nil && status.Revision == result.Revision {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("gateway b never saw the purged head")
		}
	}
	if got, err := storeB.Read(t.Context(), "a.md", doc.Revision); err == nil && bytes.Contains(got.Content, []byte(secret)) {
		t.Fatal("gateway b serves the purged revision from its cache after it followed the purged head")
	}
	indexesB.heardPurge("")
	if got, err := storeB.Read(t.Context(), "a.md", doc.Revision); err == nil && bytes.Contains(got.Content, []byte(secret)) {
		t.Fatal("gateway b still serves the purged revision from its cache after its listener reconnected")
	}
}

// A gateway whose own clean-up of a purge failed still tells the others,
// which drop what they derived.
func TestAPurgeIsAnnouncedWhenItsCleanUpFails(t *testing.T) {
	db, blobs := pgtest.Org(t), t.TempDir()
	a, indexesA := gateway(t, db, blobs)
	b, _ := gateway(t, db, blobs)
	storeA, err := a.Access(catalog.Bootstrap, "default", "edit")
	if err != nil {
		t.Fatal(err)
	}
	storeB, err := b.Access(catalog.Bootstrap, "default", "read")
	if err != nil {
		t.Fatal(err)
	}
	derived := filepath.Join(filepath.Dir(storeB.Root), "note-map.json")
	if err := os.WriteFile(derived, []byte(`{"derived":"before the purge"}`), 0600); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		// An engine refuses a clean-up without the purge's ID.
		if _, err := indexesA.Purged(t.Context(), storeA, "", nil, true); err == nil {
			t.Fatal("the clean-up did not fail")
		}
		if _, err := os.Stat(derived); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the other gateway was not told of the purge")
		}
	}
}
