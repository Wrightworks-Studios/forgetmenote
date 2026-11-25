package workspace

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Hints coalesce, come only after a whole revision is readable, never for a
// failed commit, and stop when their subscription is cancelled.
func TestCommittedHintsCoalesceAndGroupsStayCoherent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	hints, cancel, err := s.Watch()
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	// One revision of two notes, committed by another writer.
	pair := commitFiles(t, s, map[string]string{"one.md": "One", "two.md": "Two"})
	select {
	case <-hints:
	case <-time.After(10 * time.Second):
		t.Fatal("missing hint")
	}
	for _, path := range []string{"one.md", "two.md"} {
		if d, err := s.Read(ctx, path, ""); err != nil || d.Revision != pair {
			t.Fatal("partial group notification", path, err)
		}
	}
	if _, err = s.Save(ctx, "one.md", []byte("bad"), "", ""); err == nil {
		t.Fatal("expected conflict")
	}
	select {
	case <-hints:
		t.Fatal("failed commit hint")
	default:
	}
	for i := 0; i < 5; i++ {
		d, _ := s.Read(ctx, "one.md", "")
		if _, err = s.Save(ctx, "one.md", []byte(strings.Repeat("a", i+1)), d.Revision, ""); err != nil {
			t.Fatal(err)
		}
	}
	if len(hints) != 1 {
		t.Fatal("burst was not bounded", len(hints))
	}
	cancel()
	cancel()
	s.mu.Lock()
	n := len(s.watchers)
	s.mu.Unlock()
	if n != 0 {
		t.Fatal("subscription retained")
	}
}

func TestSubscriptionsAreBounded(t *testing.T) {
	s := newTestStore(t)
	for i := 0; i < 128; i++ {
		_, cancel, err := s.Watch()
		if err != nil {
			t.Fatal(err)
		}
		defer cancel()
	}
	if _, _, err := s.Watch(); err == nil {
		t.Fatal("subscriptions past the limit")
	}
}
