package workspace

import (
	"context"
	"strings"
	"testing"
	"time"

	"forgetmenote/internal/notestore"
)

// Finished filings' receipts, kept until the sweep, are never read again:
// one no capture record could be does not stop a new filing.
func TestFinishedCaptureReceiptsAreNotRead(t *testing.T) {
	ctx := captureContext()
	source, _, in, _ := filingStores(t)
	finished := notestore.Receipt{Kind: captureReceiptKind, Operation: ContentHash([]byte("an old filing")), RequestHash: ContentHash([]byte("its request")), State: notestore.ReceiptDone, Result: []byte(strings.Repeat("x", 40000))}
	if err := source.notes.PutReceipt(ctx, finished); err != nil {
		t.Fatal(err)
	}
	saveNote(t, source, "other.md", "# Other\n")
	if _, err := source.CapturePrepare(ctx, in); err != nil {
		t.Fatal("a finished filing's receipt stopped a new one:", err)
	}
}

// Every write checks the reservations of filings in flight, so it reads
// only theirs, however many finished filings' receipts the notebook keeps.
func TestWritesReadOnlyTheReceiptsOfFilingsInFlight(t *testing.T) {
	ctx := captureContext()
	source, _, in, _ := filingStores(t)
	timeSaves := func() time.Duration {
		start := time.Now()
		for i := range 10 {
			saveNote(t, source, "timed.md", strings.Repeat("x", i+1))
		}
		return time.Since(start) / 10
	}
	before := timeSaves()
	db := testEnv(t, source).db
	result := []byte(strings.Repeat(`{"version":1}`, 30))
	if _, err := db.Pool.Exec(context.Background(), `INSERT INTO operation_receipts (org, notebook, kind, operation_id, request_hash, state, result, done)
		SELECT $1, 'notes', 'capture', 'finished-' || g, 'request', 'done', $2, now() FROM generate_series(1, 5000) AS g`, db.Org, result); err != nil {
		t.Fatal(err)
	}
	if _, err := source.CapturePrepare(ctx, in); err != nil {
		t.Fatal(err)
	}
	n := source.backend
	n.mu.Lock()
	records, err := n.captureReservations(ctx)
	n.mu.Unlock()
	if err != nil || len(records) != 1 {
		t.Fatalf("reservations: %d %v", len(records), err)
	}
	after := timeSaves()
	t.Logf("Save took %v with no finished filings' receipts and %v with 5000", before, after)
}
