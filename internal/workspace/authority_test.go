package workspace

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
	"forgetmenote/internal/pg/pgtest"
)

// A handoff's preview and preparation refuse pending live edits rather than
// checkpoint them, and a refused preparation leaves no fence.
func TestAuthorityPreviewAndPreparationNeverCheckpointPendingWork(t *testing.T) {
	s, ctx, first := liveTestStore(t)
	live, err := s.LiveRead(ctx, "note.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.LivePatch(ctx, TargetedPatch{Path: "note.md", Base: live.LiveRevision, OperationID: "handoff-pending", Edits: []TextEdit{{Start: 3, End: 8, Old: "alpha", Text: "pending"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.NotebookSnapshot(ctx); !errors.Is(err, ErrLivePending) {
		t.Fatal("preview checkpointed pending collaboration", err)
	}
	if _, err = s.PrepareAuthority(ctx, AuthorityRequest{ID: "0123456789abcdef0123456789abcdef", Generation: 1, From: "forgetmenote", To: "github", Revision: first.Revision}); !errors.Is(err, ErrLivePending) {
		t.Fatal("preparation checkpointed pending collaboration", err)
	}
	if state, err := s.AuthorityStatus(ctx); err != nil || state.Phase != "" {
		t.Fatalf("a refused preparation fenced the notebook: %+v %v", state, err)
	}
	current, err := s.Read(ctx, "note.md", "")
	if err != nil || current.Revision != first.Revision || string(current.Content) != string(first.Content) {
		t.Fatal("handoff changed committed content", err)
	}
	currentLive, err := s.LiveRead(ctx, "note.md")
	if err != nil || !currentLive.Pending {
		t.Fatal("pending edits lost", err)
	}
}

func TestAuthorityHandoffFencesWritesAndRetainsHistory(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	doc, err := s.Save(ctx, "note.md", []byte("original"), "", "author")
	if err != nil {
		t.Fatal(err)
	}
	id := "0123456789abcdef0123456789abcdef"
	req := AuthorityRequest{ID: id, Generation: 1, From: "forgetmenote", To: "github", Revision: doc.Revision}
	if _, err = s.PrepareAuthority(ctx, req); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Save(ctx, "note.md", []byte("late save"), doc.Revision, "author"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("frozen save: %v", err)
	}
	// The fence refuses comments too.
	if _, err = s.AddComment(ctx, "note.md", "late comment", doc.Revision, "author"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("frozen comment: %v", err)
	}
	if _, err = s.FinishAuthority(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Save(ctx, "another.md", []byte("late"), "", "author"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("GitHub save: %v", err)
	}
	back := "1123456789abcdef0123456789abcdef"
	if _, err = s.PrepareAuthority(ctx, AuthorityRequest{ID: back, Generation: 2, From: "github", To: "forgetmenote", Revision: doc.Revision}); err != nil {
		t.Fatal(err)
	}
	snapshot := NotebookSnapshot{Source: "fixture", Revision: doc.Revision, Files: map[string][]byte{"note.md": []byte("upstream"), "draft.md": []byte("---\nfmn-container: draft\n---\ndraft"), "assets/photo.png": []byte{0x89, 'P', 'N', 'G', 0}}}
	if _, err = s.StageAuthority(ctx, back, snapshot); err != nil {
		t.Fatal(err)
	}
	// Preparation must not expose imported bytes. Restart with the fence intact.
	got, _ := s.Read(ctx, "note.md", "")
	if string(got.Content) != "original" {
		t.Fatal("stage changed live content")
	}
	s = reopenTestStore(t, s)
	if _, err = s.Save(ctx, "late.md", []byte("late"), "", "author"); !errors.Is(err, ErrForbidden) {
		t.Fatal("restart lost fence", err)
	}
	state, err := s.FinishAuthority(ctx, back, true)
	if err != nil {
		t.Fatal(err)
	}
	// The import is one revision on the prepared one, naming its operation
	// and request as the Git backend's commit did.
	commit, err := gitobj.ParseID(state.Committed)
	if err != nil {
		t.Fatal(err)
	}
	info, err := s.notes.Commit(ctx, commit)
	if err != nil || len(info.Commit.Parents) != 1 || info.Commit.Parents[0].String() != doc.Revision || commitSubject(info.Commit.Message) != "Import notebook from  at "+doc.Revision ||
		trailerValue(info.Trailers, "Gitnotes-Operation") != back || trailerValue(info.Trailers, "Gitnotes-Request") != state.StageHash || trailerValue(info.Trailers, "Gitnotes-Source") != "system" {
		t.Fatalf("import revision: %q %v %v", info.Commit.Message, info.Commit.Parents, err)
	}
	// Completion drops the stage with the receipt; a replay drops any stage
	// a record still keeps.
	record, err := s.notes.Authority(ctx)
	if err != nil || record.Staged {
		t.Fatal("completed handoff retained its content stage", err)
	}
	if _, err = s.notes.PutAuthority(ctx, notestore.AuthorityWrite{Version: record.Version, State: record.State, Stage: []byte("leftover stage")}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.FinishAuthority(ctx, back, true); err != nil {
		t.Fatal("handoff replay", err)
	}
	if record, err = s.notes.Authority(ctx); err != nil || record.Staged {
		t.Fatal("completed handoff retained its content stage", err)
	}
	for path, want := range snapshot.Files {
		got, err = s.Read(ctx, path, "")
		if err != nil || !bytes.Equal(got.Content, want) || got.Revision != state.Committed {
			t.Fatal("imported bytes", path, err)
		}
	}
	old, err := s.Read(ctx, "note.md", doc.Revision)
	if err != nil || string(old.Content) != "original" {
		t.Fatal("lost old history", err)
	}
	if _, err = s.Save(WithAuthority(ctx, 1), "note.md", []byte("stale session"), got.Revision, "author"); !errors.Is(err, ErrConflict) {
		t.Fatal("stale generation", err)
	}
	if _, err = s.Save(WithAuthority(ctx, 3), "note.md", []byte("central edit"), got.Revision, "author"); err != nil {
		t.Fatal(err)
	}
}

func TestAuthorityStateRejectsInvalidPreparedTargetOnRestart(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	head, err := s.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	id := "0123456789abcdef0123456789abcdef"
	if _, err = s.PrepareAuthority(ctx, AuthorityRequest{ID: id, Generation: 1, From: "forgetmenote", To: "github", Revision: head.Revision}); err != nil {
		t.Fatal(err)
	}
	record, err := s.notes.Authority(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var state AuthorityState
	json.Unmarshal(record.State, &state)
	state.To = "forgetmenote"
	raw, _ := json.Marshal(state)
	if _, err = s.notes.PutAuthority(ctx, notestore.AuthorityWrite{Version: record.Version, State: raw}); err != nil {
		t.Fatal(err)
	}
	reopened := reopenTestStore(t, s)
	if _, err = reopened.AuthorityStatus(ctx); err == nil {
		t.Fatal("invalid preparation receipt was accepted")
	}
	if _, err = reopened.Save(ctx, "late.md", []byte("late"), "", "author"); err == nil {
		t.Fatal("a write passed an invalid preparation receipt")
	}
	if _, err = reopened.FinishAuthority(ctx, id, false); err == nil {
		t.Fatal("an invalid preparation receipt was finished")
	}
}

// Every gateway serving a notebook kept in the notes store reads one
// authority record, as every request once reached the notebook's one owner.
func TestAuthorityHandoffFencesEveryGateway(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	other, err := OpenDB(ctx, openTestNotes(t, testEnv(t, s)), "notes", t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	doc, err := s.Save(ctx, "owned.md", []byte("owner bytes"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	// The other gateway has heard of the save, so only the fence stops it.
	for {
		if current, err := other.Status(ctx); err == nil && current.Revision == doc.Revision {
			break
		}
		time.Sleep(time.Millisecond)
	}
	state, err := s.PrepareAuthority(ctx, AuthorityRequest{ID: "0123456789abcdef0123456789abcdef", Generation: 1, From: "forgetmenote", To: "github", Revision: doc.Revision})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = other.Save(ctx, "owned.md", []byte("late"), doc.Revision, ""); !errors.Is(err, ErrForbidden) {
		t.Fatal("another gateway accepted a fenced write", err)
	}
	if got, err := other.AuthorityStatus(ctx); err != nil || got.Phase != "prepared" || got.Transfer != state.Transfer {
		t.Fatalf("another gateway's status: %+v %v", got, err)
	}
	if _, err = s.FinishAuthority(ctx, state.Transfer, true); err != nil {
		t.Fatal(err)
	}
	if _, err = other.Save(ctx, "owned.md", []byte("late"), doc.Revision, ""); !errors.Is(err, ErrReadOnly) {
		t.Fatal("another gateway wrote to a GitHub notebook", err)
	}
	if got, err := other.FinishAuthority(ctx, state.Transfer, true); err != nil || got.Phase != "complete" || got.Type != "github" || got.Generation != 2 || got.Committed != doc.Revision {
		t.Fatalf("another gateway's retry of the completion: %+v %v", got, err)
	}
}

// An existing GitHub notebook's kind lives in the catalog, which installs
// it; the notebook has no handoff record before its first handoff.
func TestAuthorityInitialGitHubNotebookTakesItsKindFromTheCatalog(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	head, err := s.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s.SetReadOnly(true)
	s.SetAuthorityGeneration(1)
	id := "0123456789abcdef0123456789abcdef"
	if _, err = s.PrepareAuthority(ctx, AuthorityRequest{ID: id, Generation: 1, From: "forgetmenote", To: "github", Revision: head.Revision}); !errors.Is(err, ErrConflict) {
		t.Fatal("a GitHub notebook prepared as a ForgetMeNote one", err)
	}
	if _, err = s.PrepareAuthority(ctx, AuthorityRequest{ID: id, Generation: 1, From: "github", To: "forgetmenote", Revision: head.Revision}); err != nil {
		t.Fatal("initial GitHub authority", err)
	}
	if _, err = s.StageAuthority(ctx, id, NotebookSnapshot{Revision: head.Revision, Files: map[string][]byte{"note.md": []byte("upstream")}}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.FinishAuthority(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Save(ctx, "note.md", []byte("central"), "", "fixture"); !errors.Is(err, ErrConflict) {
		t.Fatal("expected normal revision guard after import", err)
	}
	doc, err := s.Read(ctx, "note.md", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Save(ctx, "note.md", []byte("central"), doc.Revision, "fixture"); err != nil {
		t.Fatal(err)
	}
}

// Without a handoff record, as for a notebook brought over from a working
// tree, the notebook is at the generation the catalog installed.
func TestAuthorityWithoutARecordFollowsTheInstalledGeneration(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	head, err := s.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s.SetAuthorityGeneration(3)
	if state, err := s.AuthorityStatus(ctx); err != nil || state != (AuthorityState{Version: 1, Generation: 3, Type: "forgetmenote"}) {
		t.Fatalf("status: %+v %v", state, err)
	}
	id := "0123456789abcdef0123456789abcdef"
	if _, err = s.PrepareAuthority(ctx, AuthorityRequest{ID: id, Generation: 1, From: "forgetmenote", To: "github", Revision: head.Revision}); !errors.Is(err, ErrConflict) {
		t.Fatal("a preparation for an older generation", err)
	}
	if _, err = s.PrepareAuthority(ctx, AuthorityRequest{ID: id, Generation: 3, From: "forgetmenote", To: "github", Revision: head.Revision}); err != nil {
		t.Fatal(err)
	}
	if state, err := s.FinishAuthority(ctx, id, true); err != nil || state.Generation != 4 || state.Type != "github" {
		t.Fatalf("completion: %+v %v", state, err)
	}
}

// A completion that committed its import but not its receipt finds the
// import by its operation when retried, by this gateway or a restarted one.
func TestAuthorityImportCommittedBeforeItsReceiptCompletesOnRetry(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	s.SetReadOnly(true)
	head, err := s.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	id := "0123456789abcdef0123456789abcdef"
	if _, err = s.PrepareAuthority(ctx, AuthorityRequest{ID: id, Generation: 1, From: "github", To: "forgetmenote", Revision: head.Revision}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.StageAuthority(ctx, id, NotebookSnapshot{Revision: head.Revision, Repository: "octo/notes", Files: map[string][]byte{"a.md": []byte("upstream")}}); err != nil {
		t.Fatal(err)
	}
	restore := pgtest.FailTable(t, testEnv(t, s).db, "notebook_authority")
	if _, err = s.FinishAuthority(ctx, id, true); err == nil {
		t.Fatal("completion without its receipt")
	}
	restore()
	imported, err := s.Status(ctx)
	if err != nil || imported.Revision == head.Revision {
		t.Fatal("the import was not committed", err)
	}
	if state, err := s.AuthorityStatus(ctx); err != nil || state.Phase != "prepared" {
		t.Fatalf("a refused receipt was kept: %+v %v", state, err)
	}
	s = reopenTestStore(t, s)
	state, err := s.FinishAuthority(ctx, id, true)
	if err != nil || state.Phase != "complete" || state.Type != "forgetmenote" || state.Generation != 2 || state.Committed != imported.Revision {
		t.Fatalf("retried completion: %+v %v", state, err)
	}
	if doc, err := s.Read(ctx, "a.md", ""); err != nil || string(doc.Content) != "upstream" || doc.Revision != imported.Revision {
		t.Fatal("imported content", err)
	}
}

// A completion never takes a revision it did not prepare for its own:
// neither a handoff to GitHub nor an import completes over a later write.
func TestAuthorityNeverCompletesOverAnotherRevision(t *testing.T) {
	id := "0123456789abcdef0123456789abcdef"
	t.Run("github", func(t *testing.T) {
		s := newTestStore(t)
		ctx := t.Context()
		head, err := s.Status(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.PrepareAuthority(ctx, AuthorityRequest{ID: id, Generation: 1, From: "forgetmenote", To: "github", Revision: head.Revision}); err != nil {
			t.Fatal(err)
		}
		commitFixture(t, s, fixture("late.md", "a write that passed the fence"))
		if _, err = s.FinishAuthority(ctx, id, true); !errors.Is(err, ErrConflict) {
			t.Fatal("completed over a later revision", err)
		}
		if state, err := s.AuthorityStatus(ctx); err != nil || state.Phase != "prepared" || state.Type != "forgetmenote" {
			t.Fatalf("status: %+v %v", state, err)
		}
	})
	t.Run("forgetmenote", func(t *testing.T) {
		s := newTestStore(t)
		ctx := t.Context()
		s.SetReadOnly(true)
		head, err := s.Status(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.PrepareAuthority(ctx, AuthorityRequest{ID: id, Generation: 1, From: "github", To: "forgetmenote", Revision: head.Revision}); err != nil {
			t.Fatal(err)
		}
		if _, err = s.StageAuthority(ctx, id, NotebookSnapshot{Revision: head.Revision, Files: map[string][]byte{"a.md": []byte("upstream")}}); err != nil {
			t.Fatal(err)
		}
		late := commitFixture(t, s, fixture("late.md", "a write that passed the fence"))
		if _, err = s.FinishAuthority(ctx, id, true); !errors.Is(err, ErrConflict) {
			t.Fatal("took another revision for the import", err)
		}
		if current, err := s.Status(ctx); err != nil || current.Revision != late {
			t.Fatal("imported over a later revision", err)
		}
	})
}

// Cancelling an unpublished handoff releases the fence; the cancellation is
// final.
func TestAuthorityAbortReleasesTheFence(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	doc, err := s.Save(ctx, "note.md", []byte("original"), "", "author")
	if err != nil {
		t.Fatal(err)
	}
	id := "0123456789abcdef0123456789abcdef"
	if _, err = s.PrepareAuthority(ctx, AuthorityRequest{ID: id, Generation: 1, From: "forgetmenote", To: "github", Revision: doc.Revision}); err != nil {
		t.Fatal(err)
	}
	state, err := s.FinishAuthority(ctx, id, false)
	if err != nil || state.Phase != "aborted" || state.Type != "forgetmenote" || state.Generation != 1 {
		t.Fatalf("abort: %+v %v", state, err)
	}
	if _, err = s.FinishAuthority(ctx, id, false); err != nil {
		t.Fatal("abort replay", err)
	}
	if _, err = s.FinishAuthority(ctx, id, true); !errors.Is(err, ErrConflict) {
		t.Fatal("completed an aborted handoff", err)
	}
	if _, err = s.Save(ctx, "note.md", []byte("after abort"), doc.Revision, "author"); err != nil {
		t.Fatal("abort kept the fence", err)
	}
}

// Cancelling a staged handoff to ForgetMeNote drops its snapshot, which a
// retry may stage again only unchanged.
func TestAuthorityAbortDropsTheStagedSnapshot(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	s.SetReadOnly(true)
	head, err := s.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	id := "0123456789abcdef0123456789abcdef"
	if _, err = s.PrepareAuthority(ctx, AuthorityRequest{ID: id, Generation: 1, From: "github", To: "forgetmenote", Revision: head.Revision}); err != nil {
		t.Fatal(err)
	}
	snapshot := NotebookSnapshot{Revision: head.Revision, Files: map[string][]byte{"a.md": []byte("upstream")}}
	staged, err := s.StageAuthority(ctx, id, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := s.StageAuthority(ctx, id, snapshot); err != nil || again.StageHash != staged.StageHash {
		t.Fatal("stage replay", err)
	}
	snapshot.Files["a.md"] = []byte("moved upstream")
	if _, err = s.StageAuthority(ctx, id, snapshot); !errors.Is(err, ErrConflict) {
		t.Fatal("restaged other content", err)
	}
	if record, err := s.notes.Authority(ctx); err != nil || !record.Staged {
		t.Fatal("no stage", err)
	}
	state, err := s.FinishAuthority(ctx, id, false)
	if err != nil || state.Phase != "aborted" || state.StageHash != "" || state.Type != "github" {
		t.Fatalf("abort: %+v %v", state, err)
	}
	if record, err := s.notes.Authority(ctx); err != nil || record.Staged {
		t.Fatal("an aborted handoff kept its stage", err)
	}
	if current, err := s.Status(ctx); err != nil || current.Revision != head.Revision {
		t.Fatal("an aborted handoff imported content", err)
	}
}

// An import keeps the notebook's identities: a note renamed upstream keeps
// its history, and an fmn-id a kept note has is not given to another.
func TestAuthorityImportKeepsNoteIdentities(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	s.SetReadOnly(true)
	renamed, kept, changed := strings.Repeat("a", 32), strings.Repeat("b", 32), strings.Repeat("c", 32)
	note := func(id, text string) []byte { return []byte("---\nfmn-id: " + id + "\n---\n" + text) }
	head := commitFixture(t, s,
		notestore.FileChange{Path: "old.md", Content: note(renamed, "moving"), EntryID: renamed},
		notestore.FileChange{Path: "kept.md", Content: note(kept, "staying"), EntryID: kept})
	id := "0123456789abcdef0123456789abcdef"
	if _, err := s.PrepareAuthority(ctx, AuthorityRequest{ID: id, Generation: 1, From: "github", To: "forgetmenote", Revision: head}); err != nil {
		t.Fatal(err)
	}
	snapshot := NotebookSnapshot{Revision: head, Files: map[string][]byte{"new.md": note(renamed, "moving"), "kept.md": note(changed, "staying"), "other.md": note(kept, "claims the kept identity")}}
	if _, err := s.StageAuthority(ctx, id, snapshot); err != nil {
		t.Fatal(err)
	}
	state, err := s.FinishAuthority(ctx, id, true)
	if err != nil {
		t.Fatal(err)
	}
	rev, _ := gitobj.ParseID(state.Committed)
	entry := func(path string) string {
		f, err := s.notes.Stat(ctx, rev, path)
		if err != nil {
			t.Fatal(path, err)
		}
		return f.EntryID
	}
	if entry("new.md") != renamed || entry("kept.md") != kept {
		t.Fatal("import lost a note's identity")
	}
	if other := entry("other.md"); other == kept || other == "" {
		t.Fatal("import gave a kept identity to another note:", other)
	}
	info, err := s.notes.Commit(ctx, rev)
	if err != nil {
		t.Fatal(err)
	}
	renames := 0
	for _, c := range info.Changes {
		if c.Path == "new.md" && c.Op == notestore.OpRename && c.OldPath == "old.md" {
			renames++
		}
	}
	if renames != 1 {
		t.Fatalf("a renamed note's history: %+v", info.Changes)
	}
}

// A handoff keeps a source original the note budget would refuse.
func TestSourceOriginalBeyondNoteBudgetSurvivesHandoff(t *testing.T) {
	t.Setenv("FMN_LIMIT_FILE_BYTES", "1024")
	t.Setenv("FMN_LIMIT_SOURCE_BYTES", "8192")
	s := newTestStore(t)
	ctx := t.Context()
	// An admitted source's note and original, where source capture keeps them.
	original := bytes.Repeat([]byte("x"), 4096)
	path := "assets/sources/0123456789abcdef0123456789abcdef/" + ContentHash(original) + ".pdf"
	revision := commitFixture(t, s, fixture("larger-original.md", "Amber evidence.\n"), notestore.FileChange{Path: path, Content: original})
	read, err := s.Read(ctx, path, revision)
	if err != nil || !bytes.Equal(read.Content, original) {
		t.Fatal("source read used ordinary note limit", err)
	}
	snapshot, err := s.NotebookSnapshot(ctx)
	if err != nil || snapshot.Revision != revision || !bytes.Equal(snapshot.Files[path], original) {
		t.Fatal("handoff rejected an admitted source", err)
	}
	if err = ValidateNotebookSnapshot(ctx, snapshot); err != nil {
		t.Fatal("handoff validation rejected an admitted source", err)
	}
}

// A handoff carries only what an ordinary notebook holds: no executable
// files, links or submodules, within the handoff's budgets.
func TestNotebookSnapshotRefusesWhatAHandoffCannotCarry(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	commitFixture(t, s, fixture("note.md", "note"), notestore.FileChange{Path: "run.md", Content: []byte("#!/bin/sh\n"), Mode: gitobj.ModeExecutable})
	if _, err := s.NotebookSnapshot(ctx); err == nil || err.Error() != "handoff does not support executable files, symlinks or submodules" {
		t.Fatal("snapshot of an executable file:", err)
	}
	t.Setenv("FMN_LIMIT_FILE_BYTES", "1024")
	s = newTestStore(t)
	commitFixture(t, s, fixture("large.md", strings.Repeat("x", 1025)))
	if _, err := s.NotebookSnapshot(ctx); err == nil || err.Error() != "handoff exceeds the configured file/source byte budget, 64 MiB total, or 10,000 files" {
		t.Fatal("snapshot past the file budget:", err)
	}
}

// Validation parses a snapshot's lock policy and Trash index as committed
// ones are parsed, with no repository to commit them to.
func TestNotebookSnapshotValidatesPoliciesAsCommitted(t *testing.T) {
	revision := strings.Repeat("a", 40)
	trashID := "0123456789abcdef0123456789abcdef"
	trash := func(r TrashRecord) []byte {
		raw, _ := json.Marshal(TrashIndex{Version: 1, Records: []TrashRecord{r}})
		return raw
	}
	valid := TrashRecord{TrashEntry: TrashEntry{ID: trashID, Path: "Trash/" + trashID + ".md", OriginalPath: "notes/old.md"}, History: map[string][]string{"notes/old.md": {revision}}}
	invalidEntry := valid
	invalidEntry.Path = "Trash/other.md"
	for want, files := range map[string]map[string][]byte{
		"invalid project lock policy": {locksPath: []byte(`{"version":2,"paths":[]}`)},
		"duplicate project lock path": {locksPath: []byte(`{"version":1,"paths":["notes","notes"]}`)},
		ErrForbidden.Error():          {locksPath: []byte(`{"version":1,"paths":["notes/.private"]}`)},
		"lock policy exceeds 64 KiB":  {locksPath: append([]byte(`{"version":1,"paths":[]}`), bytes.Repeat([]byte(" "), 65536)...)},
		"invalid Trash index":         {trashIndexPath: []byte(`{"version":1}`)},
		"invalid Trash entry":         {trashIndexPath: trash(invalidEntry)},
		"existing Trash content is not system managed; relocate it before enabling Trash": {"Trash/old.md": []byte("old")},
		"handoff has a file and a folder at notes.md":                                     {"notes.md": []byte("a"), "notes.md/child.md": []byte("b")},
	} {
		if err := ValidateNotebookSnapshot(t.Context(), NotebookSnapshot{Revision: revision, Files: files}); err == nil || err.Error() != want {
			t.Errorf("got %v, want %q", err, want)
		}
	}
	files := map[string][]byte{
		locksPath:                  []byte(`{"version":1,"paths":["notes"]}`),
		trashIndexPath:             trash(valid),
		"Trash/" + trashID + ".md": []byte("old"),
		"notes/plan.md":            []byte("# Plan\n"),
	}
	if err := ValidateNotebookSnapshot(t.Context(), NotebookSnapshot{Revision: revision, Files: files}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateNotebookSnapshot(t.Context(), NotebookSnapshot{Revision: revision, Files: map[string][]byte{}}); err != nil {
		t.Fatal("an empty notebook:", err)
	}
}
