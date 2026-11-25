package chat

import (
	"forgetmenote/internal/pg/pgtest"
	"testing"
	"time"
)

func TestPriorDayResumeRetainsUsageWithoutQuota(t *testing.T) {
	s, err := OpenStore(pgtest.Org(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c, err := s.Create("alice", "alpha", Options{Mode: "ask"})
	if err != nil {
		t.Fatal(err)
	}
	// A real interrupted run retained overnight. It has no usage before today's resumes.
	err = s.Update("alice", "alpha", c.ID, func(c *Conversation) error {
		c.Runs = append(c.Runs, Run{ID: "old-run", State: "interrupted", CreatedAt: time.Now().UTC().Add(-24 * time.Hour).Format(time.RFC3339Nano), Options: Options{Mode: "ask", Corpus: testCorpus()}})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Today's first explicit continuation reports 10,000 tokens.
	if _, err = s.Resume("alice", "alpha", c.ID, "old-run", 8, 16, 0); err != nil {
		t.Fatal(err)
	}
	err = s.Update("alice", "alpha", c.ID, func(c *Conversation) error {
		r := c.Run("old-run")
		r.Usage.InputTokens += 10000
		r.Usage.Reserved = 0
		r.State = "interrupted"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Prior and current usage remain accounting, never an admission ceiling.
	if resumed, err := s.Resume("alice", "alpha", c.ID, "old-run", 8, 16, 0); err != nil || resumed.Run("old-run").Usage.Tokens() != 10000 {
		t.Fatalf("continuation blocked or lost usage: %+v %v", resumed, err)
	}
}
