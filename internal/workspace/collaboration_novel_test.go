package workspace

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"forgetmenote/internal/attribution"
	"forgetmenote/internal/benchfixture"

	"github.com/reearth/ygo/crdt"
)

// Live editing takes the 1.2 MB F1 manuscript, past the Git backend's 1 MiB
// limit, and its polls, browser updates, agent patches, checkpoints and
// restarts stay fast.
func TestLiveNovelStaysFast(t *testing.T) {
	novel := benchfixture.NovelOrSkip(t)
	if len(novel) <= MaxLiveBytes {
		t.Fatalf("F1 is %d bytes, within the old live limit", len(novel))
	}
	s := newTestStore(t)
	ctx := attribution.With(context.Background(), attribution.Identity{ID: "alice", Name: "Alice", Email: "alice@example.test", Source: "browser"})
	const path = "manuscript/moby-dick.md"
	if _, err := s.Save(ctx, path, novel, "", ""); err != nil {
		t.Fatal(err)
	}
	var report []string
	measure := func(name string, runs int, budget time.Duration, op func(i int)) {
		t.Helper()
		took := make([]time.Duration, runs)
		for i := range took {
			started := time.Now()
			op(i)
			took[i] = time.Since(started)
		}
		slices.Sort(took)
		p50, worst := took[len(took)/2], took[len(took)-1]
		report = append(report, fmt.Sprintf("%s: runs=%d p50=%v max=%v", name, runs, p50.Round(time.Microsecond), worst.Round(time.Microsecond)))
		if p50 > budget {
			t.Errorf("%s takes %v at the median, over its %v budget", name, p50, budget)
		}
	}
	var live LiveDocument
	measure("first live read (start)", 1, 2*time.Second, func(int) {
		var err error
		if live, err = s.LiveRead(ctx, path); err != nil || live.Content != string(novel) {
			t.Fatal(err)
		}
	})
	measure("unchanged poll", 50, 20*time.Millisecond, func(int) {
		if v, err := s.WaitLive(ctx, path, liveKnown(live), 0, false); err != nil || !v.Unchanged {
			t.Fatal(v.Unchanged, err)
		}
	})
	// A browser sends its whole state after each burst of typing, as
	// frontend/collaboration.mjs does.
	browser, err := browserDoc(live.Update)
	if err != nil {
		t.Fatal(err)
	}
	generation := strings.Split(live.LiveRevision, ":")[0]
	at := strings.Index(live.Content, "Call me Ishmael.")
	measure("browser update (whole state)", 20, 500*time.Millisecond, func(i int) {
		browser.Transact(func(tx *crdt.Transaction) { tx.GetText("markdown").Insert(tx, at+i, "~", nil) })
		update := crdt.EncodeStateAsUpdateV1(browser, nil)
		if live, err = s.LiveApply(ctx, LiveUpdate{Path: path, Generation: generation, OperationID: fmt.Sprintf("keystroke-%d", i), Update: update}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(live.Content, strings.Repeat("~", 20)+"Call me Ishmael.") {
		t.Fatal("browser updates were lost")
	}
	measure("agent patch", 20, 500*time.Millisecond, func(i int) {
		old := "Queequeg"
		start := utf16Index(live.Content, strings.Index(live.Content, old))
		if live, err = s.LivePatch(ctx, TargetedPatch{Path: path, Base: live.LiveRevision, OperationID: fmt.Sprintf("patch-%d", i), Edits: []TextEdit{{Start: start, End: start + len(old), Old: old, Text: "Kokovoko"}}}); err != nil {
			t.Fatal(err)
		}
	})
	stored := liveStateOf(t, s, path)
	report = append(report, fmt.Sprintf("stored: snapshot=%d bytes, %d updates totalling %d bytes", len(stored.Snapshot), len(stored.Updates), func() (n int) {
		for _, u := range stored.Updates {
			n += len(u.Update)
		}
		return
	}()))
	var checkpoint LiveDocument
	measure("checkpoint", 1, 3*time.Second, func(int) {
		if checkpoint, err = s.LiveCheckpoint(ctx, path); err != nil || checkpoint.Pending || checkpoint.Content != live.Content {
			t.Fatal(err)
		}
	})
	if committed, err := s.Read(ctx, path, ""); err != nil || string(committed.Content) != live.Content {
		t.Fatal("the checkpoint lost text", err)
	}
	title := utf16Index(checkpoint.Content, strings.Index(checkpoint.Content, "# Moby-Dick"))
	if _, err = s.LivePatch(ctx, TargetedPatch{Path: path, Base: checkpoint.LiveRevision, OperationID: "pending-at-restart", Edits: []TextEdit{{Start: title, End: title, Text: "Draft. "}}}); err != nil {
		t.Fatal(err)
	}
	var reopened *Store
	measure("restart and recover", 1, 3*time.Second, func(int) {
		if reopened, err = reopenLiveStore(t, s); err != nil {
			t.Fatal(err)
		}
		if v, err := reopened.LiveRead(ctx, path); err != nil || !v.Pending || !strings.Contains(v.Content, "\nDraft. # Moby-Dick") {
			t.Fatal(err)
		}
	})
	for _, line := range report {
		t.Log(line)
	}
}

// utf16Index converts a byte offset of s into UTF-16 code units.
func utf16Index(s string, offset int) int {
	n := 0
	for _, r := range s[:offset] {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// BenchmarkLiveTypingInTheNovel is a browser typing in F1, one key per
// update as frontend/collaboration.mjs sends them: each LiveApply carries
// only the key, encoded against what the server acknowledged, and asks for
// only what changed since the revision the browser holds.
func BenchmarkLiveTypingInTheNovel(b *testing.B) {
	novel := benchfixture.NovelOrSkip(b)
	s := newTestStore(b)
	ctx := attribution.With(context.Background(), attribution.Identity{ID: "alice", Name: "Alice", Email: "alice@example.test", Source: "browser"})
	const path = "manuscript/moby-dick.md"
	if _, err := s.Save(ctx, path, novel, "", ""); err != nil {
		b.Fatal(err)
	}
	live, err := s.LiveRead(ctx, path)
	if err != nil {
		b.Fatal(err)
	}
	browser, err := browserDoc(live.Update)
	if err != nil {
		b.Fatal(err)
	}
	generation := strings.Split(live.LiveRevision, ":")[0]
	at := strings.Index(live.Content, "Call me Ishmael.")
	key := func(i int) {
		acknowledged := browser.StateVector()
		browser.Transact(func(tx *crdt.Transaction) { tx.GetText("markdown").Insert(tx, at+i, "~", nil) })
		update := crdt.EncodeStateAsUpdateV1(browser, acknowledged)
		answer, err := s.LiveApply(WithLiveAnswer(ctx, LiveAnswer{Since: live.LiveRevision, Tab: "bench"}), LiveUpdate{Path: path, Generation: generation, OperationID: fmt.Sprintf("key-%d", i), Update: update})
		if err != nil || !answer.Incremental {
			b.Fatal(answer.Incremental, err)
		}
		if err = crdt.ApplyUpdateV1(browser, answer.Update, nil); err != nil {
			b.Fatal(err)
		}
		live = answer
	}
	b.ResetTimer()
	for i := range b.N {
		key(i)
	}
}
