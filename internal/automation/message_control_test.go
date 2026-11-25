package automation

import (
	"context"
	"forgetmenote/internal/pg/pgtest"
	"testing"

	"forgetmenote/internal/messaging"
)

func TestSlackStopFencesQueuedAndDelayedMessagesAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	q, err := Open(pgtest.Dir(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	enqueue := func(message, thread, text string) Run {
		t.Helper()
		spec := Spec{ID: "chat-" + Hash(message)[:32], Kind: "assistant", Actor: "alice", Project: "default", AIConnection: "local", Model: "synthetic", ExternalProcessing: true, Instruction: text, Delivery: &messaging.Address{Client: "slack", Workspace: "T123", Channel: "C123", Thread: thread, User: "U123", Message: message}}
		r, err := q.Enqueue(spec, message)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	old := enqueue("9.999999", "thread", "Original request")
	other := enqueue("9.999998", "other", "Another thread")
	stop := enqueue("10.000001", "thread", "<@UBOT> Please stop responding to this thread.")
	for _, id := range []string{old.ID, stop.ID} {
		r, err := q.Get(id)
		if err != nil || r.State != "cancelled" {
			t.Fatalf("stop did not cancel %s: %s %v", id, r.State, err)
		}
	}
	if r, _ := q.Get(other.ID); r.State != "queued" {
		t.Fatal("stopped an unrelated thread")
	}
	q, err = Open(pgtest.Dir(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	delayed := enqueue("10.000000", "thread", "Late delivery of an old request")
	if delayed.State != "cancelled" {
		t.Fatal("delayed request escaped durable stop", delayed.State)
	}
	resumed := enqueue("10.000002", "thread", "New explicit request")
	if resumed.State != "queued" {
		t.Fatal("explicit invocation did not resume")
	}
	enqueue("10.000001", "thread", "<@UBOT> Please stop responding to this thread.")
	if r, _ := q.Get(resumed.ID); r.State != "queued" {
		t.Fatal("stop retry cancelled a newer request")
	}
	called := map[string]bool{}
	for {
		ran, err := q.RunNext(context.Background(), func(_ context.Context, r *Run, _ func() error) error { called[r.ID] = true; return nil })
		if err != nil {
			t.Fatal(err)
		}
		if !ran {
			break
		}
	}
	if len(called) != 2 || !called[other.ID] || !called[resumed.ID] {
		t.Fatal("wrong requests executed", called)
	}
}

func TestSlackStopRespectsConversationAudience(t *testing.T) {
	for _, tc := range []struct {
		name                                    string
		shared                                  bool
		otherUser, otherChannel, otherWorkspace bool
		cancelled                               bool
	}{
		{name: "same private requester", cancelled: true},
		{name: "other private requester", otherUser: true},
		{name: "shared coworker", shared: true, otherUser: true, cancelled: true},
		{name: "other channel", shared: true, otherChannel: true},
		{name: "other workspace", shared: true, otherWorkspace: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q, err := Open(pgtest.Org(t))
			if err != nil {
				t.Fatal(err)
			}
			d := messaging.Address{Client: "slack", Workspace: "T123", Channel: "C123", Thread: "1.000001", User: "U123", Message: "1.000001", Shared: tc.shared}
			spec := Spec{ID: "first", Kind: "assistant", Actor: "alice", Project: "default", AIConnection: "local", Model: "synthetic", Instruction: "Analyze", ExternalProcessing: true, Delivery: &d}
			first, err := q.Enqueue(spec, "first")
			if err != nil {
				t.Fatal(err)
			}
			stop := d
			stop.Message = "2.000001"
			if tc.otherUser {
				stop.User = "U456"
			}
			if tc.otherChannel {
				stop.Channel = "C456"
			}
			if tc.otherWorkspace {
				stop.Workspace = "T456"
			}
			spec.ID = "stop"
			spec.Instruction = "stop"
			spec.Delivery = &stop
			if _, err := q.Enqueue(spec, "stop"); err != nil {
				t.Fatal(err)
			}
			got, err := q.Get(first.ID)
			if err != nil {
				t.Fatal(err)
			}
			if (got.State == "cancelled") != tc.cancelled {
				t.Fatalf("state %s, cancelled wanted %v", got.State, tc.cancelled)
			}
		})
	}
}

func TestSlackStopPreviewCannotCancelLiveWork(t *testing.T) {
	q, err := Open(pgtest.Org(t))
	if err != nil {
		t.Fatal(err)
	}
	d := messaging.Address{Client: "slack", Workspace: "T123", Channel: "C123", Thread: "1.000001", User: "U123", Message: "1.000001"}
	spec := Spec{ID: "live", Kind: "assistant", Actor: "alice", Project: "default", AIConnection: "local", Model: "synthetic", Instruction: "Analyze", ExternalProcessing: true, Delivery: &d}
	live, err := q.Enqueue(spec, "live")
	if err != nil {
		t.Fatal(err)
	}
	stop := d
	stop.Message = "2.000001"
	spec.ID = "preview"
	spec.Instruction = "stop"
	spec.Delivery = &stop
	preview, err := q.EnqueuePreview(spec, "preview")
	if err != nil {
		t.Fatal(err)
	}
	got, err := q.Get(live.ID)
	if err != nil || got.State != "queued" {
		t.Fatalf("preview cancelled live work: %s %v", got.State, err)
	}
	stop.Message = "3.000001"
	spec.ID = "live-stop"
	if _, err = q.List(); err != nil {
		t.Fatal(err)
	}
	if _, err = q.Enqueue(spec, "live-stop"); err != nil {
		t.Fatal(err)
	}
	got, err = q.Get(preview.ID)
	if err != nil || got.State != "queued" {
		t.Fatalf("live stop cancelled an isolated preview: %s %v", got.State, err)
	}
}
