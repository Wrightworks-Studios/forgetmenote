package workspace

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
)

// browserText applies an answer's update to a browser's document and
// returns its text.
func browserText(t *testing.T, doc *crdt.Doc, v LiveDocument) string {
	t.Helper()
	if err := crdt.ApplyUpdateV1(doc, v.Update, nil); err != nil {
		t.Fatal(err)
	}
	return doc.GetText("markdown").ToString()
}

// A browser that names the live revision it holds gets only what changed
// after it: an update that brings its document to the server's text, with
// the revision and checkpoint metadata but without the Markdown.
func TestLivePollAnswersOnlyTheChangeSinceTheKnownRevision(t *testing.T) {
	s, ctx, _ := liveTestStore(t)
	large := "# Manuscript\n\n" + strings.Repeat("Call me Ishmael. Some years ago, never mind how long precisely.\n\n", 4000)
	if _, err := s.Save(ctx, "book.md", []byte(large), "", ""); err != nil {
		t.Fatal(err)
	}
	base, err := s.LiveRead(ctx, "book.md")
	if err != nil {
		t.Fatal(err)
	}
	reader, err := browserDoc(base.Update)
	if err != nil {
		t.Fatal(err)
	}
	patched, err := s.LivePatch(ctx, TargetedPatch{Path: "book.md", Base: base.LiveRevision, OperationID: "agent-word", Edits: []TextEdit{{Start: 2, End: 12, Old: "Manuscript", Text: "Moby-Dick"}}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.WaitLive(ctx, "book.md", liveKnown(base), 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Incremental || got.Content != "" || got.LiveRevision != patched.LiveRevision || got.Version != patched.Version || !got.Pending || got.Revision != patched.Revision || got.Path != "book.md" {
		t.Fatalf("incremental answer: %+v", got)
	}
	if len(got.Update) > 512 {
		t.Fatalf("a one-word change answered with %d bytes for a %d-byte note", len(got.Update), len(large))
	}
	if text := browserText(t, reader, got); text != patched.Content {
		t.Fatalf("the update does not bring the reader to the server's text: %q", text[:40])
	}
	// The browser's next poll names the revision it now holds.
	if v, err := s.WaitLive(ctx, "book.md", liveKnown(got), 0, true); err != nil || !v.Unchanged {
		t.Fatalf("poll after catching up: %+v %v", v, err)
	}
	// A checkpoint changes only the metadata: the answer says so, and its
	// update changes no text.
	checkpoint, err := s.LiveCheckpoint(ctx, "book.md")
	if err != nil {
		t.Fatal(err)
	}
	got, err = s.WaitLive(ctx, "book.md", liveKnown(got), 0, true)
	if err != nil || !got.Incremental || got.Pending || got.Version != checkpoint.Version || got.Revision != checkpoint.Revision {
		t.Fatalf("checkpoint answer: %+v %v", got, err)
	}
	if text := browserText(t, reader, got); text != patched.Content {
		t.Fatal("a checkpoint changed the reader's text")
	}
}

// The whole note answers every reader that does not ask for less, a browser
// running the previous JavaScript included, and every reader whose revision
// the server cannot serve a change from.
func TestLivePollAnswersTheWholeNoteWhenItCannotServeAChange(t *testing.T) {
	s, ctx, _ := liveTestStore(t)
	base, err := s.LiveRead(ctx, "note.md")
	if err != nil {
		t.Fatal(err)
	}
	patched, err := s.LivePatch(ctx, TargetedPatch{Path: "note.md", Base: base.LiveRevision, OperationID: "agent", Edits: []TextEdit{{Start: 3, End: 8, Old: "alpha", Text: "gamma"}}})
	if err != nil {
		t.Fatal(err)
	}
	whole := func(v LiveDocument, err error, what string) {
		t.Helper()
		if err != nil || v.Incremental || v.Content != patched.Content || v.LiveRevision != patched.LiveRevision {
			t.Fatalf("%s: %+v %v", what, v, err)
		}
		doc, err := browserDoc(v.Update)
		if err != nil || doc.GetText("markdown").ToString() != patched.Content {
			t.Fatalf("%s: the update is not the whole state: %v", what, err)
		}
	}
	v, err := s.WaitLive(ctx, "note.md", liveKnown(base), 0, false)
	whole(v, err, "the previous request form")
	v, err = s.WaitLive(ctx, "note.md", liveKnown(base), 25000, false)
	whole(v, err, "the previous long poll")
	stale := liveKnown(base)
	stale = "0000000000000000aaaaaaaaaaaaaaaa:7" + stale[strings.Index(stale, "/"):]
	v, err = s.WaitLive(ctx, "note.md", stale, 0, true)
	whole(v, err, "a revision of another generation")
	v, err = s.WaitLive(ctx, "note.md", "", 0, true)
	whole(v, err, "no revision")
	// A revision older than the patch bases the server keeps. The bases are
	// dropped from the loaded record only: the live listener's first
	// (re)connect hint marks it stale whenever it arrives, and the poll then
	// reloads the stored record with every base, so drop them again then.
	n := liveDB(s)
	for attempt := 1; ; attempt++ {
		n.mu.Lock()
		record := n.live["note.md"]
		record.Versions = record.Versions[len(record.Versions)-1:]
		n.mu.Unlock()
		v, err = s.WaitLive(ctx, "note.md", liveKnown(base), 0, true)
		n.mu.Lock()
		reloaded := n.live["note.md"] != record
		n.mu.Unlock()
		if !reloaded || attempt == 5 {
			break
		}
	}
	whole(v, err, "an expired revision")
}

// A browser's own update is acknowledged with only what it lacks: its own
// update and what others added since the revision it named. The revision it
// names is not part of the operation, so a retry naming another stays a
// replay; and a browser that names none gets the whole note, as before.
func TestLiveApplyAcknowledgesWithOnlyTheChange(t *testing.T) {
	s, ctx, _ := liveTestStore(t)
	base, err := s.LiveRead(ctx, "note.md")
	if err != nil {
		t.Fatal(err)
	}
	generation := strings.Split(base.LiveRevision, ":")[0]
	writer, _ := browserDoc(base.Update)
	known := writer.StateVector()
	writer.Transact(func(tx *crdt.Transaction) { tx.GetText("markdown").Insert(tx, 8, " typed", nil) })
	update := crdt.EncodeStateAsUpdateV1(writer, known)
	// An agent's change the writer has not seen.
	agent, err := s.LivePatch(ctx, TargetedPatch{Path: "note.md", Base: base.LiveRevision, OperationID: "agent", Edits: []TextEdit{{Start: 10, End: 14, Old: "beta", Text: "BETA"}}})
	if err != nil {
		t.Fatal(err)
	}
	in := LiveUpdate{Path: "note.md", Generation: generation, OperationID: "typed", Update: update}
	ack, err := s.LiveApply(WithLiveAnswer(ctx, LiveAnswer{Since: base.LiveRevision}), in)
	if err != nil {
		t.Fatal(err)
	}
	if !ack.Incremental || ack.Content != "" || !ack.Pending || ack.LiveRevision == agent.LiveRevision {
		t.Fatalf("acknowledgement: %+v", ack)
	}
	server, err := s.LiveRead(ctx, "note.md")
	if err != nil {
		t.Fatal(err)
	}
	if text := browserText(t, writer, ack); text != server.Content || !strings.Contains(text, "typed") || !strings.Contains(text, "BETA") {
		t.Fatalf("the writer's text after the acknowledgement: %q, server %q", text, server.Content)
	}
	// A lost acknowledgement's retry names the revision the browser then held.
	retry, err := s.LiveApply(WithLiveAnswer(ctx, LiveAnswer{Since: agent.LiveRevision}), in)
	if err != nil || !retry.Incremental || retry.LiveRevision != ack.LiveRevision {
		t.Fatalf("retry naming another revision: %+v %v", retry, err)
	}
	old, err := s.LiveApply(ctx, in)
	if err != nil || old.Incremental || old.Content != server.Content || old.LiveRevision != ack.LiveRevision {
		t.Fatalf("the previous request form's retry: %+v %v", old, err)
	}
	in.Update = crdt.EncodeStateAsUpdateV1(writer, nil)
	if _, err = s.LiveApply(ctx, in); !errors.Is(err, ErrConflict) {
		t.Fatalf("a changed payload under the same operation: %v", err)
	}
}

// A long poll past the notebook's limit is refused as capacity, which
// callers retry, and leaves the waiting polls as they were.
func TestLiveWaitPastItsLimitIsACapacityRefusal(t *testing.T) {
	s, _, _ := liveTestStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	base, err := s.LiveRead(ctx, "note.md")
	if err != nil {
		t.Fatal(err)
	}
	waitCtx, stop := context.WithCancel(ctx)
	var waiting sync.WaitGroup
	for i := 0; i < maxLiveWaiters; i++ {
		waiting.Add(1)
		go func() { defer waiting.Done(); s.WaitLive(waitCtx, "note.md", liveKnown(base), 25000, true) }()
	}
	waitForLiveReaders(t, s, maxLiveWaiters)
	if _, err := s.WaitLive(ctx, "note.md", liveKnown(base), 25000, true); !errors.Is(err, ErrLiveCapacity) {
		t.Fatalf("a poll past the limit: %v", err)
	}
	waitForLiveReaders(t, s, maxLiveWaiters)
	stop()
	waiting.Wait()
	if v, err := s.WaitLive(ctx, "note.md", liveKnown(base), 0, true); err != nil || !v.Unchanged {
		t.Fatalf("a poll after the others ended: %+v %v", v, err)
	}
}

// After a restart, the first write checkpoints accepted live edits nobody
// has used since, rather than leaving them behind an ordinary save of the
// same note.
func TestFirstWriteAfterRestartCheckpointsStoredLiveEdits(t *testing.T) {
	s, ctx, _ := liveTestStore(t)
	base, err := s.LiveRead(ctx, "note.md")
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := s.LivePatch(ctx, TargetedPatch{Path: "note.md", Base: base.LiveRevision, OperationID: "before-restart", Edits: []TextEdit{{Start: 3, End: 8, Old: "alpha", Text: "accepted"}}})
	if err != nil {
		t.Fatal(err)
	}
	reopened := reopenTestStore(t, s)
	committed, err := reopened.Read(ctx, "note.md", "")
	if err != nil {
		t.Fatal(err)
	}
	// A save of another note commits the pending live edits first.
	if _, err = reopened.Save(ctx, "other.md", []byte("other\n"), "", ""); err != nil {
		t.Fatal(err)
	}
	checkpointed, err := reopened.Read(ctx, "note.md", "")
	if err != nil || string(checkpointed.Content) != accepted.Content || checkpointed.Version == committed.Version {
		t.Fatalf("pending live edits were not checkpointed before the write: %q %v", checkpointed.Content, err)
	}
	// An ordinary save of the note over the checkpoint now conflicts with
	// the version it expected, instead of replacing the accepted edits.
	if _, err = reopened.Save(ctx, "note.md", []byte("replacement\n"), committed.Version, ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("a stale save replaced checkpointed live edits: %v", err)
	}
	live, err := reopened.LiveRead(ctx, "note.md")
	if err != nil || live.Content != accepted.Content || live.Pending {
		t.Fatalf("live note after the checkpoint: %+v %v", live, err)
	}
}
