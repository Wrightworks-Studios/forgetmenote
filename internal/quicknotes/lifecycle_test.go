package quicknotes

import (
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/pg/pgtest"
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

func TestOpenDoesNotProcessConfiguredInboxBeforeStart(t *testing.T) {
	root := t.TempDir()
	c, err := catalog.Open(filepath.Join(root, "catalog"), nil, pgtest.Dir(t, filepath.Join(root, "catalog")))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	s, err := Open(pgtest.Dir(t, filepath.Join(root, "catalog")), c)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	routeTarget(t, c)
	called := make(chan struct{}, 8)
	s.ConfigureJudgments(testJudge(t, func(w http.ResponseWriter, r *http.Request) {
		called <- struct{}{}
		routeReply(w, r, "destination_000")
	}), false)
	settings, _ := s.Settings("alice")
	settings.CloudConsent = true
	if _, err := s.UpdateSettings("alice", settings); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Capture(t.Context(), "alice", "startup-pending", "# Planning\nA task for this week."); err != nil {
		t.Fatal(err)
	}
	select {
	case <-called:
		t.Fatal("Open or ConfigureJudgments processed content before explicit startup")
	case <-time.After(2 * time.Second):
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-called:
	case <-time.After(5 * time.Second):
		t.Fatal("explicit Start did not process the queued Inbox")
	}
}
