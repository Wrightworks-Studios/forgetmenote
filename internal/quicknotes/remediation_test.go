package quicknotes

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/pg/pgtest"
	"forgetmenote/internal/workspace"
)

// Exercise public operations while their invalid input's old lock is held. ID
// rejection must happen before key construction/locking, even under contention.
func TestMalformedCaptureIDsRejectBeforeLock(t *testing.T) {
	s, c, _ := fixture(t)
	account, err := c.Account("alice")
	if err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("a", 1<<20)
	operations := map[string]func() error{
		"sort":    func() error { _, e := s.Sort(context.Background(), "alice", id); return e },
		"file":    func() error { _, e := s.File(context.Background(), "alice", id, Destination{}, ""); return e },
		"undo":    func() error { _, e := s.Undo(context.Background(), "alice", id); return e },
		"recover": func() error { _, e := s.Recover(context.Background(), "alice", id); return e },
	}
	for name, operation := range operations {
		t.Run(name, func(t *testing.T) {
			lock := s.objectLock(account.ID + "/" + id)
			lock.Lock()
			done := make(chan error, 1)
			go func() { done <- operation() }()
			select {
			case err := <-done:
				lock.Unlock()
				if !errors.Is(err, workspace.ErrForbidden) {
					t.Fatal(err)
				}
			case <-time.After(200 * time.Millisecond):
				lock.Unlock()
				<-done
				t.Fatal("malformed ID waited for an object lock")
			}
		})
	}
}

func retainedLockSlots(t *testing.T, s *Service) int {
	t.Helper()
	if registry, ok := any(&s.locks).(*sync.Map); ok {
		count := 0
		registry.Range(func(_, _ any) bool { count++; return true })
		return count
	}
	v := reflect.ValueOf(&s.locks).Elem()
	if v.Kind() != reflect.Array {
		t.Fatal("unexpected lock storage")
	}
	return v.Len()
}
func TestRejectedCaptureIDsDoNotRetainLockKeys(t *testing.T) {
	s, _, _ := fixture(t)
	baseline := retainedLockSlots(t, s)
	for i := 0; i < 128; i++ {
		id := fmt.Sprintf("%032x", i)
		var err error
		switch i % 4 {
		case 0:
			_, err = s.Sort(context.Background(), "alice", id)
		case 1:
			_, err = s.File(context.Background(), "alice", id, Destination{}, "")
		case 2:
			_, err = s.Undo(context.Background(), "alice", id)
		case 3:
			_, err = s.Recover(context.Background(), "alice", id)
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatal("missing capture", err)
		}
	}
	for i := 0; i < 4; i++ {
		_, err := s.Recover(context.Background(), "alice", strings.Repeat("a", 1<<20)+fmt.Sprint(i))
		if !errors.Is(err, workspace.ErrForbidden) {
			t.Fatal("malformed capture", err)
		}
	}
	if slots := retainedLockSlots(t, s); slots != baseline {
		t.Fatalf("rejected requests retained object locks: before=%d after=%d", baseline, slots)
	}
}

// Stop only the worker, retaining live contexts for deterministic calls to the
// exact scheduler used by its wake/timer loop. Every store is a real temp Git repo.
func stopScanWorker(t *testing.T, s *Service) {
	t.Helper()
	s.cancel()
	<-s.done
	s.ctx = context.Background()
}

func TestFailedCaptureScansDoNotStarveHealthyWork(t *testing.T) {
	for _, acrossAccounts := range []bool{false, true} {
		t.Run(fmt.Sprintf("across_accounts_%t", acrossAccounts), func(t *testing.T) {
			s, c, _ := fixture(t)
			stopScanWorker(t, s)
			ctx := context.Background()
			first, err := c.Account("alice")
			if err != nil {
				t.Fatal(err)
			}
			later, err := c.Account("bob")
			if err != nil {
				t.Fatal(err)
			}
			firstSubject, laterSubject := "alice", "bob"
			if first.ID > later.ID {
				first, later = later, first
				firstSubject, laterSubject = laterSubject, firstSubject
			}
			healthySubject := firstSubject
			if acrossAccounts {
				healthySubject = laterSubject
			}
			if err = c.Create(catalog.Bootstrap, "target", "Planning"); err != nil {
				t.Fatal(err)
			}
			if err = c.Member(catalog.Bootstrap, "target", healthySubject, "owner"); err != nil {
				t.Fatal(err)
			}
			settings, err := s.Settings(healthySubject)
			if err != nil {
				t.Fatal(err)
			}
			settings.CloudConsent = true
			if _, err = s.UpdateSettings(healthySubject, settings); err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			s.ConfigureJev(testJudge(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); routeReply(w, r, "destination_000") }), false)
			inbox, err := c.EnsureInbox(firstSubject)
			if err != nil {
				t.Fatal(err)
			}
			store, err := c.Access(firstSubject, inbox.ID, "owner")
			if err != nil {
				t.Fatal(err)
			}
			restore := pgtest.FailTable(t, s.db, "revisions")
			operations := make([]string, 9)
			for i := range operations {
				operations[i] = fmt.Sprintf("dirty-capture-%d", i)
			}
			sort.Slice(operations, func(i, j int) bool {
				return workspace.ContentHash([]byte(first.ID+"\x00"+operations[i])) < workspace.ContentHash([]byte(first.ID+"\x00"+operations[j]))
			})
			for _, op := range operations[:8] {
				if _, err = s.Capture(ctx, firstSubject, op, "original private draft"); err == nil {
					t.Fatal("capture saved while the notes store refused commits")
				}
			}
			restore()
			healthy, err := s.Capture(ctx, healthySubject, operations[8], "# Planning\nHealthy later capture.")
			if err != nil {
				t.Fatal(err)
			}
			s.scan(s.ctx)
			if calls.Load() != 0 {
				t.Fatal("first scan exceeded eight recovery attempts", calls.Load())
			}
			// Make the earlier failures eligible again: progress must follow the
			// cursor rather than depend only on their temporary backoff.
			for _, op := range operations[:8] {
				id := workspace.ContentHash([]byte(first.ID + "\x00" + op))[:32]
				r, err := s.readRecord(first.ID, id)
				if err != nil || !r.AwaitingClient || r.RetryFailures != 1 || r.RetryAfter <= time.Now().Unix() {
					t.Fatal("failed submission lost its durable retry state", r.RetryFailures, err)
				}
				r.RetryAfter = 0
				if err = s.writeRecord(&r); err != nil {
					t.Fatal(err)
				}
			}
			s.scan(s.ctx)
			if calls.Load() != 1 {
				t.Fatal("cursor did not advance past earlier eligible failures", calls.Load())
			}
			retriedFailures := 0
			for _, op := range operations[:8] {
				id := workspace.ContentHash([]byte(first.ID + "\x00" + op))[:32]
				r, err := s.readRecord(first.ID, id)
				if err != nil {
					t.Fatal(err)
				}
				if r.RetryFailures == 2 {
					retriedFailures++
				}
			}
			if retriedFailures != 7 {
				t.Fatal("scan did not retain its eight-attempt bound", retriedFailures)
			}
			s.scan(s.ctx)
			list, err := s.List(ctx, healthySubject)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, capture := range list {
				if capture.ID == healthy.ID && capture.Status == "suggested" && capture.Suggestion != nil {
					found = true
				}
			}
			if !found || calls.Load() != 1 {
				t.Errorf("failed saves starved healthy capture: suggested=%t calls=%d", found, calls.Load())
			}
			retried, err := s.Capture(ctx, firstSubject, operations[0], "original private draft")
			if err != nil {
				t.Fatal("original draft retry lost", err)
			}
			again, err := s.Capture(ctx, firstSubject, operations[0], "original private draft")
			if err != nil || again.ID != retried.ID || again.Version != retried.Version {
				t.Fatal("retry duplicated", again, err)
			}
			if _, err = s.Capture(ctx, firstSubject, operations[0], "changed draft"); !errors.Is(err, workspace.ErrConflict) {
				t.Fatal("changed payload replaced failed operation", err)
			}
			history, err := store.History(ctx, retried.Path)
			if err != nil || len(history) != 1 {
				t.Fatal("failed capture retry created duplicate commits", len(history), err)
			}
		})
	}
}

func TestConcurrentCaptureAndFilingKeepSingleReceipt(t *testing.T) {
	s, c, _ := fixture(t)
	routeTarget(t, c)
	ctx := context.Background()
	const n = 24
	type result struct {
		capture Capture
		err     error
	}
	results := make(chan result, n)
	for range n {
		go func() {
			capture, err := s.Capture(ctx, "alice", "contended-save", "exact original capture")
			results <- result{capture, err}
		}()
	}
	var saved Capture
	for i := 0; i < n; i++ {
		r := <-results
		if r.err != nil {
			t.Fatal(r.err)
		}
		if i == 0 {
			saved = r.capture
		}
		if r.capture.ID != saved.ID || r.capture.Version != saved.Version {
			t.Fatal("duplicate capture receipt", r.capture)
		}
	}
	for range n {
		go func() {
			capture, err := s.File(ctx, "alice", saved.ID, Destination{Project: "target"}, saved.Version)
			results <- result{capture, err}
		}()
	}
	var filed Capture
	for i := 0; i < n; i++ {
		r := <-results
		if r.err != nil {
			t.Fatal(r.err)
		}
		if i == 0 {
			filed = r.capture
		}
		if r.capture.Status != "filed" || r.capture.Version != filed.Version {
			t.Fatal("duplicate filing receipt", r.capture)
		}
	}
	target, err := c.Access("alice", "target", "owner")
	if err != nil {
		t.Fatal(err)
	}
	history, err := target.History(ctx, filed.Path)
	if err != nil || len(history) != 1 {
		t.Fatal("concurrent filing duplicated target commit", len(history), err)
	}
	undone, err := s.Undo(ctx, "alice", saved.ID)
	if err != nil || undone.Status != "inbox" {
		t.Fatal("concurrent filing lost undo state", undone, err)
	}
}

func TestRecoveryBackoffPersistsAndFindsLateCommit(t *testing.T) {
	s, c, root := fixture(t)
	stopScanWorker(t, s)
	ctx, account, err := s.actor(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	inbox, err := c.EnsureInbox("alice")
	if err != nil {
		t.Fatal(err)
	}
	owner, err := c.Access("alice", inbox.ID, "owner")
	if err != nil {
		t.Fatal(err)
	}
	restore := pgtest.FailTable(t, s.db, "revisions")
	const operation = "backoff-retained-draft"
	const content = "original unacknowledged client draft"
	if _, err = s.Capture(ctx, "alice", operation, content); err == nil {
		t.Fatal("capture saved while the notes store refused commits")
	}
	restore()
	id := workspace.ContentHash([]byte(account.ID + "\x00" + operation))[:32]
	s.scan(s.ctx)
	first, err := s.readRecord(account.ID, id)
	if err != nil || !first.AwaitingClient || first.RetryFailures != 1 || first.RetryAfter <= time.Now().Unix() {
		t.Fatal("missing receipt not marked for backed-off client retry", err)
	}
	s.scan(s.ctx)
	s.scan(s.ctx)
	unchanged, err := s.readRecord(account.ID, id)
	if err != nil || unchanged.RetryFailures != first.RetryFailures || unchanged.RetryAfter != first.RetryAfter {
		t.Fatal("background recovery ignored backoff", err)
	}
	s.Close()
	reopened, err := Open(pgtest.Dir(t, filepath.Join(root, "catalog")), c)
	if err != nil {
		t.Fatal(err)
	}
	if err = reopened.Start(); err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	stopScanWorker(t, reopened)
	reopened.scan(reopened.ctx)
	persisted, err := reopened.readRecord(account.ID, id)
	if err != nil || !persisted.AwaitingClient || persisted.RetryAfter != first.RetryAfter || persisted.RetryFailures != 1 {
		t.Fatal("restart lost retry state", err)
	}
	for attempt := 2; attempt <= 7; attempt++ {
		persisted.RetryAfter = 0
		if err = reopened.writeRecord(&persisted); err != nil {
			t.Fatal(err)
		}
		before := time.Now().Unix()
		reopened.scan(reopened.ctx)
		persisted, err = reopened.readRecord(account.ID, id)
		if err != nil {
			t.Fatal(err)
		}
		want := int64(30 * (1 << (attempt - 1)))
		if want > 300 {
			want = 300
		}
		if persisted.RetryAfter < before+want || persisted.RetryAfter > time.Now().Unix()+want || persisted.RetryFailures > 5 {
			t.Fatal("unbounded or missing retry delay", attempt, persisted.RetryFailures, persisted.RetryAfter-before)
		}
	}
	// A delayed owner request may commit after an earlier receipt probe returned
	// missing. The original operation must still be found after its retry is due.
	committed, err := owner.CaptureCreate(ctx, id, persisted.InboxPath, []byte(content))
	if err != nil {
		t.Fatal(err)
	}
	persisted.RetryAfter = 0
	if err = reopened.writeRecord(&persisted); err != nil {
		t.Fatal(err)
	}
	reopened.scan(reopened.ctx)
	list, err := reopened.List(ctx, "alice")
	if err != nil || len(list) != 1 || list[0].ID != id || list[0].Version != committed.Version {
		t.Fatal("late committed receipt not recovered", list, err)
	}
	recovered, err := reopened.readRecord(account.ID, id)
	if err != nil || recovered.AwaitingClient || recovered.RetryAfter != 0 || recovered.RetryFailures != 0 {
		t.Fatal("recovery did not clear retry state", err)
	}
	replay, err := reopened.Capture(ctx, "alice", operation, content)
	if err != nil || replay.Version != committed.Version {
		t.Fatal("recovered save lost original idempotence", replay, err)
	}
	history, err := owner.History(ctx, persisted.InboxPath)
	if err != nil || len(history) != 1 {
		t.Fatal("late receipt recovery duplicated commit", len(history), err)
	}
}

func TestScanAttemptsAtMostEightHealthyCaptures(t *testing.T) {
	s, c, _ := fixture(t)
	stopScanWorker(t, s)
	routeTarget(t, c)
	settings, err := s.Settings("alice")
	if err != nil {
		t.Fatal(err)
	}
	settings.CloudConsent = true
	if _, err = s.UpdateSettings("alice", settings); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	s.ConfigureJev(testJudge(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); routeReply(w, r, "destination_000") }), false)
	for i := 0; i < 20; i++ {
		if _, err = s.Capture(context.Background(), "alice", fmt.Sprintf("bounded-scan-%d", i), "Planning the next task."); err != nil {
			t.Fatal(err)
		}
	}
	for _, want := range []int32{8, 16, 20, 20} {
		s.scan(s.ctx)
		if calls.Load() != want {
			t.Fatal("unexpected bounded-batch progress", calls.Load(), want)
		}
	}
}
