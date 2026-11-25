package chat

import (
	"encoding/json"
	"testing"
)

// storedRecord returns a conversation's stored bytes.
func storedRecord(t *testing.T, s *Store, user, id string) []byte {
	t.Helper()
	record, _, _, err := s.fetch(user, id)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

// storeRecord replaces a conversation's bytes as an earlier release or a
// restore left them, flagging it active when its runs are unfinished.
func storeRecord(t *testing.T, s *Store, user, id string, raw []byte) {
	t.Helper()
	var c Conversation
	active := json.Unmarshal(raw, &c) == nil && activeRuns(c)
	if _, err := s.db.Pool.Exec(t.Context(), `INSERT INTO chat_conversations (org, account, id, version, active, record) VALUES ($1, $2, $3, 1, $4, $5)
		ON CONFLICT (org, account, id) DO UPDATE SET version = chat_conversations.version + 1, active = EXCLUDED.active, record = EXCLUDED.record`,
		s.db.Org, Hash([]byte(user)), id, active, raw); err != nil {
		t.Fatal(err)
	}
}

// storeRecovery sets a conversation's recovery overlay as a crash left it.
func storeRecovery(t *testing.T, s *Store, user, id string, raw []byte) {
	t.Helper()
	if _, err := s.db.Pool.Exec(t.Context(), `UPDATE chat_conversations SET recovery = $4 WHERE org = $1 AND account = $2 AND id = $3`, s.db.Org, Hash([]byte(user)), id, raw); err != nil {
		t.Fatal(err)
	}
}

// storedChat returns every stored byte of the store's conversations.
func storedChat(t *testing.T, s *Store) string {
	t.Helper()
	var all []byte
	if err := s.db.Pool.QueryRow(t.Context(), `SELECT coalesce(string_agg(record || coalesce(recovery, ''::bytea), ''::bytea), ''::bytea) FROM chat_conversations WHERE org = $1`, s.db.Org).Scan(&all); err != nil {
		t.Fatal(err)
	}
	return string(all)
}
