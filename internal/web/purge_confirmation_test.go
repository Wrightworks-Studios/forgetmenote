package web

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"forgetmenote/internal/chat"
	"forgetmenote/internal/workspace"
)

// A person who prepares a purge right after writing gets a plan made before
// the Git export caught up: it says the GitHub copy will be force-pushed,
// since the export may hold the rewritten revisions by the time the purge
// runs, and the purge runs as confirmed once it has caught up. The receipt
// says what the rebuilt export replaces.
func TestPurgePreparedBeforeTheExportCaughtUpRuns(t *testing.T) {
	app, store := fixture(t)
	secret := webPurgeSecret()
	savePurgeNote(t, store, "notes/keys.md", "# Keys\n\nThe key is "+secret+".\n", "")
	plan := preparePurgeHTTP(t, app, workspace.PurgeScope{Kind: workspace.PurgeText, Text: secret})
	if !plan.ExportForcePush {
		t.Fatalf("a plan the export has not caught up with: %+v", plan)
	}
	if _, err := store.PurgeExporter().Sync(t.Context(), "default"); err != nil {
		t.Fatal(err)
	}
	code, receipt, body := executePurgeHTTP(app, plan, "Leaked key")
	if code != http.StatusOK || !receipt.Export || !receipt.ExportForcePush {
		t.Fatalf("the confirmed purge: HTTP %d %s", code, body)
	}
}

// The receipt of a purge whose export held none of the rewritten revisions
// says that no GitHub copy is force-pushed, though the plan warned it may be.
func TestPurgeReceiptSaysWhenNoGitHubCopyIsForcePushed(t *testing.T) {
	app, store := fixture(t)
	secret := webPurgeSecret()
	savePurgeNote(t, store, "notes/keys.md", "# Keys\n\nThe key is "+secret+".\n", "")
	plan := preparePurgeHTTP(t, app, workspace.PurgeScope{Kind: workspace.PurgeText, Text: secret})
	code, receipt, body := executePurgeHTTP(app, plan, "Leaked key")
	if code != http.StatusOK || !plan.ExportForcePush || !receipt.Export || receipt.ExportForcePush {
		t.Fatalf("the purge: HTTP %d %s", code, body)
	}
}

// A browser that opens the note for live editing after the plan was made,
// as one that has just navigated to it may, leaves the confirmation valid:
// the purge restarts that live document too, and the receipt counts it.
func TestPurgePreparedBeforeALiveSessionOpenedRuns(t *testing.T) {
	app, store := fixture(t)
	secret := webPurgeSecret()
	first := savePurgeNote(t, store, "notes/draft.md", "# Draft\n\nCard "+secret+".\n", "")
	savePurgeNote(t, store, "notes/draft.md", "# Draft\n\nNo card.\n", first.Revision)
	plan := preparePurgeHTTP(t, app, workspace.PurgeScope{Kind: workspace.PurgeVersions, Path: "notes/draft.md", Versions: []string{first.Revision}})
	if w := call(app, "GET", "/api/live?path=notes/draft.md", "", testToken); w.Code != http.StatusOK {
		t.Fatalf("live: HTTP %d %s", w.Code, w.Body)
	}
	code, receipt, body := executePurgeHTTP(app, plan, "Customer data in an early draft")
	if code != http.StatusOK || receipt.LiveReset != 1 || receipt.Versions != 1 {
		t.Fatalf("the confirmed purge: HTTP %d %s", code, body)
	}
}

// A plan that finds the text only in records outside the notebook, such as
// a chat transcript, says the notebook is unchanged and so leaves out that
// its indexes are rebuilt. When someone types the text into a note before
// the Admin confirms, the purge would now reset that note's live document
// and rebuild the notebook's indexes, so the plan must be reviewed again.
func TestPurgeOfTextOnlyRecordsQuotedIsReviewedAgainOnceANoteHoldsIt(t *testing.T) {
	app, store := fixture(t)
	ctx := t.Context()
	savePurgeNote(t, store, "notes/a.md", "# A\n", "")
	secret := webPurgeSecret()
	conversation, err := app.chat.Store.Create("reader", "default", chat.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.chat.Store.Update("reader", "default", conversation.ID, func(c *chat.Conversation) error { c.Title = "Pasted " + secret; return nil }); err != nil {
		t.Fatal(err)
	}
	plan := preparePurgeHTTP(t, app, workspace.PurgeScope{Kind: workspace.PurgeText, Text: secret})
	if !plan.NotebookUnchanged || plan.Derived.Chats != 1 {
		t.Fatalf("the admin's records-only plan: %+v", plan)
	}
	live, err := store.LiveRead(ctx, "notes/a.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LivePatch(ctx, workspace.TargetedPatch{Path: "notes/a.md", Base: live.LiveRevision, OperationID: "typed", Edits: []workspace.TextEdit{{Start: 0, End: 0, Text: "typed " + secret + "\n"}}}); err != nil {
		t.Fatal(err)
	}
	code, receipt, body := executePurgeHTTP(app, plan, "Pasted key")
	if code != http.StatusConflict || receipt.Purge != "" {
		t.Fatalf("a records-only plan once a note holds the text: HTTP %d %s", code, body)
	}
	if again := preparePurgeHTTP(t, app, plan.Scope); again.NotebookUnchanged || again.Revision != plan.Revision {
		t.Fatalf("the plan reviewed again: %+v", again)
	}
}

// Opening a note outside a text purge's scope that already holds the text,
// as a person checking the plan's list of other occurrences may, leaves the
// confirmation valid: the plan already names that note.
func TestPurgeConfirmationSurvivesOpeningANoteOutsideItsScopeThatHoldsTheText(t *testing.T) {
	app, store := fixture(t)
	secret := webPurgeSecret()
	savePurgeNote(t, store, "notes/a.md", "# A\n\nKey "+secret+".\n", "")
	savePurgeNote(t, store, "notes/b.md", "# B\n\nAlso "+secret+".\n", "")
	plan := preparePurgeHTTP(t, app, workspace.PurgeScope{Kind: workspace.PurgeText, Text: secret, Paths: []string{"notes/a.md"}})
	if len(plan.Elsewhere) != 1 || plan.Elsewhere[0] != (workspace.PurgeOccurrence{Path: "notes/b.md", Kind: "file", Versions: 1}) {
		t.Fatalf("plan %+v", plan)
	}
	if w := call(app, "GET", "/api/live?path=notes/b.md", "", testToken); w.Code != http.StatusOK {
		t.Fatalf("live: HTTP %d %s", w.Code, w.Body)
	}
	code, receipt, body := executePurgeHTTP(app, plan, "Leaked key")
	if code != http.StatusOK || receipt.Rewritten == 0 || receipt.LiveReset != 0 {
		t.Fatalf("the confirmed purge: HTTP %d %s", code, body)
	}
}

// A note someone types into after the plan was made, even one that never
// held the text, would lose those edits when the purge restarts its live
// document, so the plan must be reviewed again; the edits are kept, and the
// plan made again counts the note as losing edits not yet saved.
func TestPurgeIsReviewedAgainOnceSomeoneTypesIntoANoteItWouldRestart(t *testing.T) {
	app, store := fixture(t)
	ctx := t.Context()
	secret := webPurgeSecret()
	savePurgeNote(t, store, "notes/keys.md", "# Keys\n\nThe key is "+secret+".\n", "")
	savePurgeNote(t, store, "notes/draft.md", "# Draft\n", "")
	plan := preparePurgeHTTP(t, app, workspace.PurgeScope{Kind: workspace.PurgeText, Text: secret})
	if plan.From == "" || len(plan.Live) != 0 {
		t.Fatalf("plan %+v", plan)
	}
	live, err := store.LiveRead(ctx, "notes/draft.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LivePatch(ctx, workspace.TargetedPatch{Path: "notes/draft.md", Base: live.LiveRevision, OperationID: "typed", Edits: []workspace.TextEdit{{Start: len("# Draft\n"), End: len("# Draft\n"), Text: "My unsaved paragraph.\n"}}}); err != nil {
		t.Fatal(err)
	}
	code, receipt, body := executePurgeHTTP(app, plan, "Leaked key")
	if code != http.StatusConflict || receipt.Purge != "" {
		t.Fatalf("a purge confirmed before the draft was typed into: HTTP %d %s", code, body)
	}
	if live, err = store.LiveRead(ctx, "notes/draft.md"); err != nil || live.Content != "# Draft\nMy unsaved paragraph.\n" {
		t.Fatalf("the unsaved paragraph: %+v %v", live, err)
	}
	if again := preparePurgeHTTP(t, app, plan.Scope); !slices.Contains(again.Live, workspace.PurgeLive{Path: "notes/draft.md", Pending: true}) {
		t.Fatalf("the plan made again: %+v", again)
	}
}

// A purge that changes no revision, because only a note's live edit history
// holds the text (it was pasted and removed before the note was saved),
// restarts that note's live document. When someone types into the note
// before the person confirms, the purge would discard those edits, so the
// plan must be reviewed again; the edits are kept, and the plan made again
// says the note loses edits not yet saved.
func TestLiveOnlyPurgeIsReviewedAgainOnceSomeoneTypesIntoTheNoteItRestarts(t *testing.T) {
	app, store := fixture(t)
	ctx := t.Context()
	secret := webPurgeSecret()
	head := "# Draft\n"
	savePurgeNote(t, store, "notes/draft.md", head, "")
	live, err := store.LiveRead(ctx, "notes/draft.md")
	if err != nil {
		t.Fatal(err)
	}
	pasted := "The key is " + secret + ".\n"
	if live, err = store.LivePatch(ctx, workspace.TargetedPatch{Path: "notes/draft.md", Base: live.LiveRevision, OperationID: "pasted", Edits: []workspace.TextEdit{{Start: len(head), End: len(head), Text: pasted}}}); err != nil {
		t.Fatal(err)
	}
	if live, err = store.LivePatch(ctx, workspace.TargetedPatch{Path: "notes/draft.md", Base: live.LiveRevision, OperationID: "removed", Edits: []workspace.TextEdit{{Start: len(head), End: len(head) + len(pasted), Old: pasted, Text: "No key here.\n"}}}); err != nil {
		t.Fatal(err)
	}
	if live, err = store.LiveCheckpoint(ctx, "notes/draft.md"); err != nil || live.Pending {
		t.Fatalf("saving the draft: %+v %v", live, err)
	}
	plan := preparePurgeHTTP(t, app, workspace.PurgeScope{Kind: workspace.PurgeText, Text: secret})
	if plan.From != "" || plan.NotebookUnchanged || !slices.Equal(plan.Live, []workspace.PurgeLive{{Path: "notes/draft.md"}}) {
		t.Fatalf("a plan only the draft's live history holds: %+v", plan)
	}
	if _, err = store.LivePatch(ctx, workspace.TargetedPatch{Path: "notes/draft.md", Base: live.LiveRevision, OperationID: "typed", Edits: []workspace.TextEdit{{Start: len(head), End: len(head), Text: "My unsaved paragraph.\n"}}}); err != nil {
		t.Fatal(err)
	}
	code, receipt, body := executePurgeHTTP(app, plan, "Pasted key")
	if code != http.StatusConflict || receipt.Purge != "" || !strings.Contains(body, "prepare the purge again") {
		t.Fatalf("a purge confirmed before the draft was typed into: HTTP %d %s", code, body)
	}
	if live, err = store.LiveRead(ctx, "notes/draft.md"); err != nil || live.Content != head+"My unsaved paragraph.\nNo key here.\n" || !live.Pending {
		t.Fatalf("the unsaved paragraph: %+v %v", live, err)
	}
	if again := preparePurgeHTTP(t, app, plan.Scope); again.From != "" || !slices.Equal(again.Live, []workspace.PurgeLive{{Path: "notes/draft.md", Pending: true}}) {
		t.Fatalf("the plan made again: %+v", again)
	}
}
