package chat

import (
	"context"
	"errors"
	"forgetmenote/internal/pg/pgtest"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestStartDoesNotDecodeCompletedHistory(t *testing.T) {
	for _, resume := range []bool{false, true} {
		for _, n := range []int{0, 100} {
			s, e := OpenStore(pgtest.Org(t))
			if e != nil {
				t.Fatal(e)
			}
			ids := []string{}
			opts := Options{Mode: "ask", Corpus: &Corpus{Version: 1, Archives: "include", Drafts: "include", NoteProjects: []string{"book"}}}
			for i := 0; i < n; i++ {
				c, e := s.Create("alice", "book", opts)
				if e != nil {
					t.Fatal(e)
				}
				ids = append(ids, c.ID)
			}
			target, e := s.Create("alice", "book", opts)
			if e != nil {
				t.Fatal(e)
			}
			text := strings.Repeat("completed old evidence ", 3000)
			for _, id := range ids {
				if e = s.Update("alice", "book", id, func(c *Conversation) error {
					c.Turns = []Turn{{ID: NewID(), Role: "assistant", Content: text}}
					c.Runs = []Run{{ID: NewID(), State: "completed"}}
					return nil
				}); e != nil {
					t.Fatal(e)
				}
			}
			if resume {
				if e = s.Update("alice", "book", target.ID, func(c *Conversation) error {
					c.Runs = []Run{{ID: "resume", State: "interrupted", Options: opts}}
					return nil
				}); e != nil {
					t.Fatal(e)
				}
			}
			runtime.GC()
			var a, b runtime.MemStats
			runtime.ReadMemStats(&a)
			started := time.Now()
			if resume {
				_, e = s.Resume("alice", "book", target.ID, "resume", 2, 2, 1)
			} else {
				_, _, e = s.Start("alice", "book", target.ID, Run{ID: NewID(), SubmissionID: NewID(), State: "queued", Options: opts}, Turn{ID: NewID(), Role: "user", Content: "hello"})
			}
			elapsed := time.Since(started)
			runtime.ReadMemStats(&b)
			if e != nil {
				t.Fatal(e)
			}
			if allocated := b.TotalAlloc - a.TotalAlloc; allocated > 8<<20 {
				t.Errorf("Run admission decoded unrelated history: allocated %d bytes", allocated)
			}
			t.Logf("resume=%v completed_conversations=%d historical_text_bytes=%d admission_elapsed=%s admission_total_alloc=%d", resume, n, n*len(text), elapsed, b.TotalAlloc-a.TotalAlloc)
			s.Close()
		}
	}
}

func TestActiveAdmissionTracksDurableUpdatesAndRecovery(t *testing.T) {
	root := t.TempDir()
	s, err := OpenStore(pgtest.Dir(t, root))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	opts := Options{Mode: "ask", Corpus: testCorpus()}
	first, err := s.Create("alice", "alpha", opts)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Create("alice", "beta", opts)
	if err != nil {
		t.Fatal(err)
	}
	bob, err := s.Create("bob", "alpha", opts)
	if err != nil {
		t.Fatal(err)
	}
	run := Run{ID: "first", SubmissionID: "submission", RequestHash: "request", State: "awaiting_confirmation", Options: opts}
	start := func(user, project, id string, run Run) error {
		_, _, e := s.Start(user, project, id, run, Turn{Content: "hello"})
		return e
	}
	if err = start("alice", "alpha", first.ID, run); err != nil {
		t.Fatal(err)
	}
	if _, replay, e := s.Start("alice", "alpha", first.ID, run, Turn{}); e != nil || !replay {
		t.Fatalf("submission replay: %v %v", replay, e)
	}
	conflict := run
	conflict.RequestHash = "changed"
	if _, _, e := s.Start("alice", "alpha", first.ID, conflict, Turn{}); !errors.Is(e, ErrConflict) {
		t.Fatal("conflicting submission admitted", e)
	}
	next := run
	next.ID = "second"
	next.SubmissionID = "second"
	if err = start("alice", "beta", second.ID, next); err == nil {
		t.Fatal("same owner admitted across notebooks")
	}
	if err = start("bob", "alpha", bob.ID, next); err != nil {
		t.Fatal("different owner blocked", err)
	}
	s.Close()
	s, err = OpenStore(pgtest.Dir(t, root))
	if err != nil {
		t.Fatal(err)
	}
	if err = start("alice", "beta", second.ID, next); err == nil {
		t.Fatal("restart lost pending confirmation")
	}
	if err = s.Update("alice", "alpha", first.ID, func(c *Conversation) error {
		c.Runs[0].State = "completed"
		c.Turns = append(c.Turns, Turn{Content: strings.Repeat("x", maxRecordBytes)})
		return nil
	}); !errors.Is(err, ErrCapacity) {
		t.Fatal("expected rejected completion", err)
	}
	if err = start("alice", "beta", second.ID, next); err == nil {
		t.Fatal("failed completion released admission")
	}
	if err = s.Update("alice", "alpha", first.ID, func(c *Conversation) error { c.Runs[0].State = "completed"; return nil }); err != nil {
		t.Fatal(err)
	}
	if err = s.Update("alice", "beta", second.ID, func(c *Conversation) error {
		c.Runs = append(c.Runs, next)
		c.Turns = append(c.Turns, Turn{Content: strings.Repeat("x", maxRecordBytes)})
		return nil
	}); !errors.Is(err, ErrCapacity) {
		t.Fatal("expected rejected start", err)
	}
	if err = start("alice", "beta", second.ID, next); err != nil {
		t.Fatal("failed publication blocked next run", err)
	}
	if err = s.Update("alice", "beta", second.ID, func(c *Conversation) error { c.Runs[0].State = "running"; return nil }); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = OpenStore(pgtest.Dir(t, root))
	if err != nil {
		t.Fatal(err)
	}
	if err = start("alice", "alpha", first.ID, next); err != nil {
		t.Fatal("interrupted recovery retained admission", err)
	}
}

func TestRuntimeAdmissionDoesNotDecodeCompletedHistory(t *testing.T) {
	s, err := OpenStore(pgtest.Org(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	opts := Options{Mode: "ask", Corpus: testCorpus()}
	ids := []string{}
	for i := 0; i < 100; i++ {
		c, e := s.Create("alice", "book", opts)
		if e != nil {
			t.Fatal(e)
		}
		ids = append(ids, c.ID)
	}
	target, err := s.Create("alice", "book", opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if err = s.Update("alice", "book", id, func(c *Conversation) error {
			c.Turns = []Turn{{Content: strings.Repeat("completed old evidence ", 3000)}}
			c.Runs = []Run{{ID: NewID(), State: "completed"}}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &Config{Limits: Limits{MaxConcurrent: 1}}
	conn := Connection{ID: "synthetic", Limits: Limits{ConnectionConcurrent: 1}}
	rt := &Runtime{Store: s, Config: cfg, Active: map[string]context.CancelFunc{}, Connections: map[string]int{}}
	run := Run{ID: NewID(), SubmissionID: NewID(), State: "queued", Options: opts}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	started := time.Now()
	replay, err := rt.Admit(cfg, conn, run.ID, func() {}, "alice", func(string, string, uint64) error { return nil }, func() (bool, error) {
		_, replayed, e := s.Start("alice", "book", target.ID, run, Turn{Content: "hello"})
		return replayed, e
	})
	elapsed := time.Since(started)
	runtime.ReadMemStats(&after)
	if err != nil || replay {
		t.Fatalf("admit: %v replay=%v", err, replay)
	}
	rt.Finish(run.ID, conn.ID)
	allocated := after.TotalAlloc - before.TotalAlloc
	t.Logf("Runtime.Admit 100 completed/6.9MB: elapsed=%s allocated=%d", elapsed, allocated)
	if allocated > 8<<20 {
		t.Errorf("runtime admission decoded completed history: %d bytes", allocated)
	}
}

func TestRuntimeReconcilesOnlyRevokedInactiveConfirmations(t *testing.T) {
	rt, err := OpenRuntime(pgtest.Org(t))
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	opts := Options{Corpus: testCorpus()}
	chats := map[string]string{}
	for _, user := range []string{"alice", "bob"} {
		c, e := rt.Store.Create(user, "book", opts)
		if e != nil {
			t.Fatal(e)
		}
		chats[user] = c.ID
		if e = rt.Store.Update(user, "book", c.ID, func(c *Conversation) error {
			c.Runs = []Run{{ID: user, State: "awaiting_confirmation", Epoch: 7, Options: opts, Operations: []Operation{{State: "awaiting_confirmation", Confirmation: "private"}}}}
			return nil
		}); e != nil {
			t.Fatal(e)
		}
	}
	allowed := func(user, project string, epoch uint64) error {
		if user != "alice" || project != "book" || epoch != 7 {
			t.Fatalf("wrong reconciliation scope: %q %q %d", user, project, epoch)
		}
		return ErrNotFound
	}
	rt.Active["alice"] = func() {}
	if err = rt.ReconcilePaused("alice", allowed); err != nil {
		t.Fatal(err)
	}
	c, err := rt.Store.Read("alice", "book", chats["alice"])
	if err != nil || c.Runs[0].State != "awaiting_confirmation" {
		t.Fatal("active worker was retired", err)
	}
	delete(rt.Active, "alice")
	if err = rt.ReconcilePaused("alice", func(string, string, uint64) error { return nil }); err != nil {
		t.Fatal(err)
	}
	c, err = rt.Store.Read("alice", "book", chats["alice"])
	if err != nil || c.Runs[0].State != "awaiting_confirmation" {
		t.Fatal("authorized confirmation was retired", err)
	}
	if err = rt.ReconcilePaused("alice", allowed); err != nil {
		t.Fatal(err)
	}
	c, err = rt.Store.Read("alice", "book", chats["alice"])
	if err != nil || c.Runs[0].State != "cancelled" || c.Runs[0].Operations[0].Confirmation != "" || c.Runs[0].Operations[0].State != "declined" {
		t.Fatal("revoked confirmation retained", err)
	}
	other, err := rt.Store.Read("bob", "book", chats["bob"])
	if err != nil || other.Runs[0].State != "awaiting_confirmation" {
		t.Fatal("other owner affected", err)
	}
	if _, _, err = rt.Store.Start("alice", "book", chats["alice"], Run{ID: "new", SubmissionID: "new", State: "queued", Options: opts}, Turn{}); err != nil {
		t.Fatal("reconciliation retained active admission", err)
	}
}
