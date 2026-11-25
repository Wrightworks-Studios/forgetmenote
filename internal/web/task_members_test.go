package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"forgetmenote/internal/automation"
	"forgetmenote/internal/messaging"
)

func TestMemberNamesAreReadFromSlackOutsideTheirLock(t *testing.T) {
	f := newTaskFixture(t)
	app := f.app
	listing, release := make(chan struct{}), make(chan struct{})
	var listed sync.Once
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/users.list" {
			w.Write([]byte(`{"ok":true}`))
			return
		}
		listed.Do(func() { close(listing) })
		<-release
		w.Write([]byte(`{"ok":true,"members":[{"id":"UNEW","team_id":"T123","profile":{"real_name":"Nell Ward"}}]}`))
	}))
	defer slow.Close()
	// A failing test still lets Slack answer, so the server can close.
	var answered sync.Once
	answer := func() { answered.Do(func() { close(release) }) }
	defer answer()
	app.slack.endpoint = slow.URL + "/chat.postEphemeral"
	app.memberNames.seed([]messaging.Member{{User: "UBOB", Name: "Bob Ray"}})
	app.memberNames.mu.Lock()
	app.memberNames.read = time.Time{}
	app.memberNames.mu.Unlock()

	// A member nobody has named yet reads the list.
	named := make(chan string, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		named <- app.memberNameWithin(ctx, "UNEW")
	}()
	<-listing
	// While Slack answers, a known name is at hand, and whoever waits for the
	// list gives up at their own deadline.
	known := make(chan string, 1)
	go func() { known <- app.memberName("UBOB") }()
	select {
	case name := <-known:
		if name != "Bob Ray" {
			t.Fatalf("a known member is named %q", name)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a known name waited for Slack")
	}
	waiting, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	if name := app.memberNameWithin(waiting, "UNEW"); name != "" || time.Since(started) > 2*time.Second {
		t.Fatalf("a waiting caller got %q after %v", name, time.Since(started))
	}
	answer()
	if name := <-named; name != "Nell Ward" {
		t.Fatalf("the new member is named %q", name)
	}
	if name := app.memberName("UNEW"); name != "Nell Ward" {
		t.Fatalf("the list was not kept: %q", name)
	}
}

func TestATaskReminderReadsTheMemberListOnce(t *testing.T) {
	f := newTaskFixture(t)
	app := f.app
	now := time.Date(2026, 11, 17, 16, 30, 0, 0, time.UTC)
	app.taskClock = func() time.Time { return now }
	f.members = []map[string]any{
		{"id": "UALICE", "team_id": "T123", "tz": "America/Denver", "profile": map[string]any{"real_name": "Alice Lee"}},
		{"id": "UBOB", "team_id": "T123", "tz": "America/Denver", "profile": map[string]any{"real_name": "Bob Ray"}},
	}
	bob := taskClients(t, app, "bob")[0]
	actionCall(t, bob, "add_task", map[string]any{"person": "<@UALICE>", "text": "Renew the parking permit"}, false)
	app.memberNames = memberNameCache{}
	before := f.memberLists
	spec := automation.Spec{ID: "task-reminder", Kind: "native", Actor: f.service, Project: "default", TimeoutSeconds: 60, Native: &automation.NativeTask{Task: "task-reminder", Client: "slack"}}
	run := automation.Run{ID: automation.Hash("tuesday"), Spec: spec}
	if err := app.runAutomation(t.Context(), &run, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	posts := f.postsTo("UALICE")
	if len(posts) == 0 {
		t.Fatalf("no reminder: %s", run.Result)
	}
	if blocks := fmt.Sprint(posts[len(posts)-1]["blocks"]); !strings.Contains(blocks, "from Bob Ray") {
		t.Fatalf("the reminder does not name bob: %v", blocks)
	}
	if lists := f.memberLists - before; lists != 1 {
		t.Fatalf("one reminder run listed the workspace %d times", lists)
	}
}
