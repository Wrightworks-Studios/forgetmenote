package workspace

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestLiveRestartEvictsIdleCommittedRecordAtCapacity(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	// A near 1 MiB record holds about 3 MiB: its state, text and checkpoint
	// text. At an eighth of the capacity five fit and a sixth does not.
	defer func(full int) { liveCapacity = full }(liveCapacity)
	liveCapacity /= 8
	content := strings.Repeat("plain note text\n", (MaxLiveBytes-64)/len("plain note text\n"))
	paths := make([]string, 6)
	for i := range paths {
		paths[i] = fmt.Sprintf("note-%02d.md", i)
		if _, err := s.Save(ctx, paths[i], []byte(content), "", ""); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range paths {
		if _, err := s.LiveRead(ctx, path); err != nil {
			t.Fatal(err)
		}
	}
	for i, path := range paths[:5] {
		base, err := s.LiveRead(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.LivePatch(ctx, TargetedPatch{Path: path, Base: base.LiveRevision, OperationID: fmt.Sprintf("capacity-edit-%d", i), Edits: []TextEdit{{Start: 0, End: 5, Old: "plain", Text: "saved"}}}); err != nil {
			t.Fatal(err)
		}
	}
	// The pending records, which still fit, reopen.
	reopened, err := reopenLiveStore(t, s)
	if err != nil {
		t.Fatalf("accepted near-capacity records could not reopen: %v", err)
	}
	if pending := len(reopened.live); pending != 5 {
		t.Fatalf("restart holds %d documents, not the 5 pending", pending)
	}
	want := "saved" + content[5:]
	for _, path := range paths[:5] {
		live, err := reopened.LiveRead(ctx, path)
		if err != nil || live.Content != want {
			t.Fatalf("accepted text failed to recover for %s: %v", path, err)
		}
		checkpoint, err := reopened.LiveCheckpoint(ctx, path)
		if err != nil || checkpoint.Pending || checkpoint.Content != want {
			t.Fatalf("recovered text did not checkpoint for %s: %v", path, err)
		}
		committed, err := reopened.Read(ctx, path, checkpoint.Revision)
		if err != nil || string(committed.Content) != want {
			t.Fatalf("Git lost recovered text for %s: %v", path, err)
		}
	}
	idle, err := reopened.LiveRead(ctx, paths[5])
	if err != nil || idle.Pending || idle.Content != content {
		t.Fatalf("evicted committed record was not recoverable: %v", err)
	}
}
