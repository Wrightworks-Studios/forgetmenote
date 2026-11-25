package notestore_test

import (
	"bytes"
	"testing"

	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
)

// A live document's snapshot is rewritten whole at every compaction and
// checkpoint. Postgres stores it uncompressed: compressing the snapshot of
// the 1.2 MB F1 manuscript took about 20 ms of each checkpoint's 44 ms,
// against the design's 30 ms budget (docs/design/storage-v2-results-store.md).
func TestLiveSnapshotsAreStoredUncompressed(t *testing.T) {
	e := newEnv(t, notestore.Options{})
	n := e.create("notes")
	const entry = "0123456789abcdef0123456789abcdef"
	seq, err := n.LiveAppend(e.ctx, entry, 0, []byte("u1"), "alice")
	if err != nil {
		t.Fatal(err)
	}
	snapshot := bytes.Repeat([]byte("Call me Ishmael. Some years ago, never mind how long precisely. "), 1<<14)
	if err := n.LiveCompact(e.ctx, entry, 0, snapshot, seq, gitobj.BlobID([]byte("base"))); err != nil {
		t.Fatal(err)
	}
	var method *string
	if err := e.db.Pool.QueryRow(e.ctx, `SELECT pg_column_compression(snapshot) FROM live_docs WHERE org = $1 AND notebook = 'notes' AND entry_id = $2`, e.db.Org, entry).Scan(&method); err != nil {
		t.Fatal(err)
	}
	if method != nil {
		t.Fatalf("the snapshot is stored with %s compression", *method)
	}
	if st, err := n.LiveState(e.ctx, entry); err != nil || !bytes.Equal(st.Snapshot, snapshot) {
		t.Fatal("the snapshot did not come back whole:", len(st.Snapshot), err)
	}
}
