package workspace

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"forgetmenote/internal/gitexport"
	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
)

// purgeExporter is an exporter of s's notebooks, which exports only when
// asked (Sync), as before Start.
func purgeExporter(t *testing.T, s *Store) *gitexport.Exporter {
	t.Helper()
	ex, err := gitexport.New(s.noteStore, t.TempDir(), gitexport.Options{Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ex.Close() })
	return ex
}

// The Git export catches up with the notebook in the background, so a plan
// made before it exported the revisions the purge rewrites still says the
// export will be force-pushed: by the time the purge runs, it may hold them.
// The purge then runs as confirmed, and its result says that the export it
// rebuilt replaces them.
func TestPurgeConfirmedBeforeTheExportCaughtUpRuns(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	secret := purgeSecret(t)
	save(t, s, "b.md", []byte("Kept.\n"), "")
	ex := purgeExporter(t, s)
	if _, err := ex.Sync(ctx, s.notebook); err != nil {
		t.Fatal(err)
	}
	save(t, s, "a.md", []byte("Key "+secret+".\n"), "")
	plan, err := s.PreparePurge(ctx, PurgeScope{Kind: PurgeText, Text: secret}, ex)
	if err != nil || !plan.ExportForcePush {
		t.Fatal("a plan the export has not caught up with:", plan.ExportForcePush, err)
	}
	exported, err := ex.Sync(ctx, s.notebook)
	if err != nil || exported.Commit.String() != plan.Revision {
		t.Fatal("the export catching up:", exported, err)
	}
	result, err := s.ExecutePurge(ctx, plan, plan.Confirmation, "owner", "leak", ex)
	if err != nil || !result.Export || !result.ExportForcePush {
		t.Fatalf("the confirmed purge: %+v %v", result, err)
	}
	flag, ok, err := gitexport.ReadPurge(ex.Path(s.notebook))
	if err != nil || !ok || flag.Purge != result.Purge || len(flag.Replaces) == 0 {
		t.Fatalf("flag %+v %v %v", flag, ok, err)
	}
}

// When the export still holds none of the revisions a purge rewrote when the
// purge rebuilds it, the result says that no remote branch is replaced,
// whatever the plan warned.
func TestPurgeResultSaysWhenTheExportHeldNoRewrittenRevision(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	secret := purgeSecret(t)
	save(t, s, "b.md", []byte("Kept.\n"), "")
	ex := purgeExporter(t, s)
	if _, err := ex.Sync(ctx, s.notebook); err != nil {
		t.Fatal(err)
	}
	save(t, s, "a.md", []byte("Key "+secret+".\n"), "")
	plan, err := s.PreparePurge(ctx, PurgeScope{Kind: PurgeText, Text: secret}, ex)
	if err != nil || !plan.ExportForcePush {
		t.Fatal(plan.ExportForcePush, err)
	}
	result, err := s.ExecutePurge(ctx, plan, plan.Confirmation, "owner", "leak", ex)
	if err != nil || !result.Export || result.ExportForcePush {
		t.Fatalf("the purge: %+v %v", result, err)
	}
	flag, ok, err := gitexport.ReadPurge(ex.Path(s.notebook))
	if err != nil || !ok || flag.Purge != result.Purge || len(flag.Replaces) != 0 {
		t.Fatalf("flag %+v %v %v", flag, ok, err)
	}
}

// Live documents start when someone opens a note and change while people
// type, so a plan's confirmation does not bind them: a note opened for live
// editing between the plan and the purge leaves the confirmation valid, the
// purge restarts that live document too, so no stored copy of the text
// remains, and its result counts it.
func TestPurgeConfirmedBeforeALiveSessionOpenedRuns(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	secret := purgeSecret(t)
	save(t, s, "a.md", []byte("Key "+secret+".\n"), "")
	plan, err := s.PreparePurge(ctx, PurgeScope{Kind: PurgeText, Text: secret}, nil)
	if err != nil || len(plan.Live) != 0 {
		t.Fatalf("plan %+v %v", plan, err)
	}
	if _, err := s.LiveRead(ctx, "a.md"); err != nil {
		t.Fatal(err)
	}
	if again, err := s.PreparePurge(ctx, plan.Scope, nil); err != nil || len(again.Live) != 1 || again.Confirmation != plan.Confirmation {
		t.Fatalf("the plan once the note is live: %+v %v", again, err)
	}
	result, err := s.ExecutePurge(ctx, plan, plan.Confirmation, "owner", "leak", nil)
	if err != nil || result.LiveReset != 1 {
		t.Fatalf("the confirmed purge: %+v %v", result, err)
	}
	if bytes.Contains(storedBytes(t, testEnv(t, s).db, testEnv(t, s).blobs.Path()), []byte(secret)) {
		t.Fatal("the secret is still stored")
	}
}

// What the purge leaves where it does not reach still binds the
// confirmation: content another notebook comes to store, which the purge
// keeps for it, or the text typed into a note outside the purge's scope,
// means the plan must be reviewed again.
func TestPurgeConfirmationBindsWhatThePurgeLeaves(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	secret := purgeSecret(t)
	content := []byte("Key " + secret + ".\n")
	save(t, s, "a.md", content, "")
	save(t, s, "b.md", []byte("Clean.\n"), "")
	scope := PurgeScope{Kind: PurgeText, Text: secret, Paths: []string{"a.md"}}
	plan, err := s.PreparePurge(ctx, scope, nil)
	if err != nil {
		t.Fatal(err)
	}
	f, err := s.notes.Stat(ctx, gitobj.ID{}, "b.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.notes.LiveAppend(ctx, f.EntryID, 0, []byte("typed "+secret), "ada"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ExecutePurge(ctx, plan, plan.Confirmation, "owner", "leak", nil); !errors.Is(err, ErrPurgeConfirmation) {
		t.Fatal("the text typed outside the scope:", err)
	}
	if plan, err = s.PreparePurge(ctx, scope, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.noteStore.Create(ctx, "other", fixtureAuthor); err != nil {
		t.Fatal(err)
	}
	if _, err := s.noteStore.Notebook("other").Apply(ctx, notestore.ChangeSet{Subject: "Copy", Author: fixtureAuthor, Files: []notestore.FileChange{{Path: "copy.md", Content: content}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ExecutePurge(ctx, plan, plan.Confirmation, "owner", "leak", nil); !errors.Is(err, ErrPurgeConfirmation) {
		t.Fatal("another notebook storing the content:", err)
	}
}

// A purge that rewrites no revision changes the notebook only through the
// live documents its plan lists, so its confirmation binds them: a plan that
// found nothing in the notebook holding the text must be reviewed again once
// someone types the text into a note in its scope, as the purge would then
// reset that note's live document, which the plan never said.
func TestPurgeConfirmationBindsTheLiveDocumentsOfAPurgeThatRewritesNoRevision(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	secret := purgeSecret(t)
	save(t, s, "a.md", []byte("Clean.\n"), "")
	plan, err := s.PreparePurge(ctx, PurgeScope{Kind: PurgeText, Text: secret}, nil)
	if err != nil || plan.From != "" || len(plan.Live) != 0 {
		t.Fatalf("a plan nothing in the notebook holds: %+v %v", plan, err)
	}
	f, err := s.notes.Stat(ctx, gitobj.ID{}, "a.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.notes.LiveAppend(ctx, f.EntryID, 0, []byte("typed "+secret), "ada"); err != nil {
		t.Fatal(err)
	}
	if again, err := s.PreparePurge(ctx, plan.Scope, nil); err != nil || again.Revision != plan.Revision || len(again.Live) != 1 {
		t.Fatalf("the plan once the text is typed: %+v %v", again, err)
	}
	if result, err := s.ExecutePurge(ctx, plan, plan.Confirmation, "owner", "leak", nil); !errors.Is(err, ErrPurgeConfirmation) {
		t.Fatalf("the text typed into a note in scope: %+v %v", result, err)
	}
}

// Opening a note outside a text purge's scope whose saved text already holds
// the text, as someone checking the plan's other occurrences may, tells
// nothing the plan does not: the note is listed once, by its versions, and
// the confirmation stays valid.
func TestPurgeConfirmationSurvivesOpeningANoteOutsideItsScopeThatHoldsTheText(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	secret := purgeSecret(t)
	save(t, s, "a.md", []byte("Key "+secret+".\n"), "")
	save(t, s, "b.md", []byte("Also "+secret+".\n"), "")
	scope := PurgeScope{Kind: PurgeText, Text: secret, Paths: []string{"a.md"}}
	plan, err := s.PreparePurge(ctx, scope, nil)
	want := []PurgeOccurrence{{Path: "b.md", Kind: "file", Versions: 1}}
	if err != nil || !slices.Equal(plan.Elsewhere, want) {
		t.Fatalf("plan %+v %v", plan, err)
	}
	if _, err := s.LiveRead(ctx, "b.md"); err != nil {
		t.Fatal(err)
	}
	if again, err := s.PreparePurge(ctx, scope, nil); err != nil || !slices.Equal(again.Elsewhere, want) || again.Confirmation != plan.Confirmation {
		t.Fatalf("the plan once b.md is open: %+v %v", again, err)
	}
	if result, err := s.ExecutePurge(ctx, plan, plan.Confirmation, "owner", "leak", nil); err != nil || result.Rewritten == 0 {
		t.Fatalf("the confirmed purge: %+v %v", result, err)
	}
}

// typeLive types text at the start of path's live document, as many times as
// given, each as its own edit, and returns the document.
func typeLive(t *testing.T, s *Store, path, text string, times int) LiveDocument {
	t.Helper()
	live, err := s.LiveRead(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	for i := range times {
		if live, err = s.LivePatch(t.Context(), TargetedPatch{Path: path, Base: live.LiveRevision, OperationID: fmt.Sprintf("typed-%x-%d", text, i), Edits: []TextEdit{{Start: 0, End: 0, Text: text}}}); err != nil {
			t.Fatal(err)
		}
	}
	return live
}

// A purge of the history restarts the live documents of the notes it
// covers, and a live document's unsaved edits are lost when it restarts.
// So when someone types into a note before the purge runs, even one that
// never held the text, the plan must be reviewed again, and the plan made
// again names that note as holding unsaved edits; the edits are kept.
func TestPurgeConfirmationBindsTheUnsavedLiveEditsItWouldDiscard(t *testing.T) {
	for _, tc := range []struct {
		name  string
		times int
	}{
		{"one edit", 1},
		// So many edits that the live document stores them all in its
		// snapshot.
		{"edits stored in a snapshot", dbLiveLogEntries},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			s := newTestStore(t)
			secret := purgeSecret(t)
			save(t, s, "a.md", []byte("Key "+secret+".\n"), "")
			save(t, s, "c.md", []byte("Unrelated.\n"), "")
			plan, err := s.PreparePurge(ctx, PurgeScope{Kind: PurgeText, Text: secret}, nil)
			if err != nil || plan.From == "" || len(plan.Live) != 0 {
				t.Fatalf("plan %+v %v", plan, err)
			}
			typeLive(t, s, "c.md", "Unsaved. ", tc.times)
			if st := liveStateOf(t, s, "c.md"); tc.times == dbLiveLogEntries && (len(st.Updates) != 0 || !st.Base.IsZero()) {
				t.Fatalf("the unsaved edits are not all in the snapshot: %d updates, base %s", len(st.Updates), st.Base)
			}
			if result, err := s.ExecutePurge(ctx, plan, plan.Confirmation, "owner", "leak", nil); !errors.Is(err, ErrPurgeConfirmation) {
				t.Fatalf("a purge confirmed before c.md was typed into: %+v %v", result, err)
			}
			live, err := s.LiveRead(ctx, "c.md")
			if err != nil || live.Content != strings.Repeat("Unsaved. ", tc.times)+"Unrelated.\n" || !live.Pending {
				t.Fatalf("c.md's unsaved edits: %+v %v", live, err)
			}
			again, err := s.PreparePurge(ctx, plan.Scope, nil)
			if err != nil || !slices.Contains(again.Live, PurgeLive{Path: "c.md", Pending: true}) {
				t.Fatalf("the plan made again: %+v %v", again, err)
			}
		})
	}
}

// A note opened for live editing whose unsaved edits the plan named keeps
// the confirmation valid while its edits are stored in a snapshot.
func TestPurgeConfirmationSurvivesStoringUnsavedLiveEditsInASnapshot(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	secret := purgeSecret(t)
	save(t, s, "a.md", []byte("Key "+secret+".\n"), "")
	save(t, s, "c.md", []byte("Unrelated.\n"), "")
	typeLive(t, s, "c.md", "Unsaved. ", 1)
	plan, err := s.PreparePurge(ctx, PurgeScope{Kind: PurgeText, Text: secret}, nil)
	if err != nil || !slices.Contains(plan.Live, PurgeLive{Path: "c.md", Pending: true}) {
		t.Fatalf("plan %+v %v", plan, err)
	}
	typeLive(t, s, "c.md", "More. ", dbLiveLogEntries-1)
	if st := liveStateOf(t, s, "c.md"); len(st.Updates) != 0 || !st.Base.IsZero() {
		t.Fatalf("the unsaved edits are not all in the snapshot: %d updates, base %s", len(st.Updates), st.Base)
	}
	if result, err := s.ExecutePurge(ctx, plan, plan.Confirmation, "owner", "leak", nil); err != nil || result.LiveReset == 0 {
		t.Fatalf("the confirmed purge: %+v %v", result, err)
	}
}

// typeAndDeleteLive types text into path's live document, deletes it again
// and saves the note, as someone who pastes a secret by mistake and removes
// it before the note is saved: no revision holds text, the live document's
// history does, and the document holds no edits not saved.
func typeAndDeleteLive(t *testing.T, s *Store, path, text string) {
	t.Helper()
	live := typeLive(t, s, path, text, 1)
	live, err := s.LivePatch(t.Context(), TargetedPatch{Path: path, Base: live.LiveRevision, OperationID: fmt.Sprintf("deleted-%x", text), Edits: []TextEdit{{Start: 0, End: len(text), Old: text, Text: "Edited. "}}})
	if err != nil {
		t.Fatal(err)
	}
	if live, err = s.LiveCheckpoint(t.Context(), path); err != nil || live.Pending {
		t.Fatalf("saving %s: %+v %v", path, live, err)
	}
	if st := liveStateOf(t, s, path); !bytes.Contains(st.Snapshot, []byte(text)) || len(st.Updates) != 0 || st.Base.IsZero() {
		t.Fatalf("%s's live document once saved: %d updates, base %s, text in its snapshot %v", path, len(st.Updates), st.Base, bytes.Contains(st.Snapshot, []byte(text)))
	}
}

// A purge that rewrites no revision, because only a note's live edits hold
// the text, restarts that note's live document, which loses the edits it
// holds that are not saved as a revision. So when someone types into the
// note before the purge runs, the plan must be reviewed again, and the plan
// made again names the note as holding unsaved edits; the edits are kept.
func TestPurgeConfirmationBindsTheUnsavedLiveEditsOfAPurgeThatRewritesNoRevision(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	secret := purgeSecret(t)
	save(t, s, "a.md", []byte("Clean.\n"), "")
	typeAndDeleteLive(t, s, "a.md", "Key "+secret+". ")
	plan, err := s.PreparePurge(ctx, PurgeScope{Kind: PurgeText, Text: secret}, nil)
	if err != nil || plan.From != "" || !slices.Equal(plan.Live, []PurgeLive{{Path: "a.md"}}) {
		t.Fatalf("a plan only a.md's live history holds: %+v %v", plan, err)
	}
	typeLive(t, s, "a.md", "Unsaved. ", 1)
	if result, err := s.ExecutePurge(ctx, plan, plan.Confirmation, "owner", "leak", nil); !errors.Is(err, ErrPurgeConfirmation) {
		t.Fatalf("a purge confirmed before a.md was typed into: %+v %v", result, err)
	}
	live, err := s.LiveRead(ctx, "a.md")
	if err != nil || live.Content != "Unsaved. Edited. Clean.\n" || !live.Pending {
		t.Fatalf("a.md's unsaved edits: %+v %v", live, err)
	}
	again, err := s.PreparePurge(ctx, plan.Scope, nil)
	if err != nil || again.From != "" || !slices.Equal(again.Live, []PurgeLive{{Path: "a.md", Pending: true}}) {
		t.Fatalf("the plan made again: %+v %v", again, err)
	}
}
