package chat

import (
	"encoding/json"
	"errors"
	"forgetmenote/internal/pg/pgtest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestConfiguredConversationCountIsAtomic(t *testing.T) {
	s, err := OpenStore(pgtest.Org(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.SetLimits(Limits{MaxConversations: 3}, nil)
	var accepted atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, e := s.Create("alice", "alpha", Options{})
			if e == nil {
				accepted.Add(1)
			} else if !errors.Is(e, ErrCapacity) {
				t.Error(e)
			}
		}()
	}
	wg.Wait()
	if accepted.Load() != 3 {
		t.Fatalf("concurrent admission exceeded cap: %d", accepted.Load())
	}
}
func TestConfiguredByteLimitLeavesExistingRecordIntact(t *testing.T) {
	s, err := OpenStore(pgtest.Org(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.SetLimits(Limits{MaxRecordBytes: 4096, MaxEvents: 24}, nil)
	c, err := s.Create("alice", "alpha", Options{})
	if err != nil {
		t.Fatal(err)
	}
	before := storedRecord(t, s, "alice", c.ID)
	err = s.Update("alice", "alpha", c.ID, func(c *Conversation) error {
		c.Turns = append(c.Turns, Turn{ID: "oversized", Role: "user", Content: strings.Repeat("x", 4096)})
		return nil
	})
	after := storedRecord(t, s, "alice", c.ID)
	if !errors.Is(err, ErrCapacity) || string(before) != string(after) {
		t.Fatal("rejected byte-limit write changed original record")
	}
}
func TestCorruptSiblingReportsUnknownUsageWithoutBlockingHealthyWork(t *testing.T) {
	s, err := OpenStore(pgtest.Org(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	healthy, _ := s.Create("alice", "alpha", Options{})
	bad, _ := s.Create("alice", "alpha", Options{})
	storeRecord(t, s, "alice", bad.ID, []byte(`{"corrupt":true}`))
	run := Run{ID: NewID(), SubmissionID: "paid", State: "queued", RequestHash: "hash", CreatedAt: Now(), Options: Options{Corpus: testCorpus()}}
	_, _, err = s.Start("alice", "alpha", healthy.ID, run, Turn{ID: "turn", Role: "user", Content: "hello"})
	if err != nil {
		t.Fatalf("unknown usage blocked unrelated work: %v", err)
	}
	usage, err := s.PersonalUsage("alice")
	if err != nil || usage.UnavailableConversations != 1 {
		t.Fatalf("damaged accounting no longer reported: %+v %v", usage, err)
	}
	if _, err = s.Read("alice", "alpha", healthy.ID); err != nil {
		t.Fatal(err)
	}
}
func TestLegacyByteFullRecordRestartsWithoutDiscardingTranscript(t *testing.T) {
	root := t.TempDir()
	s, err := OpenStore(pgtest.Dir(t, root))
	if err != nil {
		t.Fatal(err)
	}
	c, _ := s.Create("alice", "alpha", Options{})
	c.Runs = append(c.Runs, Run{ID: "pending", State: "running", InFlightTokens: 80, Usage: Accounting{Reserved: 100}})
	c.Turns = append(c.Turns, Turn{ID: "full", Role: "user", Content: ""})
	base, _ := json.Marshal(c)
	c.Turns[0].Content = strings.Repeat("x", maxRecordBytes-len(base))
	raw, _ := json.Marshal(c)
	if len(raw) != maxRecordBytes {
		t.Fatal("wrong byte-limit fixture")
	}
	storeRecord(t, s, "alice", c.ID, raw)
	s.Close()
	recovered, err := OpenStore(pgtest.Dir(t, root))
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	got, err := recovered.Read("alice", "alpha", c.ID)
	if err != nil || got.Runs[0].State != "interrupted" || got.Runs[0].Usage.UncertainTokens != 80 {
		t.Fatalf("full byte record recovery failed: %v", err)
	}
	after := storedRecord(t, recovered, "alice", c.ID)
	if string(after) != string(raw) {
		t.Fatal("legacy full original bytes were discarded")
	}
}
