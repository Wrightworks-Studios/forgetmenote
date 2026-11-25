package workspace

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"forgetmenote/internal/pg/pgtest"
)

// A bundle import's time starts once its whole upload is read, so an upload
// that takes longer to arrive than the import may take still imports.
func TestImportTimeStartsOnceTheUploadIsRead(t *testing.T) {
	defer func(timeout time.Duration, rate int64) { importTimeout, importRate = timeout, rate }(importTimeout, importRate)
	importTimeout, importRate = 5*time.Second, 1<<40
	path := gitBundle(t, map[string]string{"notes/a.md": "# A\n"})
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	notes := newTestNotes(t)
	upload, send := io.Pipe()
	go func() {
		send.Write(raw[:len(raw)/2])
		time.Sleep(6 * time.Second)
		send.Write(raw[len(raw)/2:])
		send.Close()
	}()
	if err := ImportBundleDB(context.Background(), notes, "slow", t.TempDir(), upload); err != nil {
		t.Fatal("an upload slower than the import's time:", err)
	}
	if head, err := notes.Notebook("slow").Head(context.Background()); err != nil || head.Commit.String() != bundleHead(t, path) {
		t.Fatalf("the import is at %v: %v", head, err)
	}
}

// A bundle import has time in proportion to its bundle, and running out of
// it, or of the caller's time, whether the bundle is being checked or its
// history stored, is a timeout that stores nothing, never a refused bundle.
func TestImportTimeGrowsWithTheBundleAndRunsOutAsATimeout(t *testing.T) {
	defer func(timeout time.Duration, rate int64) { importTimeout, importRate = timeout, rate }(importTimeout, importRate)
	ctx := context.Background()
	raw, err := os.ReadFile(gitBundle(t, map[string]string{"notes/a.md": "# A\n"}))
	if err != nil {
		t.Fatal(err)
	}
	timedOut := func(t *testing.T, err error) {
		t.Helper()
		if !errors.Is(err, ErrBundleTimeout) || errors.Is(err, ErrBundle) || errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "does not show that the bundle is damaged") {
			t.Fatal("an import that ran out of time:", err)
		}
	}

	// With almost no time of its own, the import has what the bundle's size
	// earns it: a minute for this one; and none for a bundle a billionth of
	// its size per second.
	importTimeout, importRate = time.Nanosecond, int64(len(raw))/60
	if limit := importLimit(int64(len(raw))); limit < time.Minute || limit > 70*time.Second {
		t.Fatal("the bundle's time:", limit)
	}
	notes := newTestNotes(t)
	if err := ImportBundleDB(ctx, notes, "timely", t.TempDir(), bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	importRate = 1 << 50
	timedOut(t, ImportBundleDB(ctx, notes, "late", t.TempDir(), bytes.NewReader(raw)))
	absent(t, notes, "late")

	// Storing the checked history, held as a slow database would, runs out
	// of the import's time, and of the caller's. Either time ends only once
	// the import waits to store its first commit, however long the bundle's
	// check took on a busy host.
	for _, scenario := range []string{"own time", "caller's deadline"} {
		t.Run(scenario, func(t *testing.T) {
			s := newTestStore(t)
			waiting, release := pgtest.HoldWrites(t, testEnv(t, s).db, "notebook_heads")
			importTimeout, importRate = 5*time.Second, 1<<50
			ownTime := make(chan func(), 1)
			defer func(after func(time.Duration, func()) *time.Timer) { importAfter = after }(importAfter)
			importAfter = func(_ time.Duration, f func()) *time.Timer {
				ownTime <- f
				return time.NewTimer(time.Hour)
			}
			caller := &passingDeadline{Context: ctx, done: make(chan struct{})}
			failed := make(chan error, 1)
			go func() { failed <- ImportBundleDB(caller, s.noteStore, "held", t.TempDir(), bytes.NewReader(raw)) }()
			waiting(1)
			if scenario == "own time" {
				(<-ownTime)()
			} else {
				close(caller.done)
			}
			err := <-failed
			release()
			timedOut(t, err)
			absent(t, s.noteStore, "held")
		})
	}
}

// passingDeadline is a caller's context whose deadline passes when done is
// closed.
type passingDeadline struct {
	context.Context
	done chan struct{}
}

func (c *passingDeadline) Done() <-chan struct{} { return c.done }

func (c *passingDeadline) Err() error {
	select {
	case <-c.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}
