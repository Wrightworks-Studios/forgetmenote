package citationchecks

import (
	"context"
	"errors"
	"forgetmenote/internal/pg"
	"forgetmenote/internal/pg/pgtest"
	"testing"
	"time"

	"forgetmenote/internal/decision"
)

func TestRuntimeOwnsPendingPublicationCancellationAndLateSave(t *testing.T) {
	records, err := decision.OpenRecords(pgtest.Org(t))
	if err != nil {
		t.Fatal(err)
	}
	r := New(context.Background(), records)
	defer r.Close()
	started := make(chan string, 2)
	finished := make(chan struct{}, 2)
	execute := func(ctx context.Context, id string) { started <- id; <-ctx.Done(); finished <- struct{}{} }
	if err = r.Start("user", "book", "first", "run", true, execute); err != nil {
		t.Fatal(err)
	}
	first := <-started
	if err = r.Start("user", "book", "second", "run", true, execute); err != nil {
		t.Fatal(err)
	}
	<-started
	if err = r.Start("user", "book", "queued", "run", true, func(context.Context, string) { t.Error("retired queued task ran") }); err != nil {
		t.Fatal(err)
	}
	record, active, err := r.Snapshot("user", "book", "queued", "run")
	if err != nil || !active || record.Checks["run"].State != "pending" {
		t.Fatal(record, active, err)
	}
	if err = r.ForgetConversation("user", "queued"); err != nil {
		t.Fatal(err)
	}
	if err = r.ForgetConversation("user", "first"); err != nil {
		t.Fatal(err)
	}
	<-finished
	if err = r.Save("user", "book", "first", "run", Check{ID: first, State: "completed"}); !errors.Is(err, context.Canceled) {
		t.Fatal("late publication survived retirement", err)
	}
	r.Close()
	<-finished
	if err = r.Start("user", "book", "later", "run", true, execute); !errors.Is(err, decision.ErrDisabled) {
		t.Fatal("closed runtime admitted work", err)
	}
}

// Two gateways each start a check of the same conversation while writes stall,
// first creating its record and then changing it: every check is recorded.
func TestChecksStartedOnTwoGatewaysAreAllRecorded(t *testing.T) {
	db := pgtest.Org(t)
	other, _ := pgtest.Gateway(t, db)
	runtimes := [2]*Runtime{}
	for i, gateway := range []*pg.DB{db, other} {
		records, err := decision.OpenRecords(gateway)
		if err != nil {
			t.Fatal(err)
		}
		runtimes[i] = New(context.Background(), records)
		defer runtimes[i].Close()
	}
	for _, runs := range [][2]string{{"first", "second"}, {"third", "fourth"}} {
		waiting, release := pgtest.HoldWrites(t, db, "jev_records")
		started := make(chan error, 2)
		for i, run := range runs {
			go func() {
				started <- runtimes[i].Start("user", "book", "chat", run, true, func(context.Context, string) {})
			}()
			waiting(i + 1)
		}
		release()
		for range runs {
			if err := <-started; err != nil {
				t.Fatal(err)
			}
		}
		record, _, err := runtimes[0].Snapshot("user", "book", "chat", "")
		if err != nil {
			t.Fatal(err)
		}
		for _, run := range runs {
			if _, ok := record.Checks[run]; !ok {
				t.Fatalf("the check of %s started on one gateway was lost to the other's: %v", run, record.Checks)
			}
		}
	}
}

// A conversation forgotten through one gateway while another saves its check
// stays forgotten.
func TestForgottenConversationStaysForgottenDuringAnotherGatewaysSave(t *testing.T) {
	db := pgtest.Org(t)
	other, _ := pgtest.Gateway(t, db)
	records, err := decision.OpenRecords(db)
	if err != nil {
		t.Fatal(err)
	}
	r := New(context.Background(), records)
	defer r.Close()
	otherRecords, err := decision.OpenRecords(other)
	if err != nil {
		t.Fatal(err)
	}
	forgetting := New(context.Background(), otherRecords)
	defer forgetting.Close()
	started := make(chan string, 1)
	if err = r.Start("user", "book", "chat", "run", true, func(_ context.Context, id string) { started <- id }); err != nil {
		t.Fatal(err)
	}
	id := <-started
	waiting, release := pgtest.HoldWrites(t, db, "jev_records")
	saved := make(chan error, 1)
	go func() {
		saved <- r.Save("user", "book", "chat", "run", Check{ID: id, State: "completed", Items: []Item{}})
	}()
	waiting(1)
	forgot := make(chan error, 1)
	go func() { forgot <- forgetting.ForgetConversation("user", "chat") }()
	// The removal either completes at once or waits for the save.
	for deadline := time.Now().Add(10 * time.Second); len(forgot) == 0 && pgtest.LockWaiters(t, db) < 2; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the removal neither finished nor waited")
		}
	}
	release()
	if err = <-forgot; err != nil {
		t.Fatal(err)
	}
	if err = <-saved; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if record, _, err := r.Snapshot("user", "book", "chat", "run"); err != nil || len(record.Checks) != 0 {
		t.Fatal("a check saved on one gateway brought back a conversation another forgot", record.Checks, err)
	}
}

func TestRuntimePublishesBeforeAdmissionAndFailureQueuesNothing(t *testing.T) {
	records, err := decision.OpenRecords(pgtest.Org(t))
	if err != nil {
		t.Fatal(err)
	}
	r := New(context.Background(), records)
	defer r.Close()
	done := make(chan error, 1)
	if err = r.Start("user", "book", "chat", "run", true, func(_ context.Context, id string) {
		record, _, err := r.Snapshot("user", "book", "chat", "run")
		if err == nil && record.Checks["run"].ID != id {
			err = errors.New("callback preceded pending receipt")
		}
		done <- err
	}); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	r.Close()
	records.Bytes = 1
	r = New(context.Background(), records)
	defer r.Close()
	if err = r.Start("other", "book", "chat", "run", true, func(context.Context, string) { t.Error("failed publication admitted work") }); !errors.Is(err, decision.ErrStorage) {
		t.Fatal(err)
	}
	if r.Active() != 0 {
		t.Fatal("failed publication retained worker")
	}
}
