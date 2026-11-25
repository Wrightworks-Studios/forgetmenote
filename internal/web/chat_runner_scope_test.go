package web

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"

	"forgetmenote/internal/chat"
)

// A run whose saved corpus cannot bind its scope fails with that reason, not
// as a stopped runner.
func TestChatRunReportsScopeFailure(t *testing.T) {
	var requests atomic.Int32
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected provider request", http.StatusInternalServerError)
	})
	cfg := app.chat.Snapshot().Config
	connection, err := cfg.Resolve("local", "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	// A source notebook saved without its binding: the corpus is valid data,
	// but the run's scope cannot be bound.
	corpus := &chat.Corpus{Version: 1, Archives: "exclude", Drafts: "exclude", SourceProjects: []string{"default"}}
	now := chat.Now()
	run := chat.Run{ID: chat.NewID(), SubmissionID: "unbound", RequestHash: "unbound", State: "queued", Options: chat.Options{Mode: "ask", Connection: "local", Model: "synthetic", Corpus: corpus}, Epoch: app.catalog.Epoch("alice"), CreatedAt: now, UpdatedAt: now}
	turn := chat.Turn{ID: chat.NewID(), RunID: run.ID, Role: "user", Content: "Use the unbound source."}
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := app.chat.Admit(cfg, connection, run.ID, cancel, "alice", nil, func() (bool, error) {
		_, replayed, err := app.chat.Store.Start("alice", "default", c.ID, run, turn)
		return replayed, err
	}); err != nil {
		t.Fatal(err)
	}
	app.executeChat(ctx, cancel, chat.RunAuth{User: "alice", Project: "default", Epoch: run.Epoch}, c.ID, run.ID, connection)
	done, err := app.chat.Store.Read("alice", "default", c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := done.Run(run.ID); got.State != "failed" || got.Error != chat.ErrCorpus.Error() || requests.Load() != 0 {
		t.Fatalf("unbound scope settled %s %q after %d provider requests", got.State, got.Error, requests.Load())
	}
}
