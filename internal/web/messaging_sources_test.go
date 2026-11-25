package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"forgetmenote/internal/automation"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/messaging"
	"forgetmenote/internal/workspace"
)

func TestMessagingRevalidatesEarlierAnswerBeforeFollowup(t *testing.T) {
	for _, tc := range []struct {
		name              string
		changed, recovery bool
	}{
		{name: "unrelated save preserves evidence"},
		{name: "changed source omits earlier answer", changed: true},
		{name: "recovery still rejects changed evidence", changed: true, recovery: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			app, _ := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				var input struct{ Messages []chat.Message }
				if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
					t.Fatal(err)
				}
				found := false
				for _, m := range input.Messages {
					found = found || strings.Contains(m.Content, "Earlier total is seven")
				}
				if found == tc.changed {
					t.Errorf("earlier answer retained = %v, source changed = %v", found, tc.changed)
				}
				w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Fresh follow-up."},"finish_reason":"stop"}]}`))
			})
			configureSlackFixture(t, app)
			transport := &fixtureChatTransport{}
			app.messageClients["slack"] = transport
			store, _ := app.catalog.Access(catalog.Bootstrap, "default", "edit")
			doc, err := store.Save(t.Context(), "source.md", []byte("The total is seven."), "", "test")
			if err != nil {
				t.Fatal(err)
			}
			d := messaging.Address{Client: "slack", Workspace: "T123", Channel: "C123", Thread: "1.2", User: "U123"}
			owner, binding := messageConversation(app.slack.policy(), app.slack.config.Channels["C123"], d, "alice")
			c, err := app.chat.Store.OpenBound(owner, "default", binding, chat.Options{Mode: "ask", Connection: "local", Model: "synthetic"})
			if err != nil {
				t.Fatal(err)
			}
			citation := chat.Citation{Kind: "notebook", Project: "default", Path: doc.Path, Revision: doc.Revision, Hash: workspace.ContentHash(doc.Content)}
			oldID := "old"
			if tc.recovery {
				oldID = automation.Hash(tc.name)
			}
			if err := app.chat.Store.Update(owner, "default", c.ID, func(c *chat.Conversation) error {
				c.Turns = append(c.Turns, chat.Turn{ID: chat.NewID(), RunID: oldID, Role: "user", Content: "What is the total?"}, chat.Turn{ID: chat.NewID(), RunID: oldID, Role: "assistant", Content: "Earlier total is seven.", Citations: []chat.Citation{citation}})
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			path, revision := "unrelated.md", ""
			if tc.changed {
				path, revision = doc.Path, doc.Revision
			}
			if _, err := store.Save(t.Context(), path, []byte("The total is nine."), revision, "test"); err != nil {
				t.Fatal(err)
			}
			r := automation.Run{ID: automation.Hash(tc.name), Spec: automation.Spec{ID: "followup", Kind: "assistant", Actor: "alice", Project: "default", AIConnection: "local", Model: "synthetic", Instruction: "What about now?", ExternalProcessing: true, Delivery: &d}}
			err = app.runAutomation(t.Context(), &r, func() error { return nil })
			if tc.recovery {
				if !errors.Is(err, workspace.ErrConflict) || calls != 0 || len(transport.replies) != 0 {
					t.Fatal("recovery reused changed evidence or regenerated", err, calls, transport.replies)
				}
				return
			}
			if err != nil {
				t.Fatal("old evidence blocked a fresh request", err)
			}
			if calls != 1 || len(transport.replies) != 1 || transport.replies[0].Text != "Fresh follow-up." {
				t.Fatal("follow-up did not reach the model and deliver", calls, transport.replies)
			}
			if (len(r.Citations) > 0) == tc.changed {
				t.Fatal("retained the wrong citation set", r.Citations)
			}
		})
	}
}

func TestMessagingKeepsAnswerLinksWithoutDumpingRetrievedSources(t *testing.T) {
	app, _ := runnerFixture(t, func(http.ResponseWriter, *http.Request) {
		t.Error("a saved answer must not regenerate")
	})
	configureSlackFixture(t, app)
	transport := &fixtureChatTransport{}
	app.messageClients["slack"] = transport
	store, _ := app.catalog.Access(catalog.Bootstrap, "default", "edit")
	doc, err := store.Save(t.Context(), "unrelated.md", []byte("Retrieved but not used in this answer."), "", "test")
	if err != nil {
		t.Fatal(err)
	}
	answer := "The answer is **seven**. [Evidence](https://example.com/evidence)."
	r := automation.Run{
		ID:        automation.Hash("concise-message"),
		Spec:      automation.Spec{ID: "concise-message", Kind: "assistant", Actor: "alice", Project: "default", AIConnection: "local", Model: "synthetic", Instruction: "How many?", ExternalProcessing: true, Delivery: &automation.Delivery{Channel: "C123", Thread: "1.2", User: "U123"}},
		Answer:    answer,
		Citations: []chat.Citation{{Kind: "notebook", Project: "default", Path: doc.Path, Revision: doc.Revision, Hash: workspace.ContentHash(doc.Content), Title: "Unrelated retrieval", URL: "/unrelated"}},
	}
	if err := app.runAutomation(t.Context(), &r, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if len(transport.replies) != 1 || transport.replies[0].Text != answer {
		t.Fatal("delivery appended unrelated retrieval instead of keeping the answer's inline links", transport.replies)
	}
	if len(r.Citations) != 1 {
		t.Fatal("retrieval provenance was discarded from the private run")
	}
}

func TestMessagingMakesNotebookCitationLinksAbsolute(t *testing.T) {
	app, _ := runnerFixture(t, func(http.ResponseWriter, *http.Request) { t.Error("saved answer regenerated") })
	configureSlackFixture(t, app)
	transport := &fixtureChatTransport{}
	app.messageClients["slack"] = transport
	store, _ := app.catalog.Access(catalog.Bootstrap, "default", "edit")
	doc, err := store.Save(t.Context(), "source.md", []byte("# Evidence\nSeven."), "", "test")
	if err != nil {
		t.Fatal(err)
	}
	link := "/?project=default&note=source.md&revision=" + doc.Revision + "#evidence"
	answer := "Seven. [Source](" + link + "). [External](https://example.com/evidence)."
	r := automation.Run{ID: automation.Hash("absolute-citation"), Spec: automation.Spec{ID: "absolute-citation", Kind: "assistant", Actor: "alice", Project: "default", AIConnection: "local", Model: "synthetic", Instruction: "How many?", ExternalProcessing: true, Delivery: &automation.Delivery{Channel: "C123", Thread: "1.2", User: "U123"}}, Answer: answer, Citations: []chat.Citation{{Kind: "notebook", Project: "default", Path: doc.Path, Revision: doc.Revision, Hash: workspace.ContentHash(doc.Content), URL: link}}}
	if err := app.runAutomation(t.Context(), &r, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	want := strings.ReplaceAll(answer, "]("+link+")", "]("+app.origin+link+")")
	if len(transport.replies) != 1 || transport.replies[0].Text != want || r.Answer != answer {
		t.Fatal("delivery lost the instance origin or changed the retained answer", transport.replies)
	}
}
