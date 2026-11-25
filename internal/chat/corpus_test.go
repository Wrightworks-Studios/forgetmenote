package chat

import (
	"encoding/json"
	"errors"
	"forgetmenote/internal/pg/pgtest"
	"reflect"
	"testing"
)

func testCorpus() *Corpus {
	return &Corpus{Version: 1, Archives: "exclude", Drafts: "exclude", History: false}
}
func TestCorpusLegacyReadMigrationAndInvalidResume(t *testing.T) {
	s, err := OpenStore(pgtest.Org(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c, err := s.Create("alice", "alpha", Options{Mode: "ask", Corpus: testCorpus()})
	if err != nil {
		t.Fatal(err)
	}
	c.Version = 1
	c.Options.Corpus = nil
	c.Turns = []Turn{{ID: "old-user", RunID: "old", Role: "user", Content: "Original private user turn"}, {ID: "old-answer", RunID: "old", Role: "assistant", Content: "Original private answer", Citations: []Citation{{ID: "old-citation", Path: "note.md", Revision: "old-revision", Text: "Original quoted text"}}}}
	c.Runs = []Run{{ID: "old", State: "interrupted", Options: Options{Mode: "ask"}, Usage: Accounting{InputTokens: 17, OutputTokens: 3, Requests: 1}}}
	raw, _ := json.Marshal(c)
	storeRecord(t, s, "alice", c.ID, raw)
	before, err := s.Read("alice", "alpha", c.ID)
	if err != nil {
		t.Fatal(err)
	}
	afterRead := storedRecord(t, s, "alice", c.ID)
	if string(afterRead) != string(raw) {
		t.Fatal("reading migrated legacy bytes")
	}
	if _, err = s.Resume("alice", "alpha", c.ID, "old", 2, 2, 0); !errors.Is(err, ErrCorpus) {
		t.Fatal("legacy scope resumed", err)
	}
	run := Run{ID: "new", SubmissionID: "new", RequestHash: "new", State: "queued", Options: Options{Mode: "ask", Corpus: testCorpus()}, Usage: Accounting{Reserved: 100}}
	got, _, err := s.Start("alice", "alpha", c.ID, run, Turn{ID: "new-user", RunID: "new", Role: "user", Content: "New explicit task"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != 2 || !reflect.DeepEqual(got.Turns[:len(before.Turns)], before.Turns) || !reflect.DeepEqual(got.Runs[0], before.Runs[0]) || got.Usage().Tokens() != 20 {
		t.Fatal("migration changed original transcript/accounting", got)
	}
	// Unknown scope stays readable and accounted, but can never authorize a resume.
	var wire map[string]any
	record := storedRecord(t, s, "alice", c.ID)
	json.Unmarshal(record, &wire)
	runs := wire["runs"].([]any)
	latest := runs[1].(map[string]any)
	latest["state"] = "interrupted"
	latest["options"].(map[string]any)["corpus"] = map[string]any{"version": 2, "futureChoice": "unknown"}
	record, _ = json.Marshal(wire)
	storeRecord(t, s, "alice", c.ID, record)
	invalid, err := s.Read("alice", "alpha", c.ID)
	if err != nil || len(invalid.Turns) != 3 || invalid.Usage().Tokens() != 20 {
		t.Fatal("unknown corpus hides transcript/accounting", err, invalid)
	}
	if _, err = s.Resume("alice", "alpha", c.ID, "new", 2, 2, 0); !errors.Is(err, ErrCorpus) {
		t.Fatal("unknown corpus resumed", err)
	}
	unchanged := storedRecord(t, s, "alice", c.ID)
	if string(unchanged) != string(record) {
		t.Fatal("scope refusal modified record")
	}
}
func TestCorpusRequiredCompleteWire(t *testing.T) {
	for _, raw := range []string{`{}`, `{"version":1,"archives":"exclude","drafts":"exclude"}`, `{"version":1,"archives":"exclude","drafts":"exclude","history":false,"future":true}`, `{"version":2,"archives":"exclude","drafts":"exclude","history":false}`, `{"version":1,"archives":"exclude","drafts":"exclude","history":null}`} {
		var c Corpus
		if err := json.Unmarshal([]byte(raw), &c); err != nil {
			t.Fatal(err)
		}
		if c.Validate() == nil || c.ID() != "" {
			t.Fatal("invalid scope accepted", raw)
		}
		saved, _ := json.Marshal(c)
		if string(saved) != raw {
			t.Fatal("unknown scope rewritten", string(saved), raw)
		}
	}
	corpus := testCorpus()
	raw, _ := json.Marshal(corpus)
	if string(raw) != `{"version":1,"archives":"exclude","drafts":"exclude","history":false}` {
		t.Fatal(string(raw))
	}
	if corpus.ID() != Hash(raw) {
		t.Fatal("unstable identity")
	}
}
