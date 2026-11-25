package web

import (
	"context"
	"encoding/json"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/workspace"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

func notebookCitationConversation(t *testing.T, app *Server, other, selected bool) (chat.Conversation, string, *workspace.Store) {
	t.Helper()
	c, path := citationConversation(t, app, "Every batch requires cooling before packing. [Process](SOURCE)")
	project := "default"
	if other {
		project = "other"
		if err := app.catalog.Create(catalog.Bootstrap, project, "Other notebook"); err != nil {
			t.Fatal(err)
		}
		if err := app.catalog.Member(catalog.Bootstrap, project, "alice", "read"); err != nil {
			t.Fatal(err)
		}
	}
	store, err := app.catalog.Access(catalog.Bootstrap, project, "edit")
	if err != nil {
		t.Fatal(err)
	}
	if other {
		if _, err = store.Save(t.Context(), "notes/evidence.md", []byte("# Process\nEvery batch requires cooling before packing.\nOnly in the other notebook."), "", "operator"); err != nil {
			t.Fatal(err)
		}
	}
	client := actionClient(t, app, "alice", "notes:read")
	raw := actionCall(t, client, "read_document", map[string]any{"project": project, "origin": "notes", "path": "notes/evidence.md"}, false)
	citations, _ := app.chatToolEvidence(t.Context(), chat.RunAuth{User: "alice", Project: "default"}, "original-read", "read_document", raw)
	if len(citations) != 1 || citations[0].Kind != "notebook" || citations[0].Project != project {
		t.Fatalf("missing canonical tool evidence: %+v", citations)
	}
	oldURL := c.Turns[len(c.Turns)-1].Citations[0].URL
	if err = app.chat.Store.Update("alice", "default", c.ID, func(c *chat.Conversation) error {
		if other && selected {
			c.Options.Corpus.NoteProjects = []string{project}
			c.Runs[0].Options = c.Options
		}
		c.Runs[0].Citations = citations
		turn := &c.Turns[len(c.Turns)-1]
		turn.Citations = citations
		turn.Content = strings.ReplaceAll(turn.Content, oldURL, citations[0].URL)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	c, err = app.chat.Store.Read("alice", "default", c.ID)
	if err != nil {
		t.Fatal(err)
	}
	return c, path, store
}

func TestJevCanonicalNotebookCitations(t *testing.T) {
	for _, test := range []struct {
		name                                      string
		other, selected, revoke, changeDuringCall bool
		wantCalls                                 int32
		wantChecked                               int
	}{
		{name: "current notebook", wantCalls: 1, wantChecked: 1},
		{name: "selected other notebook", other: true, selected: true, wantCalls: 1, wantChecked: 1},
		{name: "unselected other notebook", other: true},
		{name: "revoked other notebook", other: true, selected: true, revoke: true},
		{name: "other source changes during judgment", other: true, selected: true, changeDuringCall: true, wantCalls: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			app := chatFixture(t)
			var calls atomic.Int32
			var source *workspace.Store
			configureSyntheticJev(t, app, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if test.changeDuringCall {
					doc, err := source.Read(r.Context(), "notes/evidence.md", "")
					if err != nil {
						t.Error(err)
						return
					}
					if _, err = source.Save(r.Context(), doc.Path, []byte("# Process\nCooling is no longer required."), doc.Revision, "operator"); err != nil {
						t.Error(err)
						return
					}
				}
				syntheticCitationAnswer(w, r)
			})
			_, path, store := notebookCitationConversation(t, app, test.other, test.selected)
			source = store
			if test.revoke {
				if err := app.catalog.Member(catalog.Bootstrap, "other", "alice", ""); err != nil {
					t.Fatal(err)
				}
			}
			if w := proxyCall(app, "alice", "POST", "/api/jev/settings", `{"consent":true,"citations":true}`); w.Code != 200 {
				t.Fatal(w.Code, w.Body)
			}
			if test.revoke {
				// A conversation that drew on a notebook the person can no longer
				// read is withheld as a whole, so no check starts.
				if w := proxyCall(app, "alice", "POST", path, `{}`); w.Code != 403 || calls.Load() != 0 {
					t.Fatal(w.Code, w.Body, calls.Load())
				}
				return
			}
			if w := proxyCall(app, "alice", "POST", path, `{}`); w.Code != 200 {
				t.Fatal(w.Code, w.Body)
			}
			check := waitCitation(t, app, path)
			if check.Checked != test.wantChecked || calls.Load() != test.wantCalls {
				t.Fatalf("check=%+v providerCalls=%d", check, calls.Load())
			}
			if test.wantChecked > 0 {
				if check.State != "completed" || check.Items[0].Status != "supported" {
					t.Fatalf("check=%+v", check)
				}
				var receipt struct {
					Items []struct {
						Evidence []struct {
							Project string `json:"project"`
						} `json:"evidence"`
					} `json:"items"`
				}
				raw, _ := json.Marshal(check)
				if err := json.Unmarshal(raw, &receipt); err != nil {
					t.Fatal(err)
				}
				project := "default"
				if test.other {
					project = "other"
				}
				if receipt.Items[0].Evidence[0].Project != project {
					t.Fatalf("lost notebook identity: %s", raw)
				}
				if test.other {
					if err := app.catalog.Member(catalog.Bootstrap, "other", "alice", ""); err != nil {
						t.Fatal(err)
					}
					w := proxyCall(app, "alice", "GET", path, "")
					if w.Code != 403 || strings.Contains(w.Body.String(), "evidence") {
						t.Fatalf("revoked source disclosed: %d %s", w.Code, w.Body)
					}
				}
			} else if check.State == "completed" {
				t.Fatalf("invalid source was verified: %+v", check)
			}
		})
	}
}

func TestJevProposedTextIsNotASourceQuotation(t *testing.T) {
	for _, test := range []struct {
		name, state, failure, project, extra string
		wrongHash                            bool
		want                                 string
	}{
		{name: "retained proposal", state: "completed", project: "default", want: "unchecked"},
		{name: "repeated proposed quotations", state: "completed", project: "default", extra: strings.Repeat(` Also append "New cooling instructions.".`, 32), want: "unchecked"},
		{name: "mixed source quotation", state: "completed", project: "default", extra: ` The source says "skip cooling".`, want: "invalid_quote"},
		{name: "failed proposal", state: "completed", failure: "proposal denied", project: "default", want: "invalid_quote"},
		{name: "pending proposal", state: "running", project: "default", want: "invalid_quote"},
		{name: "mismatched source hash", state: "completed", project: "default", wrongHash: true, want: "invalid_quote"},
		{name: "different destination notebook", state: "completed", project: "other", want: "invalid_quote"},
	} {
		t.Run(test.name, func(t *testing.T) {
			app := chatFixture(t)
			var calls atomic.Int32
			configureSyntheticJev(t, app, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); syntheticCitationAnswer(w, r) })
			c, path := citationConversation(t, app, `Submitted a reviewable proposal to append "New cooling instructions." to [Process](SOURCE).`+test.extra)
			store, err := app.catalog.Access(catalog.Bootstrap, "default", "edit")
			if err != nil {
				t.Fatal(err)
			}
			doc, err := store.Read(t.Context(), "notes/evidence.md", "")
			if err != nil {
				t.Fatal(err)
			}
			e, release, err := app.acquireEngine(t.Context(), store)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			group := workspace.EditGroup{ID: "new-text", Summary: "Proposed instruction", Edits: []workspace.NoteEdit{{Path: doc.Path, Revision: doc.Revision, Hash: workspace.ContentHash(doc.Content), Content: string(doc.Content) + "\nNew cooling instructions.\n"}}}
			proposal, err := e.Propose(context.Background(), "alice", group)
			if err != nil {
				t.Fatal(err)
			}
			if test.wrongHash {
				proposal.Group.Edits[0].Hash = "mismatched"
			}
			result, _ := json.Marshal(proposal)
			arguments, _ := json.Marshal(map[string]any{"project": test.project, "mode": "proposal", "group": group})
			if err = app.chat.Store.Update("alice", "default", c.ID, func(c *chat.Conversation) error {
				c.Runs[0].Operations = []chat.Operation{{ID: "proposal", Name: "submit_change_group", State: test.state, Error: test.failure, Arguments: arguments, Result: result}}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			proxyCall(app, "alice", "POST", "/api/jev/settings", `{"consent":true,"citations":true}`)
			if w := proxyCall(app, "alice", "POST", path, `{}`); w.Code != 200 {
				t.Fatal(w.Code, w.Body)
			}
			check := waitCitation(t, app, path)
			checked := 0
			if test.want == "invalid_quote" {
				checked = 1
			}
			if check.Checked != checked || len(check.Items) != 1 || check.Items[0].Status != test.want || calls.Load() != 0 {
				t.Fatalf("check=%+v calls=%d", check, calls.Load())
			}
		})
	}
}

func TestJevCanonicalCitationPreservesExplicitCommittedVersion(t *testing.T) {
	for _, test := range []struct {
		name           string
		other, history bool
		want           int
	}{
		{name: "explicit same-notebook attachment", want: 1},
		{name: "attachment cannot authorize another notebook", other: true},
		{name: "explicit history selection", other: true, history: true, want: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			app := chatFixture(t)
			var calls atomic.Int32
			configureSyntheticJev(t, app, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); syntheticCitationAnswer(w, r) })
			c, path, store := notebookCitationConversation(t, app, test.other, true)
			doc, err := store.Read(t.Context(), "notes/evidence.md", "")
			if err != nil {
				t.Fatal(err)
			}
			if _, err = store.Save(t.Context(), doc.Path, []byte("# Process\nCooling is no longer required."), doc.Revision, "operator"); err != nil {
				t.Fatal(err)
			}
			if err = app.chat.Store.Update("alice", "default", c.ID, func(c *chat.Conversation) error {
				c.Runs[0].Options.Corpus.History = test.history
				if !test.history {
					c.Turns[0].Attachments = []chat.Attachment{{Kind: "committed", Path: doc.Path, Revision: doc.Revision, Content: string(doc.Content)}}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			proxyCall(app, "alice", "POST", "/api/jev/settings", `{"consent":true,"citations":true}`)
			if w := proxyCall(app, "alice", "POST", path, `{}`); w.Code != 200 {
				t.Fatal(w.Code, w.Body)
			}
			check := waitCitation(t, app, path)
			if check.Checked != test.want || int(calls.Load()) != test.want {
				t.Fatalf("check=%+v calls=%d", check, calls.Load())
			}
		})
	}
}
