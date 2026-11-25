package chat

import (
	"encoding/json"
	"errors"
	"forgetmenote/internal/pg/pgtest"
	"testing"
)

func TestConversationDurablePrivateAndRestart(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(pgtest.Dir(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.Create("alice", "alpha", Options{Connection: "local", Model: "test", Mode: "ask"})
	if err != nil {
		t.Fatal(err)
	}
	if c.ID == "" || c.Owner != "alice" || c.Project != "alpha" {
		t.Fatalf("missing durable identity: %+v", c)
	}
	err = s.Update("alice", "alpha", c.ID, func(c *Conversation) error {
		c.Turns = append(c.Turns, Turn{ID: "turn", Role: "user", Content: "private evidence"})
		c.Runs = append(c.Runs, Run{ID: "run", State: "running"})
		c.AddEvent("run", "text", map[string]string{"text": "durable answer"})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]string{{"bob", "alpha"}, {"alice", "beta"}} {
		if _, err = s.Read(pair[0], pair[1], c.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("cross-owner/project access: %v", err)
		}
	}
	s.Close()
	s, err = OpenStore(pgtest.Dir(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.Read("alice", "alpha", c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Turns) != 1 || got.Turns[0].Content != "private evidence" || got.Runs[0].State != "interrupted" || len(got.Events) < 1 {
		t.Fatalf("state not recovered: %+v", got)
	}
	// Another gateway reads the same conversation.
	other, err := OpenStore(pgtest.Dir(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	r, err := other.Read("alice", "alpha", c.ID)
	if err != nil || r.Turns[0].Content != "private evidence" {
		t.Fatalf("another gateway could not read: %+v %v", r, err)
	}
}
func TestConversationFailedUpdateAndDamagedRecord(t *testing.T) {
	s, err := OpenStore(pgtest.Org(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c, err := s.Create("alice", "alpha", Options{Mode: "ask"})
	if err != nil {
		t.Fatal(err)
	}
	err = s.Update("alice", "alpha", c.ID, func(c *Conversation) error { c.Title = "lost"; return errors.New("reject") })
	if err == nil {
		t.Fatal("accepted rejected update")
	}
	got, _ := s.Read("alice", "alpha", c.ID)
	if got.Title == "lost" {
		t.Fatal("failed candidate published")
	}
	if _, err = s.Read("alice", "alpha", "../../outside"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unsafe ID %v", err)
	}
	storeRecord(t, s, "alice", c.ID, append(storedRecord(t, s, "alice", c.ID), []byte(`{"trailing":true}`)...))
	if _, err = s.Read("alice", "alpha", c.ID); err == nil {
		t.Fatal("read a damaged record")
	}
}

// A conversation another gateway wrote after this one read it is refused,
// never overwritten, and the next read sees it.
func TestGatewaysRefuseStaleConversationWrites(t *testing.T) {
	db := pgtest.Org(t)
	first, err := OpenStore(db)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := OpenStore(db)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	c, err := first.Create("alice", "alpha", Options{Mode: "ask"})
	if err != nil {
		t.Fatal(err)
	}
	stale, version, err := first.loadVersion("alice", "alpha", c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = second.Update("alice", "alpha", c.ID, func(c *Conversation) error { c.Title = "Second gateway"; return nil }); err != nil {
		t.Fatal(err)
	}
	generation := first.Generation("alice", c.ID)
	if generation == uint64(version) {
		t.Fatal("another gateway's write did not change the generation")
	}
	stale.Title = "Stale write"
	if err = first.persist(stale, version); !errors.Is(err, ErrConflict) {
		t.Fatalf("a stale write was not refused: %v", err)
	}
	got, err := first.Read("alice", "alpha", c.ID)
	if err != nil || got.Title != "Second gateway" {
		t.Fatalf("another gateway's write was lost: %+v %v", got, err)
	}
	run := Run{ID: NewID(), SubmissionID: "one", State: "queued", RequestHash: "hash", CreatedAt: Now(), Options: Options{Corpus: testCorpus()}}
	if _, _, err = second.Start("alice", "alpha", c.ID, run, Turn{ID: "turn", Role: "user", Content: "hello"}); err != nil {
		t.Fatal(err)
	}
	other, err := first.Create("alice", "alpha", Options{Mode: "ask"})
	if err != nil {
		t.Fatal(err)
	}
	run.ID, run.SubmissionID = NewID(), "two"
	if _, _, err = first.Start("alice", "alpha", other.ID, run, Turn{ID: "turn", Role: "user", Content: "hello"}); err == nil {
		t.Fatal("a second run started while another gateway's run was active")
	}
}

func TestConversationEventSequenceAndBound(t *testing.T) {
	s, err := OpenStore(pgtest.Org(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c, err := s.Create("alice", "alpha", Options{Mode: "ask"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err = s.Update("alice", "alpha", c.ID, func(c *Conversation) error { c.AddEvent("run", "progress", json.RawMessage(`{"step":1}`)); return nil }); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := s.Read("alice", "alpha", c.ID)
	for i, e := range got.Events {
		if e.Sequence != int64(i+1) {
			t.Fatalf("unordered event %+v", e)
		}
	}
}

// A gateway that opens leaves the runs another live gateway executes, and
// interrupts them once that gateway is gone.
func TestGatewaysInterruptOnlyRunsOfAGoneGateway(t *testing.T) {
	db := pgtest.Org(t)
	otherDB, leave := pgtest.Gateway(t, db)
	executing, err := OpenStore(otherDB)
	if err != nil {
		t.Fatal(err)
	}
	defer executing.Close()
	c, err := executing.Create("alice", "alpha", Options{Mode: "ask"})
	if err != nil {
		t.Fatal(err)
	}
	run := Run{ID: NewID(), SubmissionID: "one", State: "queued", RequestHash: "hash", CreatedAt: Now(), Options: Options{Corpus: testCorpus()}}
	if _, _, err = executing.Start("alice", "alpha", c.ID, run, Turn{ID: "turn", Role: "user", Content: "hello"}); err != nil {
		t.Fatal(err)
	}
	opened, err := OpenStore(db)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	if got, err := opened.Read("alice", "alpha", c.ID); err != nil || got.Run(run.ID).State != "queued" {
		t.Fatalf("opening interrupted a live gateway's run: %+v %v", got.Run(run.ID), err)
	}
	if err = opened.RecoverGone(); err != nil {
		t.Fatal(err)
	}
	if got, _ := opened.Read("alice", "alpha", c.ID); got.Run(run.ID).State != "queued" {
		t.Fatal("recovery interrupted a live gateway's run")
	}
	leave()
	if err = opened.RecoverGone(); err != nil {
		t.Fatal(err)
	}
	if got, err := opened.Read("alice", "alpha", c.ID); err != nil || got.Run(run.ID).State != "interrupted" {
		t.Fatalf("the run of a gateway that is gone was left: %+v %v", got.Run(run.ID), err)
	}
}
