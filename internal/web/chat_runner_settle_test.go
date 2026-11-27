package web

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"forgetmenote/internal/chat"
)

// waitChatActivity returns the backend of this database that matches where,
// a pg_stat_activity condition, once one does.
func waitChatActivity(t *testing.T, app *Server, where string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var pid int
		if err := app.db.Pool.QueryRow(t.Context(), `SELECT pid FROM pg_stat_activity WHERE datname = current_database() AND `+where+` LIMIT 1`).Scan(&pid); err == nil {
			return pid
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no backend matched %s", where)
	return 0
}

// A failed read of the run's own conversation must not leave it running: the
// person could otherwise start no conversation until the gateway restarts.
func TestChatStoreReadFailureLeavesRunTerminal(t *testing.T) {
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"Synthetic answer"},"finish_reason":"stop"}]}`)
	})
	ctx := t.Context()
	fault := "chat_read_fault_" + strings.ToLower(app.db.Org)
	// Admission's write waits on an advisory lock the test holds, so a table
	// lock can be queued ahead of the runner's first read of the conversation.
	if _, err := app.db.Pool.Exec(ctx, `CREATE FUNCTION `+fault+`() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_advisory_xact_lock(hashtext('`+fault+`')); RETURN NEW; END $$;
		CREATE TRIGGER `+fault+` BEFORE UPDATE ON chat_conversations FOR EACH ROW WHEN (NEW.org = '`+app.db.Org+`' AND NEW.active AND NOT OLD.active) EXECUTE FUNCTION `+fault+`()`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		app.db.Pool.Exec(t.Context(), `DROP TRIGGER IF EXISTS `+fault+` ON chat_conversations; DROP FUNCTION IF EXISTS `+fault+`()`)
	})
	gate, err := app.db.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Release()
	if _, err = gate.Exec(ctx, `SELECT pg_advisory_lock(hashtext($1))`, fault); err != nil {
		t.Fatal(err)
	}
	submitted := make(chan int, 1)
	go func() {
		submitted <- proxyCall(app, "alice", "POST", "/p/default/api/chat/"+c.ID+"/turns", `{"submissionId":"read-fault","text":"What does the notebook say?","options":{"mode":"ask"}}`).Code
	}()
	waitChatActivity(t, app, `wait_event_type = 'Lock' AND wait_event = 'advisory'`)
	holder, err := app.db.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Release()
	tx, err := holder.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	locked := make(chan error, 1)
	go func() {
		_, err := tx.Exec(ctx, `LOCK TABLE chat_conversations IN ACCESS EXCLUSIVE MODE`)
		locked <- err
	}()
	waitChatActivity(t, app, `wait_event_type = 'Lock' AND query LIKE 'LOCK TABLE chat_conversations%'`)
	if _, err = gate.Exec(ctx, `SELECT pg_advisory_unlock(hashtext($1))`, fault); err != nil {
		t.Fatal(err)
	}
	if err = <-locked; err != nil {
		t.Fatal(err)
	}
	if code := <-submitted; code != 200 {
		t.Fatalf("turn was not admitted: %d", code)
	}
	reader := waitChatActivity(t, app, `wait_event_type = 'Lock' AND query LIKE 'SELECT version, record, recovery FROM chat_conversations%'`)
	if _, err = app.db.Pool.Exec(ctx, `SELECT pg_cancel_backend($1)`, reader); err != nil {
		t.Fatal(err)
	}
	if err = tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	got := waitChatRun(t, app, c.ID)
	if run := got.Runs[len(got.Runs)-1]; run.State != "failed" || !strings.Contains(run.Error, "stopped unexpectedly") || run.Usage.Reserved != 0 {
		t.Fatalf("store read failure left run %s %q reserved=%d", run.State, run.Error, run.Usage.Reserved)
	}
	if w := proxyCall(app, "alice", "POST", "/p/default/api/chat/"+c.ID+"/turns", `{"submissionId":"after-fault","text":"Try again.","options":{"mode":"ask"}}`); w.Code != 200 {
		t.Fatalf("person cannot start a conversation after the failure: %d %s", w.Code, w.Body)
	}
	if run := waitChatRun(t, app, c.ID).Runs[1]; run.State != "completed" {
		t.Fatalf("later run %s %q", run.State, run.Error)
	}
}

// Only the runner's deliberate pause for confirmation outlives its exit.
func TestChatRunnerExitSettlesOnlyUnfinishedRuns(t *testing.T) {
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) { t.Error("unexpected provider request") })
	runs := map[string]chat.Run{}
	for _, state := range []string{"running", "queued", "awaiting_confirmation", "interrupted"} {
		runs[state] = chat.Run{ID: chat.NewID(), State: state, Options: c.Options}
	}
	if err := app.chat.Store.Update("alice", "default", c.ID, func(saved *chat.Conversation) error {
		for _, run := range runs {
			saved.Runs = append(saved.Runs, run)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, run := range runs {
		app.chatrun.RecoverRun(chat.RunAuth{User: "alice", Project: "default"}, c.ID, run.ID)
	}
	saved, err := app.chat.Store.Read("alice", "default", c.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"running": "failed", "queued": "failed", "awaiting_confirmation": "awaiting_confirmation", "interrupted": "interrupted"}
	for before, run := range runs {
		if got := saved.Run(run.ID); got.State != want[before] || want[before] == "failed" && !strings.Contains(got.Error, "stopped unexpectedly") {
			t.Errorf("%s run exited as %s %q", before, got.State, got.Error)
		}
	}
}
