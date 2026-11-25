package chat

import (
	"forgetmenote/internal/pg/pgtest"
	"testing"
	"time"
)

func TestStage2ResumeUsageChargedOnSpendDay(t *testing.T) {
	s, err := OpenStore(pgtest.Org(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c, err := s.Create("alice", "alpha", Options{Mode: "ask"})
	if err != nil {
		t.Fatal(err)
	}
	err = s.Update("alice", "alpha", c.ID, func(c *Conversation) error {
		c.Runs = []Run{{ID: "old", Options: Options{Corpus: testCorpus()}, State: "interrupted", CreatedAt: time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339Nano)}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Resume("alice", "alpha", c.ID, "old", 8, 16, 1); err != nil {
		t.Fatal(err)
	}
	err = s.Update("alice", "alpha", c.ID, func(c *Conversation) error {
		r := c.Run("old")
		r.State = "interrupted"
		r.Usage.InputTokens = 100
		r.Usage.Reserved = 0
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Resume("alice", "alpha", c.ID, "old", 8, 16, 1)
	if err != nil {
		t.Fatalf("recorded usage blocked a second continuation: %v", err)
	}
	got, err := s.Read("alice", "alpha", c.ID)
	if err != nil || got.Run("old").UsageDays[Day()].Tokens() != 100 {
		t.Fatalf("continuation lost the actual spend day: %+v %v", got, err)
	}
}

func TestStage2RestartReconcilesConfirmedAction(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(pgtest.Dir(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.Create("alice", "alpha", Options{Mode: "edit"})
	if err != nil {
		t.Fatal(err)
	}
	err = s.Update("alice", "alpha", c.ID, func(c *Conversation) error {
		c.Runs = []Run{{ID: "approval-run", State: "awaiting_confirmation", Usage: Accounting{Reserved: 100}, Operations: []Operation{{ID: "approved", Name: "trash_notes", State: "running", Approved: true}}}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
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
	if Active(got.Runs[0].State) {
		t.Fatalf("confirmed operation has no remaining confirmation token but restart leaves user blocked in %q; operation=%+v", got.Runs[0].State, got.Runs[0].Operations[0])
	}
}
