package chat

import (
	"encoding/json"
	"errors"
	"forgetmenote/internal/pg/pgtest"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDeleteScrubsPrivateRecordRetainsBudgetsAndRecovers(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(pgtest.Dir(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	s.SetLimits(Limits{MaxConversations: 1}, nil)
	c, err := s.Create("alice", "alpha", Options{Connection: "local", Mode: "ask"})
	if err != nil {
		t.Fatal(err)
	}
	private := "SENSITIVE-DELETE-FIXTURE"
	native := Message{Role: "assistant", Content: private, Continuation: json.RawMessage(`{"opaque":"` + private + `"}`), ToolCalls: []ToolCall{{ID: private, Name: private, Arguments: json.RawMessage(`{"content":"` + private + `"}`)}}}
	citation := Citation{ID: private, Path: private, Title: private, Text: private, URL: private}
	if err = s.Update("alice", "alpha", c.ID, func(c *Conversation) error {
		c.Title = private
		c.Error = private
		c.Memory = Memory{Text: private, Revisions: map[string]string{private: private}}
		c.Turns = []Turn{{ID: "turn", Role: "user", Content: private, Attachments: []Attachment{{Kind: "private", Path: private, Content: private}}, Citations: []Citation{citation}, Provider: &native}}
		c.Runs = []Run{{ID: "run", State: "completed", CreatedAt: Now(), Options: Options{Connection: "local", Model: private}, Usage: Accounting{InputTokens: 70, OutputTokens: 20, UncertainTokens: 10, Requests: 2}, Messages: []Message{native}, Citations: []Citation{citation}, Operations: []Operation{{ID: private, Arguments: json.RawMessage(`{"private":"` + private + `"}`), Result: json.RawMessage(`{"private":"` + private + `"}`)}}, Review: &Coverage{Purpose: private, Notes: []ReviewNote{{Summary: private}}}}}
		c.AddEvent("run", "text_delta", map[string]string{"text": private})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before, err := s.PersonalUsage("alice")
	if err != nil {
		t.Fatal(err)
	}
	storeRecovery(t, s, "alice", c.ID, []byte(`{"hash":"old","runs":["`+private+`"]}`))
	for _, pair := range [][2]string{{"bob", "alpha"}, {"alice", "beta"}} {
		if _, err = s.Delete(pair[0], pair[1], c.ID, nil); !errors.Is(err, ErrNotFound) {
			t.Fatalf("cross scope delete: %v", err)
		}
	}
	if _, err = s.Delete("alice", "alpha", c.ID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Delete("alice", "alpha", c.ID, nil); err != nil {
		t.Fatalf("retry: %v", err)
	}
	assertDeleted := func() {
		t.Helper()
		if _, err = s.Read("alice", "alpha", c.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("deleted read: %v", err)
		}
		all, e := s.List("alice", "")
		if e != nil || len(all) != 0 {
			t.Fatalf("receipt listed: %v %v", all, e)
		}
		after, e := s.PersonalUsage("alice")
		if e != nil || !reflect.DeepEqual(before, after) {
			t.Fatalf("usage changed: %+v %+v %v", before, after, e)
		}
		if strings.Contains(storedChat(t, s), private) {
			t.Error("retained private payload in stored chat")
		}
	}
	assertDeleted()
	if err = s.Update("alice", "alpha", c.ID, func(*Conversation) error { t.Error("late updater ran"); return nil }); !errors.Is(err, ErrNotFound) {
		t.Fatalf("late update: %v", err)
	}
	if err = s.SettleUsage("alice", "alpha", c.ID, "run", Usage{}, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("late provider: %v", err)
	}
	if _, err = s.Resume("alice", "alpha", c.ID, "run", 1, 1, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted resume: %v", err)
	}
	// A crash after replacing the record but before dropping its recovery
	// overlay must finish scrubbing on startup without exposing old content.
	storeRecovery(t, s, "alice", c.ID, []byte(`{"hash":"old","runs":["`+private+`"]}`))
	s.Close()
	s, err = OpenStore(pgtest.Dir(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	s.SetLimits(Limits{MaxConversations: 1}, nil)
	assertDeleted()
	next, err := s.Create("alice", "alpha", Options{Connection: "local", Mode: "ask"})
	if err != nil {
		t.Fatalf("deleted conversation still occupies capacity: %v", err)
	}
	if _, err = s.Create("alice", "alpha", Options{Mode: "ask"}); !errors.Is(err, ErrCapacity) {
		t.Fatalf("live count bypass: %v", err)
	}
	run := Run{ID: "new-run", SubmissionID: "new", RequestHash: "hash", State: "queued", Options: Options{Connection: "local", Corpus: testCorpus()}, Usage: Accounting{Reserved: 1}}
	if _, _, err = s.Start("alice", "alpha", next.ID, run, Turn{}); err != nil {
		t.Fatalf("deleted accounting blocked new work: %v", err)
	}
	if err = s.Update("alice", "alpha", next.ID, func(c *Conversation) error {
		c.Runs = []Run{{ID: "resume", State: "cancelled", Options: Options{Connection: "local", Corpus: testCorpus()}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Resume("alice", "alpha", next.ID, "resume", 1, 1, 0); err != nil {
		t.Fatalf("deleted accounting blocked continuation: %v", err)
	}
	if _, err = s.Create("alice", "deleted:alpha", Options{Mode: "ask"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("reserved namespace writable: %v", err)
	}
}

func TestDeleteRequiresSettledInactiveRun(t *testing.T) {
	for _, run := range []Run{{State: "queued"}, {State: "running"}, {State: "awaiting_confirmation"}, {State: "cancelled", InFlightTokens: 10}} {
		t.Run(run.State, func(t *testing.T) {
			s, err := OpenStore(pgtest.Org(t))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			c, err := s.Create("alice", "alpha", Options{Mode: "ask"})
			if err != nil {
				t.Fatal(err)
			}
			if err = s.Update("alice", "alpha", c.ID, func(c *Conversation) error {
				c.Runs = []Run{run}
				c.Turns = []Turn{{Content: "retain until stopped"}}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			before := storedRecord(t, s, "alice", c.ID)
			if _, err = s.Delete("alice", "alpha", c.ID, nil); !errors.Is(err, ErrDeleteActive) {
				t.Fatalf("active deletion: %v", err)
			}
			if after := storedRecord(t, s, "alice", c.ID); string(before) != string(after) {
				t.Fatal("rejected deletion changed record")
			}
		})
	}
}

func TestDeleteRacesCannotResurrectConversation(t *testing.T) {
	s, err := OpenStore(pgtest.Org(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c, err := s.Create("alice", "alpha", Options{Mode: "ask"})
	if err != nil {
		t.Fatal(err)
	}
	gate := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-gate
			err := s.Update("alice", "alpha", c.ID, func(c *Conversation) error { c.Turns = append(c.Turns, Turn{Content: "racing payload"}); return nil })
			if err != nil && !errors.Is(err, ErrNotFound) {
				t.Error(err)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-gate
		if _, err := s.Delete("alice", "alpha", c.ID, nil); err != nil {
			t.Error(err)
		}
	}()
	close(gate)
	wg.Wait()
	if _, err = s.Read("alice", "alpha", c.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("resurrected: %v", err)
	}
	if raw := storedRecord(t, s, "alice", c.ID); strings.Contains(string(raw), "racing payload") {
		t.Fatal("late payload retained")
	}
}

func TestDamagedDeletionReceiptReportsUnknownAccounting(t *testing.T) {
	s, err := OpenStore(pgtest.Org(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c, err := s.Create("alice", "alpha", Options{Mode: "ask"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Delete("alice", "alpha", c.ID, nil); err != nil {
		t.Fatal(err)
	}
	raw := storedRecord(t, s, "alice", c.ID)
	raw = append([]byte(`{"unknownDurableField":true,`), raw[1:]...)
	storeRecord(t, s, "alice", c.ID, raw)
	usage, err := s.PersonalUsage("alice")
	if err != nil || usage.UnavailableConversations != 1 {
		t.Fatalf("corrupt receipt silently lost: %+v %v", usage, err)
	}
	if _, err = s.Delete("alice", "alpha", c.ID, nil); err == nil {
		t.Fatal("corrupt receipt acknowledged")
	}
	next, err := s.Create("alice", "alpha", Options{Mode: "ask"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.Start("alice", "alpha", next.ID, Run{ID: "new", SubmissionID: "new", State: "queued", Options: Options{Corpus: testCorpus()}}, Turn{}); err != nil {
		t.Fatalf("unknown accounting blocked unrelated work: %v", err)
	}
}

func TestDeleteRechecksAuthorizationAfterStoreWait(t *testing.T) {
	s, err := OpenStore(pgtest.Org(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c, err := s.Create("alice", "alpha", Options{Mode: "ask"})
	if err != nil {
		t.Fatal(err)
	}
	held, unblock, updated := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	var once sync.Once
	release := func() { once.Do(func() { close(unblock) }) }
	defer release()
	go func() {
		updated <- s.Update("alice", "alpha", c.ID, func(c *Conversation) error {
			c.Turns = []Turn{{Content: "preserve after queued revocation"}}
			close(held)
			<-unblock
			return nil
		})
	}()
	<-held
	var allowed atomic.Bool
	allowed.Store(true)
	denied := errors.New("caller authority revoked")
	ended := make(chan error, 1)
	go func() {
		_, err := s.Delete("alice", "alpha", c.ID, func() error {
			if !allowed.Load() {
				return denied
			}
			return nil
		})
		ended <- err
	}()
	queued := false
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		stack := make([]byte, 1<<20)
		n := runtime.Stack(stack, true)
		for _, routine := range strings.Split(string(stack[:n]), "\n\n") {
			if strings.Contains(routine, "(*Store).Delete") {
				queued = true
				break
			}
		}
		if queued {
			break
		}
		time.Sleep(time.Millisecond)
	}
	allowed.Store(false)
	release()
	if !queued {
		t.Error("Delete did not reach the held storage mutex")
	}
	if err := <-updated; err != nil {
		t.Fatal(err)
	}
	if err := <-ended; !errors.Is(err, denied) {
		t.Fatalf("stale authorization deleted content: %v", err)
	}
	saved, err := s.Read("alice", "alpha", c.ID)
	if err != nil || len(saved.Turns) != 1 {
		t.Fatalf("revoked delete changed transcript: %+v %v", saved, err)
	}
	if retired, err := s.Delete("alice", "other", c.ID, nil); retired || !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong notebook retired current chat: %v %v", retired, err)
	}
	if retired, err := s.Delete("alice", "alpha", c.ID, nil); !retired || err != nil {
		t.Fatal(retired, err)
	}
	if retired, err := s.Delete("alice", "other", c.ID, nil); retired || !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong notebook matched receipt: %v %v", retired, err)
	}
	if retired, err := s.Delete("alice", "alpha", c.ID, func() error { return denied }); retired || !errors.Is(err, denied) {
		t.Fatalf("revoked retry reached receipt cleanup: %v %v", retired, err)
	}
}

func TestDeleteIgnoresLegacyUnusedTokenReservation(t *testing.T) {
	s, err := OpenStore(pgtest.Org(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c, err := s.Create("alice", "alpha", Options{Mode: "ask"})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Update("alice", "alpha", c.ID, func(c *Conversation) error {
		c.Runs = append(c.Runs, Run{ID: "legacy", State: "failed", Options: Options{BudgetTokens: 1000}, Usage: Accounting{InputTokens: 70, UncertainTokens: 30, Reserved: 900, Estimated: true, Requests: 1}})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Delete("alice", "alpha", c.ID, nil); err != nil {
		t.Fatalf("obsolete reservation blocked deletion: %v", err)
	}
	usage, err := s.PersonalUsage("alice")
	if err != nil || usage.Total.Tokens() != 100 || usage.Total.Requests != 1 || usage.Total.Reserved != 0 {
		t.Fatalf("deletion changed usage or retained a spending reservation: %+v %v", usage, err)
	}
}
