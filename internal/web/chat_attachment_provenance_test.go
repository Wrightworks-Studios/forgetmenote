package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"forgetmenote/internal/chat"
	"forgetmenote/internal/chatrun"
	"forgetmenote/internal/workspace"
)

func TestChatExplicitAttachmentSnapshotsRemainBoundAndLabelled(t *testing.T) {
	for _, kind := range []string{"private", "live"} {
		t.Run(kind, func(t *testing.T) {
			app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{}`) })
			save := func(path, content, revision string) {
				t.Helper()
				raw, _ := json.Marshal(map[string]string{"path": path, "content": content, "revision": revision})
				w := call(app, "POST", "/api/note", string(raw), testToken)
				if w.Code != 200 {
					t.Fatal(w.Body)
				}
			}
			save("notes/alphalaunch.md", "# Alphalaunch\nAn unrelated committed source.", "")
			save("notes/attached.md", "ACCEPTED_OLD_SNAPSHOT", "")
			store, _ := app.catalog.Access("alice", "default", "read")
			input := chat.Attachment{Kind: kind, Path: "notes/attached.md"}
			if kind == "private" {
				input.Content = "ACCEPTED_OLD_SNAPSHOT"
			}
			attachments, err := app.chatrun.Attachments(t.Context(), store, []chat.Attachment{input})
			if err != nil {
				t.Fatal(err)
			}
			c.Turns = []chat.Turn{{ID: chat.NewID(), Role: "user", Content: "alphalaunch", Attachments: attachments}}
			connection := app.chat.Config.Connections[0]
			_, citations, memory, err := initialChatTestMessages(t, app, c, connection)
			if err != nil {
				t.Fatal(err)
			}
			if !memory.Provenance.Complete || len(memory.Provenance.AttachmentReferences) != 1 || memory.Provenance.AttachmentReferences[0].Kind != kind {
				t.Fatalf("explicit input not independently tracked: %+v", memory.Provenance)
			}
			c.Turns = append(c.Turns, chat.Turn{ID: chat.NewID(), Role: "assistant", Content: "SNAPSHOT_DERIVED_ANSWER", Citations: citations, Provenance: memory.Provenance}, chat.Turn{ID: chat.NewID(), Role: "user", Content: "alphalaunch"})
			old, err := store.Read(t.Context(), "notes/attached.md", "")
			if err != nil {
				t.Fatal(err)
			}
			save(old.Path, "NEW_CONTENT_MUST_NOT_BE_SAMPLED", old.Revision)
			if err = app.chat.Store.Update("alice", "default", c.ID, func(saved *chat.Conversation) error { saved.Turns = c.Turns; return nil }); err != nil {
				t.Fatal(err)
			}
			reloaded, err := app.chat.Store.Read("alice", "default", c.ID)
			if err != nil {
				t.Fatal(err)
			}
			messages, _, _, err := initialChatTestMessages(t, app, reloaded, connection)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(messages)
			if strings.Contains(string(raw), "SNAPSHOT_DERIVED_ANSWER") || strings.Contains(string(raw), "ACCEPTED_OLD_SNAPSHOT") || strings.Contains(string(raw), "NEW_CONTENT_MUST_NOT_BE_SAMPLED") {
				t.Fatalf("prior attachment exception entered a new run without selection: %s", raw)
			}
			// Selecting the exact accepted snapshot again shares its original bytes,
			// without implicitly fetching the newer committed/live content.
			reloaded.Turns[len(reloaded.Turns)-1].Attachments = attachments
			messages, _, _, err = initialChatTestMessages(t, app, reloaded, connection)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ = json.Marshal(messages)
			if !strings.Contains(string(raw), "ACCEPTED_OLD_SNAPSHOT") || strings.Contains(string(raw), "NEW_CONTENT_MUST_NOT_BE_SAMPLED") {
				t.Fatalf("explicitly selected snapshot lost its immutable bytes: %s", raw)
			}
			// Within its original turn the proof stays valid until the exact
			// accepted input is inconsistent; unrelated citations cannot certify it.
			snapshot, err := store.Snapshot(t.Context(), "")
			if err != nil {
				t.Fatal(err)
			}
			originalTurn := reloaded.Turns[0]
			valid := chatrun.EvidenceValidator(snapshot, memory.Provenance.InstructionsHash, []chat.Turn{originalTurn}, c.Options.Corpus.ID())
			if !valid(memory.Provenance, citations) {
				t.Fatal("original selected snapshot lost valid proof")
			}
			originalTurn.Attachments = append([]chat.Attachment(nil), originalTurn.Attachments...)
			originalTurn.Attachments[0].Content = "UNVERIFIABLE_CHANGED_INPUT"
			valid = chatrun.EvidenceValidator(snapshot, memory.Provenance.InstructionsHash, []chat.Turn{originalTurn}, c.Options.Corpus.ID())
			if valid(memory.Provenance, citations) {
				t.Fatal("unverifiable accepted input remained reusable")
			}
			stored, err := app.chat.Store.Read("alice", "default", c.ID)
			if err != nil || stored.Turns[1].Content != "SNAPSHOT_DERIVED_ANSWER" || stored.Turns[0].Attachments[0].Content != "ACCEPTED_OLD_SNAPSHOT" {
				t.Fatal("invalidating reusable context changed the stored transcript or attachment")
			}
		})
	}
}

func TestChatPinnedAttachmentNeverAcquiresCurrentFingerprint(t *testing.T) {
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{}`) })
	save := func(path, content, revision string) {
		t.Helper()
		raw, _ := json.Marshal(map[string]string{"path": path, "content": content, "revision": revision})
		w := call(app, "POST", "/api/note", string(raw), testToken)
		if w.Code != 200 {
			t.Fatal(w.Body)
		}
	}
	save("notes/alphalaunch.md", "# Alphalaunch\nUnrelated retrieval.", "")
	save("notes/attached.md", "ORIGINAL_PINNED_ATTACHMENT", "")
	store, _ := app.catalog.Access("alice", "default", "read")
	attachments, err := app.chatrun.Attachments(t.Context(), store, []chat.Attachment{{Kind: "committed", Path: "notes/attached.md"}})
	if err != nil {
		t.Fatal(err)
	}
	original, err := store.Snapshot(t.Context(), attachments[0].Revision)
	if err != nil {
		t.Fatal(err)
	}
	expected := "attachment:" + attachments[0].Hash
	save("notes/attached.md", "NEWER_ATTACHMENT_VERSION", attachments[0].Revision)
	c.Turns = []chat.Turn{{ID: chat.NewID(), Role: "user", Content: "alphalaunch", Attachments: attachments}}
	messages, citations, memory, err := initialChatTestMessages(t, app, c, app.chat.Config.Connections[0])
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(messages)
	if !strings.Contains(string(raw), "ORIGINAL_PINNED_ATTACHMENT") || strings.Contains(string(raw), "NEWER_ATTACHMENT_VERSION") || memory.Provenance.Dependencies[attachments[0].Path] != "" || len(memory.Provenance.AttachmentReferences) != 1 || memory.Provenance.AttachmentReferences[0].References != expected || memory.Provenance.AttachmentReferences[0].Revision != original.Revision {
		t.Fatal("historical attachment acquired a newer source identity")
	}
	c.Turns = append(c.Turns, chat.Turn{ID: chat.NewID(), Role: "assistant", Content: "HISTORICAL_ATTACHMENT_CLAIM", Citations: citations, Provenance: memory.Provenance}, chat.Turn{ID: chat.NewID(), Role: "user", Content: "alphalaunch"})
	messages, _, _, err = initialChatTestMessages(t, app, c, app.chat.Config.Connections[0])
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(messages)
	if strings.Contains(string(raw), "HISTORICAL_ATTACHMENT_CLAIM") {
		t.Fatal("historical attachment answer silently reused as current evidence")
	}
}

func TestChatAttachmentProofInheritanceAndBounds(t *testing.T) {
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{}`) })
	connection := app.chat.Config.Connections[0]
	c.Turns = []chat.Turn{{ID: chat.NewID(), Role: "user", Content: "hello", Attachments: []chat.Attachment{{Kind: "private", Path: "notes/private.md", Content: "EXPLICIT_PRIVATE_ORIGINAL"}}}}
	_, citations, first, err := initialChatTestMessages(t, app, c, connection)
	if err != nil {
		t.Fatal(err)
	}
	c.Turns = append(c.Turns, chat.Turn{ID: chat.NewID(), Role: "assistant", Content: "ORIGINAL_PRIVATE_DERIVED_CLAIM", Citations: citations, Provenance: first.Provenance})
	for i := 0; i < 12; i++ {
		c.Turns = append(c.Turns, chat.Turn{ID: chat.NewID(), Role: "user", Content: "Intervening conversation"})
	}
	c.Turns = append(c.Turns, chat.Turn{ID: chat.NewID(), Role: "user", Content: "hello"})
	_, ownCitations, inherited, err := initialChatTestMessages(t, app, c, connection)
	if err != nil {
		t.Fatal(err)
	}
	if len(inherited.Provenance.AttachmentReferences) != 0 || strings.Contains(inherited.Text, "ORIGINAL_PRIVATE_DERIVED_CLAIM") {
		t.Fatal("compact memory inherited a prior run’s private attachment exception")
	}
	// A same-run descendant retains its source proof, but that does not grant a later turn the attachment.
	descendant := chatrun.CloneProvenance(first.Provenance)
	chatrun.InheritProvenance(&descendant, chat.Turn{Provenance: first.Provenance, Citations: citations})
	if !descendant.Complete || len(descendant.AttachmentReferences) != 1 {
		t.Fatal("same-run attachment inheritance lost bounded proof")
	}
	c.Turns = append(c.Turns, chat.Turn{ID: chat.NewID(), Role: "assistant", Content: "DESCENDANT_PRIVATE_CLAIM", Citations: ownCitations, Provenance: descendant}, chat.Turn{ID: chat.NewID(), Role: "user", Content: "hello"})
	messages, _, _, err := initialChatTestMessages(t, app, c, connection)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(messages)
	if strings.Contains(string(raw), "DESCENDANT_PRIVATE_CLAIM") {
		t.Fatal("a prior run’s inherited attachment exception reached the next turn")
	}
	if c.Turns[1].Content != "ORIGINAL_PRIVATE_DERIVED_CLAIM" || c.Turns[len(c.Turns)-2].Content != "DESCENDANT_PRIVATE_CLAIM" {
		t.Fatal("context filtering rewrote the transcript")
	}
	c.Turns[0].Attachments = nil
	messages, _, _, err = initialChatTestMessages(t, app, c, connection)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(messages)
	if strings.Contains(string(raw), "DESCENDANT_PRIVATE_CLAIM") {
		t.Fatal("descendant borrowed unrelated proof after original input disappeared")
	}
	target := chat.Provenance{CorpusID: c.Options.Corpus.ID(), Complete: true, AttachmentsTracked: true}
	for i := 0; i < 65; i++ {
		ref := chat.AttachmentReference{TurnID: fmt.Sprint(i), Kind: "private", Path: "notes/private.md", Hash: workspace.ContentHash([]byte(fmt.Sprint(i)))}
		chatrun.InheritProvenance(&target, chat.Turn{Provenance: chat.Provenance{CorpusID: c.Options.Corpus.ID(), Complete: true, AttachmentsTracked: true, AttachmentReferences: []chat.AttachmentReference{ref}}})
	}
	if target.Complete || len(target.AttachmentReferences) != 64 {
		t.Fatal("attachment dependency overflow remained reusable or unbounded")
	}
}
