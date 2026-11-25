package reposource

import (
	"context"
	"errors"
	"forgetmenote/internal/pg/pgtest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"forgetmenote/internal/workspace"
)

func TestHandoffFenceIsNotClaimedWhileStateCannotBeWritten(t *testing.T) {
	f := searchFixture(t)
	s, err := f.m.Status("alpha")
	if err != nil {
		t.Fatal(err)
	}
	restore := pgtest.FailWrites(t, f.m.db, stateKind)
	id := "0123456789abcdef0123456789abcdef"
	if _, err = f.m.BeginAuthority("alpha", "owner", id, s.ID, s.Generation, s.Downloaded); err == nil {
		t.Fatal("a fence was claimed without being stored")
	}
	current, err := f.m.Status("alpha")
	if err != nil || current.Handoff != "" || current.Paused != s.Paused || current.Generation != s.Generation {
		t.Fatalf("an unstored fence stayed visible: %+v %v", current, err)
	}
	restore()
	if _, err = f.m.BeginAuthority("alpha", "owner", id, s.ID, s.Generation, s.Downloaded); err != nil {
		t.Fatal(err)
	}
	if err = f.m.FinishAuthority("alpha", id, "github", false); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(f.m.db, f.m.root, f.m.blobs, f.m.github, f.m.check)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	current, err = reopened.Status("alpha")
	if err != nil || current.Handoff != "" || current.Paused {
		t.Fatalf("aborted fence did not persist: %+v %v", current, err)
	}
}

// Another gateway's fence and its release reach this one's next read, and a
// write over state it has not seen is refused rather than lost.
func TestGatewaysShareSourceStateAndFences(t *testing.T) {
	f := searchFixture(t)
	s, err := f.m.Status("alpha")
	if err != nil {
		t.Fatal(err)
	}
	other, err := Open(f.m.db, f.m.root, f.m.blobs, f.m.github, f.m.check)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	id := "0123456789abcdef0123456789abcdef"
	if _, err = other.BeginAuthority("alpha", "owner", id, s.ID, s.Generation, s.Downloaded); err != nil {
		t.Fatal(err)
	}
	if current, err := f.m.Status("alpha"); err != nil || current.Handoff != id || !current.Paused {
		t.Fatalf("another gateway's fence was not seen: %+v %v", current, err)
	}
	if _, err = f.m.Control("alpha", "owner", "resume"); !errors.Is(err, ErrConflict) {
		t.Fatalf("a fenced source was resumed through another gateway: %v", err)
	}
	if err = f.m.FinishAuthority("alpha", id, "github", false); err != nil {
		t.Fatal(err)
	}
	if current, err := other.Status("alpha"); err != nil || current.Handoff != "" || current.LastHandoff != id {
		t.Fatalf("the release did not reach the fencing gateway: %+v %v", current, err)
	}
	n, err := f.m.book("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if err := n.lock(); err != nil {
		t.Fatal(err)
	}
	stale := n.state.Sources[n.state.Active]
	n.mu.Unlock()
	if _, err = other.Control("alpha", "owner", "pause"); err != nil {
		t.Fatal(err)
	}
	n.mu.Lock()
	err = n.save(stale)
	n.mu.Unlock()
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("a write over unseen state was not refused: %v", err)
	}
}

func TestNotebookLayoutSurvivesVerifiedReconnection(t *testing.T) {
	f := searchFixture(t)
	f.addFiles(t, "alpha", map[string]string{
		"draft.md":  "---\nfmn-container: draft\n---\n# Draft\nreconnectdraftterm",
		"AGENTS.md": "reconnectpolicyterm",
	})
	s, err := f.m.Status("alpha")
	if err != nil {
		t.Fatal(err)
	}
	id := "0123456789abcdef0123456789abcdef"
	if _, err = f.m.BeginAuthority("alpha", "owner", id, s.ID, s.Generation, s.Downloaded); err != nil {
		t.Fatal(err)
	}
	if err = f.m.FinishAuthority("alpha", id, "github", true); err != nil {
		t.Fatal(err)
	}
	if _, err = f.m.Control("alpha", "owner", "disconnect"); err != nil {
		t.Fatal(err)
	}
	reconnected, err := f.m.Connect(t.Context(), "alpha", "owner", "grant", 7, 42)
	if err != nil {
		t.Fatal(err)
	}
	if reconnected.ID == s.ID || !reconnected.NotebookLayout || reconnected.HistoricalOnly || reconnected.Handoff != "" {
		t.Fatalf("new verified connection lost notebook layout or retained transfer state: %+v", reconnected)
	}
	if _, err = f.m.Refresh(t.Context(), "alpha"); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"reconnectdraftterm", "reconnectpolicyterm"} {
		found, err := f.m.Search(t.Context(), SearchQuery{Projects: []string{"alpha"}, Query: query, Mode: "text", Check: allowSource})
		if err != nil {
			t.Fatal(err)
		}
		for _, hit := range found.Results {
			if strings.Contains(hit.Text, query) {
				t.Fatalf("reconnection exposed excluded notebook content: %s", query)
			}
		}
	}
	if _, err = f.m.Control("alpha", "owner", "disconnect"); err != nil {
		t.Fatal(err)
	}
	different, err := f.m.Connect(t.Context(), "alpha", "owner", "grant", 7, 43)
	if err != nil || different.NotebookLayout {
		t.Fatalf("another repository inherited the layout: %+v %v", different, err)
	}
}

func TestHandoffEnumeratesUnindexedFilesAndVerifiesDefaultBranch(t *testing.T) {
	f := searchFixture(t)
	state, err := f.m.Status("alpha")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := f.m.NotebookSnapshot(t.Context(), "alpha", state.ID, state.Downloaded, "main")
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Files["health.go"]) == 0 {
		t.Fatal("snapshot dropped code without reporting incompatibility")
	}
	if err = workspace.ValidateNotebookSnapshot(t.Context(), snapshot); err == nil {
		t.Fatal("code repository accepted as writable Markdown notebook")
	}
	if _, err = f.m.NotebookSnapshot(t.Context(), "alpha", state.ID, state.Downloaded, "other"); err == nil {
		t.Fatal("default branch change ignored")
	}
}

func TestNotebookLayoutSeparatesContainersAndRetainedEvidence(t *testing.T) {
	f := searchFixture(t)
	ctx := context.Background()
	gitRun(t, f.dirs["alpha"], "rm", "health.go")
	f.addFiles(t, "alpha", map[string]string{"draft.md": "---\nfmn-container: draft\n---\n# Draft\nuniquedraftterm", "archive.md": "---\nfmn-container: archive\n---\n# Old\narchiveonlyterm", "comments/" + strings.Repeat("a", 64) + ".json": `[{"text":"discussiononlyterm"}]`, "AGENTS.md": "instructiononlyterm"})
	s, _ := f.m.Status("alpha")
	snapshot, err := f.m.NotebookSnapshot(ctx, "alpha", s.ID, s.Downloaded, "main")
	if err != nil {
		t.Fatal(err)
	}
	id := "0123456789abcdef0123456789abcdef"
	if _, err = f.m.BeginAuthority("alpha", "owner", id, s.ID, s.Generation, s.Downloaded); err != nil {
		t.Fatal(err)
	}
	if err = f.m.PinNotebook(ctx, "alpha", id, snapshot); err != nil {
		t.Fatal(err)
	}
	if err = f.m.FinishAuthority("alpha", id, "github", true); err != nil {
		t.Fatal(err)
	}
	if err = f.m.FinishAuthority("alpha", id, "github", true); err != nil {
		t.Fatal("completion replay", err)
	}
	if err = f.m.FinishAuthority("alpha", id, "forgetmenote", true); err == nil {
		t.Fatal("completion replay changed its target")
	}
	if _, err = f.m.Refresh(ctx, "alpha"); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"uniquedraftterm", "discussiononlyterm", "instructiononlyterm"} {
		found, err := f.m.Search(ctx, SearchQuery{Projects: []string{"alpha"}, Query: query, Mode: "text", Check: allowSource})
		if err != nil {
			t.Fatal(err)
		}
		for _, hit := range found.Results {
			if strings.Contains(hit.Text, query) {
				t.Fatalf("default retrieval exposed %s", query)
			}
		}
	}
	// Historical reads remain independent of the replaceable Git cache.
	if err = os.RemoveAll(filepath.Join(f.m.root, "alpha", s.ID, "source.git")); err != nil {
		t.Fatal(err)
	}
	file, err := f.m.File(ctx, "alpha", s.ID, s.Downloaded, "draft.md")
	if err != nil || !strings.Contains(file.Content, "uniquedraftterm") {
		t.Fatal("pinned original lost", err)
	}
	f.denied.Store(true)
	if _, err = f.m.File(ctx, "alpha", s.ID, s.Downloaded, "draft.md"); err == nil {
		t.Fatal("retained evidence bypassed authorization")
	}
}
