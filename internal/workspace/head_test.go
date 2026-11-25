package workspace

import (
	"context"
	"testing"
)

func TestRevisionHonoursCancelledRequest(t *testing.T) {
	s := newTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Status(ctx); err == nil {
		t.Fatal("cancelled request read the revision")
	}
	if _, err := s.Read(ctx, "a.md", ""); err == nil {
		t.Fatal("cancelled request read a note")
	}
}
