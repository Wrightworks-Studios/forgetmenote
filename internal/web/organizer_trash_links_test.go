package web

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"forgetmenote/internal/workspace"
)

// organizerMoveResults are the reasons and Trash links of a settled
// organization job's actions, by the path each organized.
func organizerMoveResults(t *testing.T, job map[string]any) map[string]organizationAction {
	t.Helper()
	raw, _ := json.Marshal(job["results"])
	var results []jevJobResult
	if err := json.Unmarshal(raw, &results); err != nil {
		t.Fatal(err)
	}
	actions := map[string]organizationAction{}
	for _, result := range results {
		a, err := actionFromResult(result)
		if err != nil {
			t.Fatal(err)
		}
		actions[a.Path] = a
	}
	return actions
}

// The organizer's preview names the notes in the Trash whose links a move
// will update and those it will leave, and why; applying updates them, and
// Undo run reverts a note in the Trash two moves of the run updated, the
// later move's undo named for the earlier one, saying so in each action's
// reason.
func TestOrganizerMovesUpdateLinksOfNotesInTheTrash(t *testing.T) {
	app := reviewFixture(t, organizerChoiceAnswer)
	ctx := context.Background()
	store, _ := app.catalog.Access("alice", "default", "owner")
	for path, content := range map[string]string{"dest/index.md": "# Workshops\nWorkshop planning.\n", "linker.md": "# Linker\n[A](a.md) and [B](b.md)\n", "L/locked.md": "# Locked\n[A](../a.md)\n"} {
		if _, err := store.Save(ctx, path, []byte(content), "", "alice"); err != nil {
			t.Fatal(err)
		}
	}
	status, err := store.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	trashed, err := store.TrashNotes(ctx, []string{"linker.md", "L/locked.md"}, status.Revision, "alice")
	if err != nil {
		t.Fatal(err)
	}
	entries := map[string]workspace.TrashEntry{}
	for _, entry := range trashed.Entries {
		entries[entry.OriginalPath] = entry
	}
	state, err := store.Locks(ctx, "L")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.SetLock(ctx, "L", true, state.Revision, "alice"); err != nil {
		t.Fatal(err)
	}
	linker, locked := entries["linker.md"].Path, entries["L/locked.md"].Path
	readLinker := func() string {
		t.Helper()
		doc, err := store.Read(ctx, linker, "")
		if err != nil {
			t.Fatal(err)
		}
		return string(doc.Content)
	}
	original := readLinker()
	left := locked + " (from L/locked.md): its original location L/locked.md is locked"

	preview := waitReview(t, app, organizerStart(t, app, `{"kind":"organize","operationId":"trash-preview","organization":{"mode":"preview","scope":"selected","paths":["a.md"],"policy":{"moves":true,"folders":["dest"]}}}`))
	a := organizerMoveResults(t, preview)["a.md"]
	if want := "Preview only; no notes were changed. Notes in the Trash whose links to it will be updated: " + linker + " (from linker.md). Notes in the Trash that will be left as they are: " + left + "."; a.Status != "suggested" || a.Reason != want {
		t.Errorf("the preview: %s %q, want %q", a.Status, a.Reason, want)
	}
	if a.Result == nil || len(a.Result.TrashLinks) != 2 {
		t.Errorf("the preview's result: %+v", a.Result)
	}
	if readLinker() != original {
		t.Fatal("the preview changed a note in the Trash")
	}

	id := organizerStart(t, app, `{"kind":"organize","operationId":"trash-apply","organization":{"mode":"apply","scope":"selected","paths":["a.md","b.md"],"policy":{"moves":true,"folders":["dest"]}}}`)
	if job := waitReview(t, app, id); job["state"] != "completed" {
		t.Fatal(job)
	}
	applied := organizerMoveResults(t, waitReview(t, app, id))
	if want := "Applied under the selected organization policy. Links updated in notes in the Trash: " + linker + " (from linker.md). Notes in the Trash left as they were: " + left + "."; applied["a.md"].Status != "applied" || applied["a.md"].Reason != want {
		t.Errorf("moving a.md: %s %q, want %q", applied["a.md"].Status, applied["a.md"].Reason, want)
	}
	if want := "Applied under the selected organization policy. Links updated in notes in the Trash: " + linker + " (from linker.md)."; applied["b.md"].Status != "applied" || applied["b.md"].Reason != want {
		t.Errorf("moving b.md: %s %q, want %q", applied["b.md"].Status, applied["b.md"].Reason, want)
	}
	if got, want := readLinker(), "# Linker\n[A](../dest/a.md) and [B](../dest/b.md)\n"; got != want {
		t.Fatalf("the linker in the Trash after the moves: %q, want %q", got, want)
	}

	w := proxyCall(app, "alice", "POST", "/p/default/api/jev/jobs/"+id+"/organization/undo", `{}`)
	if w.Code != 200 || strings.Contains(w.Body.String(), `"error"`) {
		t.Fatal("Undo run:", w.Code, w.Body.String())
	}
	if got := readLinker(); got != original {
		t.Errorf("the linker in the Trash after Undo run: %q, want %q", got, original)
	}
	undone := organizerMoveResults(t, waitReview(t, app, id))
	for _, p := range []string{"a.md", "b.md"} {
		if want := "Undone. Links reverted in notes in the Trash: " + linker + " (from linker.md)."; undone[p].Status != "undone" || undone[p].Reason != want {
			t.Errorf("undoing the move of %s: %s %q, want %q", p, undone[p].Status, undone[p].Reason, want)
		}
	}
}
