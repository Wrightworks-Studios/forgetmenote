package chat

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"forgetmenote/internal/pg/pgtest"
)

func redactSecret(secret string) func([]byte) ([]byte, bool, error) {
	return func(raw []byte) ([]byte, bool, error) {
		quoted, _ := json.Marshal(secret)
		inner := quoted[1 : len(quoted)-1]
		if !bytes.Contains(raw, inner) {
			return raw, false, nil
		}
		return bytes.ReplaceAll(raw, inner, []byte("[purged]")), true, nil
	}
}

// A purge redacts the transcripts of its notebook's conversations, whoever
// owns them, leaves other notebooks' alone, keeps each readable and
// rebinds a recovery overlay to the new bytes.
func TestRedactProjectRedactsOnlyThatNotebooksTranscripts(t *testing.T) {
	s, err := OpenStore(pgtest.Org(t))
	if err != nil {
		t.Fatal(err)
	}
	const secret = "sk-live-chat-<0123456789>"
	conversations := map[string]Conversation{}
	for _, owner := range []string{"alice", "bob"} {
		for _, project := range []string{"keys", "other"} {
			c, err := s.Create(owner, project, Options{})
			if err != nil {
				t.Fatal(err)
			}
			err = s.Update(owner, project, c.ID, func(c *Conversation) error {
				c.Title = "Quoted " + secret
				c.AddEvent("run", "tool", map[string]string{"result": "The key is " + secret})
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			conversations[owner+"/"+project] = c
		}
	}
	// An unfinished run's recovery overlay is bound to the record's bytes.
	alice := conversations["alice/keys"]
	var record []byte
	s.db.Pool.QueryRow(t.Context(), `SELECT record FROM chat_conversations WHERE org = $1 AND account = $2 AND id = $3`, s.db.Org, Hash([]byte("alice")), alice.ID).Scan(&record)
	overlay, _ := json.Marshal(recoveryRecord{Hash: Hash(record), Runs: []string{"run"}})
	if _, err := s.db.Pool.Exec(t.Context(), `UPDATE chat_conversations SET recovery = $4 WHERE org = $1 AND account = $2 AND id = $3`, s.db.Org, Hash([]byte("alice")), alice.ID, overlay); err != nil {
		t.Fatal(err)
	}

	// A conversation of another notebook whose corpus drew on this one quotes
	// it too.
	corpus := &Corpus{NoteProjects: []string{"other", "keys"}, Version: 1, Archives: "exclude", Drafts: "exclude"}
	drew, err := s.Create("carol", "other", Options{Corpus: corpus})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Update("carol", "other", drew.ID, func(c *Conversation) error { c.Title = "Drew on " + secret; return nil }); err != nil {
		t.Fatal(err)
	}
	// So does one whose earlier run drew on it, whatever its corpus is now.
	ran, err := s.Create("dan", "other", Options{})
	if err != nil {
		t.Fatal(err)
	}
	err = s.Update("dan", "other", ran.ID, func(c *Conversation) error {
		c.Title = "Ran on " + secret
		c.Runs = append(c.Runs, Run{ID: "run", Options: Options{Corpus: corpus}})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if n, err := s.RedactProject(t.Context(), "keys", redactSecret(secret), false); err != nil || n != 4 {
		t.Fatal("dry run:", n, err)
	}
	if c, _ := s.Read("alice", "keys", alice.ID); !strings.Contains(c.Title, secret) {
		t.Fatal("a dry run changed a transcript")
	}
	if n, err := s.RedactProject(t.Context(), "keys", redactSecret(secret), true); err != nil || n != 4 {
		t.Fatal("redact:", n, err)
	}
	if read, err := s.Read("carol", "other", drew.ID); err != nil || read.Title != "Drew on [purged]" || read.Project != "other" {
		t.Fatalf("the conversation that drew on the notebook: %+v %v", read, err)
	}
	if read, err := s.Read("dan", "other", ran.ID); err != nil || read.Title != "Ran on [purged]" {
		t.Fatalf("the conversation whose run drew on the notebook: %+v %v", read, err)
	}
	for key, c := range conversations {
		owner, project, _ := strings.Cut(key, "/")
		read, err := s.Read(owner, project, c.ID)
		if err != nil {
			t.Fatal(key, err)
		}
		raw, _ := json.Marshal(read)
		if project == "keys" && (strings.Contains(string(raw), "0123456789") || !strings.Contains(read.Title, "[purged]")) {
			t.Fatalf("%s was not redacted: %s", key, raw)
		}
		if project == "other" && !strings.Contains(read.Title, secret) {
			t.Fatalf("%s of another notebook changed", key)
		}
	}
	var recovery []byte
	s.db.Pool.QueryRow(t.Context(), `SELECT record, recovery FROM chat_conversations WHERE org = $1 AND account = $2 AND id = $3`, s.db.Org, Hash([]byte("alice")), alice.ID).Scan(&record, &recovery)
	var bound recoveryRecord
	if json.Unmarshal(recovery, &bound) != nil || bound.Hash != Hash(record) || len(bound.Runs) != 1 {
		t.Fatalf("the recovery overlay is not bound to the redacted record: %s", recovery)
	}
	// A redaction that would leave a record unreadable is refused.
	broken := func(raw []byte) ([]byte, bool, error) { return []byte(`{"unknown":true}`), true, nil }
	if _, err := s.RedactProject(t.Context(), "other", broken, true); err == nil {
		t.Fatal("an unreadable redaction was written")
	}
}

// A transcript whose redaction would not decode is left as it was and
// reported; the other transcripts are still redacted.
func TestRedactProjectSkipsATranscriptItCannotRedact(t *testing.T) {
	s, err := OpenStore(pgtest.Org(t))
	if err != nil {
		t.Fatal(err)
	}
	const secret = "sk-live-chat-skip-0123456789"
	var ids []string
	for _, owner := range []string{"alice", "bob"} {
		c, err := s.Create(owner, "keys", Options{})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Update(owner, "keys", c.ID, func(c *Conversation) error { c.Title = owner + " quoted " + secret; return nil }); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, c.ID)
	}
	redact := redactSecret(secret)
	// alice's transcript redacts into something that is no transcript.
	fn := func(raw []byte) ([]byte, bool, error) {
		if bytes.Contains(raw, []byte("alice quoted")) {
			return []byte(`{"unknown":true}`), true, nil
		}
		return redact(raw)
	}
	n, err := s.RedactProject(t.Context(), "keys", fn, true)
	if err == nil || n != 1 {
		t.Fatal("redact:", n, err)
	}
	alice, err := s.Read("alice", "keys", ids[0])
	if err != nil || alice.Title != "alice quoted "+secret {
		t.Fatalf("alice's transcript: %+v %v", alice.Title, err)
	}
	bob, err := s.Read("bob", "keys", ids[1])
	if err != nil || bob.Title != "bob quoted [purged]" {
		t.Fatalf("bob's transcript: %+v %v", bob.Title, err)
	}
}
