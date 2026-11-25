package knowledge

import (
	"context"
	"encoding/json"
	"errors"
	"forgetmenote/internal/pg/pgtest"
	"forgetmenote/internal/workspace"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestRetrievalDefaultsCASPersistenceAndFailures(t *testing.T) {
	e, _ := indexFixture(t)
	if version, _ := storedDocument(t, e, retrievalKind); version != 0 {
		t.Fatalf("missing defaults wrote settings: version %d", version)
	}
	before := e.RetrievalSettings()
	config := e.state.Config.ID()
	a, b := before.Settings, before.Settings
	a.Search.Archives = "include"
	b.Search.Drafts = "exclude"
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, settings := range []RetrievalSettings{a, b} {
		wg.Add(1)
		go func(s RetrievalSettings) {
			defer wg.Done()
			<-start
			_, err := e.SetRetrievalSettings(s, before.ID)
			results <- err
		}(settings)
	}
	close(start)
	wg.Wait()
	close(results)
	success, conflicts := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, workspace.ErrConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflicts != 1 {
		t.Fatalf("CAS: %d success %d conflicts", success, conflicts)
	}
	saved := e.RetrievalSettings()
	version, raw := storedDocument(t, e, retrievalKind)
	reopened, err := Open(database(t, e.Store), e.Store)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.RetrievalSettings() != saved || e.state.Config.ID() != config {
		t.Fatal("settings restart changed scope or vector configuration")
	}
	explicit, err := e.ResolveContentScope(ContainerSelection{Archives: "include", Drafts: "include"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if explicit.Policy.Archives != "include" || explicit.Policy.Drafts != "include" {
		t.Fatal("defaults treated as restrictions")
	}
	replacement := saved.Settings
	replacement.Search.Archives = "exclude"
	restore := pgtest.FailWrites(t, e.db, retrievalKind)
	if _, err = e.SetRetrievalSettings(replacement, saved.ID); err == nil {
		t.Fatal("settings write unexpectedly succeeded while Postgres refused it")
	}
	if e.RetrievalSettings() != saved {
		t.Fatal("failed persistence changed live defaults")
	}
	restore()
	if after, body := storedDocument(t, e, retrievalKind); after != version || body != raw {
		t.Fatal("failed update lost persisted settings")
	}
	// Another gateway's change applies here before the next scope is resolved.
	writeDocument(t, reopened, retrievalKind, `{"version":1,"search":{"version":1,"archives":"exclude","drafts":"include"},"ai":{"version":1,"archives":"exclude","drafts":"exclude"}}`)
	scope, err := e.ResolveContentScope(ContainerSelection{}, false)
	if err != nil || scope.Policy.Archives != "exclude" {
		t.Fatal("another gateway's settings were not applied", scope, err)
	}
	for _, bad := range []string{`{}`, `{"version":2}`, `{"version":1,"search":{"version":1,"archives":"exclude","drafts":"surprise"},"ai":{"version":1,"archives":"exclude","drafts":"exclude"}}`, `{"version":1,"extra":true}`} {
		writeDocument(t, e, retrievalKind, bad)
		if opened, err := Open(database(t, e.Store), e.Store); err == nil {
			opened.Close()
			t.Fatalf("malformed settings accepted: %s", bad)
		}
		if _, err := e.ResolveContentScope(ContainerSelection{}, true); err == nil {
			t.Fatalf("malformed settings resolved an AI scope: %s", bad)
		}
	}
	writeDocument(t, e, retrievalKind, raw)
}

// Retrieval settings live in the database, never as a file in the notebook's
// state directory.
func TestRetrievalDefaultsStayInTheDatabase(t *testing.T) {
	f := evidenceNotebook(t)
	e := f.engine
	before := e.RetrievalSettings()
	before.Settings.Search.Drafts = "exclude"
	updated, err := e.SetRetrievalSettings(before.Settings, before.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(filepath.Dir(f.store.Root), "retrieval.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("settings were written to the notebook's directory: %v", err)
	}
	_, raw := storedDocument(t, e, retrievalKind)
	var saved RetrievalSettings
	if err = json.Unmarshal([]byte(raw), &saved); err != nil || saved != updated.Settings {
		t.Fatal("database persistence", err)
	}
	if _, err = e.Find(context.Background(), "evidence", "", "", "", false, 100, ContainerSelection{}); err != nil {
		t.Fatal(err)
	}
}
