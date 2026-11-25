package chat

import (
	"encoding/json"
	"forgetmenote/internal/pg/pgtest"
	"testing"
)

func TestReviewCapacityDoesNotPreventRestart(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(pgtest.Dir(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.Create("alice", "alpha", Options{Mode: "ask"})
	if err != nil {
		t.Fatal(err)
	}
	err = s.Update("alice", "alpha", c.ID, func(c *Conversation) error {
		c.Runs = append(c.Runs, Run{ID: "running", State: "running"})
		for i := 0; i < 20000; i++ {
			c.AddEvent("running", "progress", map[string]int{"n": i})
		}
		return nil
	})
	if err != nil {
		if err != ErrCapacity {
			t.Fatal(err)
		}
		// New writes reserve recovery headroom. Recreate an already-existing
		// full record to verify compatibility recovery preserves its transcript.
		c.Runs = append(c.Runs, Run{ID: "running", State: "running"})
		for i := 0; i < 20000; i++ {
			c.AddEvent("running", "progress", map[string]int{"n": i})
		}
		raw, _ := json.Marshal(c)
		storeRecord(t, s, "alice", c.ID, raw)
	}
	s.Close()
	restored, err := OpenStore(pgtest.Dir(t, dir))
	if err != nil {
		t.Fatalf("a valid capacity-bound conversation prevented the entire chat store from restarting: %v", err)
	}
	defer restored.Close()
	got, err := restored.Read("alice", "alpha", c.ID)
	if err != nil || got.Runs[0].State != "interrupted" {
		t.Fatalf("recovery = %+v, %v", got, err)
	}
}

func TestReviewCorruptConversationDoesNotBlockHealthySibling(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(pgtest.Dir(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	healthy, err := s.Create("alice", "alpha", Options{Mode: "ask"})
	if err != nil {
		t.Fatal(err)
	}
	damaged, err := s.Create("alice", "alpha", Options{Mode: "ask"})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(damaged)
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	if err = json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	record["events"] = []any{map[string]any{"sequence": 99, "run": "r", "type": "state", "data": map[string]string{"state": "completed"}}}
	raw, err = json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	storeRecord(t, s, "alice", damaged.ID, raw)
	if _, err = s.Read("alice", "alpha", healthy.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.List("alice", "alpha")
	if err != nil {
		t.Fatalf("one damaged conversation prevents listing an independently readable healthy sibling: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("healthy conversation omitted")
	}
}
