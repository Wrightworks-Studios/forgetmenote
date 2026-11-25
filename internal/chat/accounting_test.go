package chat

import (
	"forgetmenote/internal/pg/pgtest"
	"testing"
	"time"
)

func TestRequestUsageKeepsItsDayAcrossMidnight(t *testing.T) {
	s, err := OpenStore(pgtest.Org(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c, _ := s.Create("alice", "alpha", Options{Connection: "a"})
	yesterday := time.Now().UTC().Add(-24 * time.Hour).Format("2006-01-02")
	today := Day()
	err = s.Update("alice", "alpha", c.ID, func(c *Conversation) error {
		c.Runs = append(c.Runs, Run{ID: "cross-midnight", State: "running", CreatedAt: yesterday + "T23:59:59Z", Options: Options{Connection: "a", BudgetTokens: 1000}, InFlightDay: yesterday, InFlightTokens: 100, Usage: Accounting{Reserved: 1000}})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SettleUsage("alice", "alpha", c.ID, "cross-midnight", Usage{InputTokens: 50, OutputTokens: 10}, 0); err != nil {
		t.Fatal(err)
	}
	err = s.Update("alice", "alpha", c.ID, func(c *Conversation) error {
		r := c.Run("cross-midnight")
		r.InFlightTokens = 20
		r.InFlightDay = today
		r.ReservationDay = today
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SettleUsage("alice", "alpha", c.ID, "cross-midnight", Usage{InputTokens: 5, OutputTokens: 3}, 0); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Read("alice", "alpha", c.ID)
	run := got.Runs[0]
	if run.UsageDays[yesterday].Tokens() != 60 || run.UsageDays[today].Tokens() != 8 || run.Usage.Tokens() != 68 || run.Usage.Requests != 2 {
		t.Fatalf("request day attribution changed history: %+v", run)
	}
}
func TestHistoricalUsageNeverBlocksNewWorkOrContinuation(t *testing.T) {
	for _, connection := range []string{"a", "b"} {
		t.Run(connection, func(t *testing.T) {
			s, err := OpenStore(pgtest.Org(t))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			c, err := s.Create("alice", "alpha", Options{Connection: connection})
			if err != nil {
				t.Fatal(err)
			}
			yesterday := time.Now().UTC().Add(-24 * time.Hour).Format("2006-01-02")
			err = s.Update("alice", "alpha", c.ID, func(c *Conversation) error {
				c.Runs = append(c.Runs, Run{ID: "old", State: "completed", CreatedAt: yesterday + "T23:59:59Z", Options: Options{Connection: "a"}, Usage: Accounting{InputTokens: 9000000, UncertainTokens: 7000000, Estimated: true}, UsageDays: map[string]Accounting{yesterday: {InputTokens: 8000000}, Day(): {InputTokens: 1000000, UncertainTokens: 7000000, Estimated: true}}})
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			run := Run{ID: "new", SubmissionID: "new", RequestHash: "hash", State: "queued", CreatedAt: Now(), Options: Options{Connection: connection, Corpus: testCorpus()}, Usage: Accounting{Reserved: 1}}
			if _, _, err = s.Start("alice", "alpha", c.ID, run, Turn{ID: "user", Role: "user", Content: "Continue reading"}); err != nil {
				t.Fatalf("reported or uncertain usage blocked %s: %v", connection, err)
			}
			if err = s.Update("alice", "alpha", c.ID, func(c *Conversation) error {
				r := c.Run("new")
				r.State = "interrupted"
				r.InFlightDay = Day()
				r.Usage.InputTokens = 3000000
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if _, err = s.Resume("alice", "alpha", c.ID, "new", 8, 16, 0); err != nil {
				t.Fatalf("usage blocked continuation: %v", err)
			}
			got, err := s.Read("alice", "alpha", c.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Run("old").UsageDays[yesterday].Tokens() != 8000000 || got.Run("old").UsageDays[Day()].Tokens() != 8000000 || got.Run("new").Usage.Tokens() != 3000000 {
				t.Fatal("new work rewrote historical usage")
			}
		})
	}
}
