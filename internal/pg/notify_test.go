package pg_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"forgetmenote/internal/pg"
	"forgetmenote/internal/pg/pgtest"
)

// An announcement reaches the listeners of its org only, and a listener that
// lost its connection listens again and is told it may have missed some.
func TestAnnouncementsReachTheOrgsListenersAcrossReconnects(t *testing.T) {
	const channel = "test_announcements"
	db, elsewhere := pgtest.Org(t), pgtest.Org(t)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	listen := func(db *pg.DB) chan string {
		heard := make(chan string, 16)
		wg.Add(1)
		go func() { defer wg.Done(); db.Listen(ctx, channel, func(s string) { heard <- s }) }()
		return heard
	}
	heard, other := listen(db), listen(elsewhere)
	defer func() { cancel(); wg.Wait() }()
	next := func(heard chan string) string {
		t.Helper()
		select {
		case s := <-heard:
			return s
		case <-time.After(10 * time.Second):
			t.Fatal("nothing was heard")
			return ""
		}
	}
	pgtest.Listening(t, db, channel)
	pgtest.Listening(t, elsewhere, channel)
	if next(heard) != "" || next(other) != "" {
		t.Fatal("a new listener was not told to look at everything")
	}
	if err := db.Announce(ctx, channel, "alpha"); err != nil {
		t.Fatal(err)
	}
	if err := elsewhere.Announce(ctx, channel, "beta"); err != nil {
		t.Fatal(err)
	}
	if s := next(heard); s != "alpha" {
		t.Fatal("the org heard", s)
	}
	if s := next(other); s != "beta" {
		t.Fatal("another org heard", s)
	}
	if _, err := db.Pool.Exec(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE application_name = $1`, db.ListenerName()); err != nil {
		t.Fatal(err)
	}
	if s := next(heard); s != "" {
		t.Fatal("a listener that reconnected was not told it may have missed announcements", s)
	}
	pgtest.Listening(t, db, channel)
	if err := db.Announce(ctx, channel, "gamma"); err != nil {
		t.Fatal(err)
	}
	if s := next(heard); s != "gamma" {
		t.Fatal("the reconnected listener heard", s)
	}
}
