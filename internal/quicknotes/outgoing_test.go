package quicknotes

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"

	"forgetmenote/internal/outgoing"
)

// Routing captures in the background is paid Jev work, so a paused copy files
// nothing on its own; a person's explicit Sort still runs, as chat does.
func TestPausedRoutingLeavesCapturesInInbox(t *testing.T) {
	s, c, _ := fixture(t)
	routeTarget(t, c)
	var calls atomic.Int32
	judge := testJudge(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); routeReply(w, r, "destination_000") })
	s.Outgoing = &outgoing.Mode{}
	s.Outgoing.Pause()
	s.ConfigureJev(judge, false)
	available, _ := s.Destinations(context.Background(), "alice")
	settings, _ := s.Settings("alice")
	settings.Mode = "automatic"
	settings.CloudConsent = true
	settings.ApprovedDestinations = available
	if _, err := s.UpdateSettings("alice", settings); err != nil {
		t.Fatal(err)
	}
	saved, err := s.Capture(context.Background(), "alice", "paused-sort", "planning")
	if err != nil {
		t.Fatal(err)
	}
	// The routing duty wakes on every capture; this pass waits for any it began.
	s.scan(t.Context())
	account, _ := c.Account("alice")
	if r, err := s.readRecord(account.ID, saved.ID); err != nil || r.Status != "inbox" || calls.Load() != 0 {
		t.Fatalf("paused routing sent the capture: status=%s calls=%d %v", r.Status, calls.Load(), err)
	}
	if filed, err := s.Sort(context.Background(), "alice", saved.ID); err != nil || filed.Status != "filed" || calls.Load() != 1 {
		t.Fatalf("explicit Sort on a paused instance: %+v calls=%d %v", filed, calls.Load(), err)
	}
}
