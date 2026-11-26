package web

import (
	"encoding/json"
	"fmt"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/citationchecks"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestJevR1CitationPipelinesBound(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	app := reviewFixture(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
			syntheticCitationAnswer(w, r)
		case <-r.Context().Done():
		}
	})
	c, _ := citationConversation(t, app, "Cooling is required [source](SOURCE).")
	err := app.chat.Store.Update(c.Owner, c.Project, c.ID, func(value *chat.Conversation) error {
		baseRun := value.Runs[0]
		baseTurn := value.Turns[len(value.Turns)-1]
		for i := 1; i < 16; i++ {
			run := baseRun
			run.ID = fmt.Sprintf("probe-%d", i)
			turn := baseTurn
			turn.ID = fmt.Sprintf("probe-turn-%d", i)
			turn.RunID = run.ID
			value.Runs = append(value.Runs, run)
			userTurn := value.Turns[0]
			userTurn.ID = fmt.Sprintf("probe-user-%d", i)
			userTurn.RunID = run.ID
			value.Turns = append(value.Turns, userTurn, turn)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 16; i++ {
		run := "run"
		if i > 0 {
			run = fmt.Sprintf("probe-%d", i)
		}
		w := proxyCall(app, "alice", "POST", "/p/default/api/chat/"+c.ID+"/runs/"+run+"/citations", "{}")
		if w.Code != 200 {
			t.Fatalf("start %d: %d %s", i, w.Code, w.Body.String())
		}
	}
	active := app.citations.Active()
	t.Logf("active_citation_workers=%d configured_provider_concurrency=%d", active, app.chat.Config.Jev.Limits.Concurrent)
	if active > app.chat.Config.Jev.Limits.Concurrent {
		t.Errorf("complete citation preparation pipelines are retained outside shared request concurrency")
	}
}

func TestJevR1QueuedCitationChecksDrainAndRecheckAuthority(t *testing.T) {
	for _, mode := range []string{"drain", "consent", "epoch"} {
		t.Run(mode, func(t *testing.T) {
			revoke := mode != "drain"
			release := make(chan struct{})
			var once sync.Once
			defer once.Do(func() { close(release) })
			var calls atomic.Int32
			app := reviewFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				select {
				case <-release:
					syntheticCitationAnswer(w, r)
				case <-r.Context().Done():
				}
			})
			c, path := citationConversation(t, app, "Cooling is required [source](SOURCE).")
			if err := app.chat.Store.Update(c.Owner, c.Project, c.ID, func(value *chat.Conversation) error {
				baseRun := value.Runs[0]
				baseUser := value.Turns[0]
				baseAnswer := value.Turns[1]
				for i := 1; i < 4; i++ {
					run := baseRun
					run.ID = fmt.Sprintf("queued-%d", i)
					user := baseUser
					user.ID = run.ID + "-user"
					user.RunID = run.ID
					answer := baseAnswer
					answer.ID = run.ID + "-answer"
					answer.RunID = run.ID
					value.Runs = append(value.Runs, run)
					value.Turns = append(value.Turns, user, answer)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			paths := []string{path}
			for i := 1; i < 4; i++ {
				paths = append(paths, strings.Replace(path, "/runs/run/", fmt.Sprintf("/runs/queued-%d/", i), 1))
			}
			for _, path := range paths {
				if w := proxyCall(app, "alice", "POST", path, `{}`); w.Code != 200 {
					t.Fatal(w.Code, w.Body.String())
				}
			}
			w := proxyCall(app, "alice", "GET", paths[3], "")
			var check citationchecks.Check
			json.Unmarshal(w.Body.Bytes(), &check)
			if w.Code != 200 || check.State != "pending" || check.Turn != "" {
				t.Fatal("queued pipeline prepared or lost pending state", w.Code, w.Body.String())
			}
			if mode == "epoch" {
				if err := app.catalog.Revoke(catalog.Bootstrap, "alice"); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "consent" {
				if w = proxyCall(app, "alice", "POST", "/api/jev/settings", `{"consent":false,"rerank":true,"citations":true}`); w.Code != 200 {
					t.Fatal(w.Code, w.Body.String())
				}
			}
			once.Do(func() { close(release) })
			for _, path := range paths {
				check = waitCitation(t, app, path)
				if !revoke && check.State != "completed" {
					t.Fatal(check)
				}
			}
			if revoke && calls.Load() > 2 {
				t.Fatal("queued calls ignored revoked consent", calls.Load())
			}
			if !revoke && calls.Load() != 4 {
				t.Fatal("queue did not drain", calls.Load())
			}
		})
	}
}

func TestJevR1DeletingQueuedCitationChatRemovesPendingRecord(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	app := reviewFixture(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
			syntheticCitationAnswer(w, r)
		case <-r.Context().Done():
		}
	})
	c, path := citationConversation(t, app, "Cooling is required [source](SOURCE).")
	if w := proxyCall(app, "alice", "POST", path, `{}`); w.Code != 200 {
		t.Fatal(w.Code)
	}
	// A second run occupies the other pipeline; a different chat remains queued.
	if err := app.chat.Store.Update(c.Owner, c.Project, c.ID, func(v *chat.Conversation) error {
		run := v.Runs[0]
		run.ID = "second"
		u := v.Turns[0]
		u.ID = "second-user"
		u.RunID = "second"
		a := v.Turns[1]
		a.ID = "second-answer"
		a.RunID = "second"
		v.Runs = append(v.Runs, run)
		v.Turns = append(v.Turns, u, a)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if w := proxyCall(app, "alice", "POST", strings.Replace(path, "/runs/run/", "/runs/second/", 1), `{}`); w.Code != 200 {
		t.Fatal(w.Code)
	}
	other, err := app.chat.Store.Create("alice", "default", c.Options)
	if err != nil {
		t.Fatal(err)
	}
	if err = app.chat.Store.Update("alice", "default", other.ID, func(v *chat.Conversation) error {
		v.Runs = c.Runs
		v.Turns = c.Turns
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	queued := strings.Replace(path, c.ID, other.ID, 1)
	if w := proxyCall(app, "alice", "POST", queued, `{}`); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w := proxyCall(app, "alice", "DELETE", "/p/default/api/chat/"+other.ID, ""); w.Code != 200 {
		t.Fatal("queued chat cannot be deleted", w.Code, w.Body.String())
	}
	once.Do(func() { close(release) })
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		active := app.citations.Active()
		if active == 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if w := proxyCall(app, "alice", "GET", queued, ""); w.Code != 404 {
		t.Fatal("deleted queued chat disclosed state", w.Code)
	}
	record, e := app.chatrun.CitationRecord("alice", "default", other.ID)
	if e != nil || len(record.Checks) != 0 {
		t.Fatal("queued worker recreated deleted claims", record, e)
	}
}
