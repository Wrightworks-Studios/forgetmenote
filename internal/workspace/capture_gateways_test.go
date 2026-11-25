package workspace

import (
	"context"
	"errors"
	"io/fs"
	"testing"
	"time"

	"forgetmenote/internal/pg/pgtest"
)

type abortOutcome struct {
	receipt CaptureReceipt
	err     error
}

// filedOnce fails unless the note is in exactly one notebook once the
// filing settles as the receive and the abort reported it.
func filedOnce(t *testing.T, source, target *Store, in CaptureMove, received bool) {
	t.Helper()
	ctx := captureContext()
	if received {
		if _, err := source.CaptureFinish(ctx, in); err != nil {
			t.Fatal("the source cannot finish a filing its destination received:", err)
		}
	} else if r, err := source.CaptureCancel(ctx, in); err != nil || r.State != "canceled" {
		t.Fatalf("cancel: %+v %v", r, err)
	}
	_, errSource := source.Read(context.Background(), in.From, "")
	_, errTarget := target.Read(context.Background(), in.To, "")
	for _, err := range []error{errSource, errTarget} {
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatal(err)
		}
	}
	if (errSource == nil) == received || (errTarget == nil) != received {
		t.Fatalf("filing settled as received=%v; the source has the note: %v; the target has it: %v", received, errSource == nil, errTarget == nil)
	}
}

// An abort on one gateway while another gateway's receive commits waits for
// that commit and reports it received, so the source is never released
// while the destination gains the note.
func TestAbortWhileAnotherGatewaysReceiveCommitsReportsTheReceive(t *testing.T) {
	ctx := captureContext()
	source, target, in, content := filingStores(t)
	if _, err := source.CapturePrepare(ctx, in); err != nil {
		t.Fatal(err)
	}
	other := captureGateway(t, target)
	waiting, release := pgtest.HoldWrites(t, testEnv(t, target).db, "revisions")
	received := make(chan error, 1)
	go func() {
		_, err := target.CaptureReceive(ctx, in, content)
		received <- err
	}()
	waiting(1)
	aborted := make(chan abortOutcome, 1)
	go func() {
		r, err := other.CaptureAbortReceive(ctx, in)
		aborted <- abortOutcome{r, err}
	}()
	select {
	case got := <-aborted:
		t.Errorf("the abort settled while the receive was committing: %+v", got)
		aborted <- got
	case <-time.After(300 * time.Millisecond):
	}
	release()
	errReceive, abort := <-received, <-aborted
	switch {
	case errReceive == nil && abort.err == nil && abort.receipt.State == "received":
		filedOnce(t, source, target, in, true)
	case errors.Is(errReceive, ErrConflict) && abort.err == nil && abort.receipt.State == "canceled":
		filedOnce(t, source, target, in, false)
	default:
		t.Fatalf("receive: %v; abort: %+v", errReceive, abort)
	}
}

// An abort that settles a filing between another gateway's check of the
// receive and its commit makes the receive fail, and its canceled receipt
// stands.
func TestReceiveAfterAnotherGatewaysAbortFails(t *testing.T) {
	ctx := captureContext()
	source, target, in, content := filingStores(t)
	if _, err := source.CapturePrepare(ctx, in); err != nil {
		t.Fatal(err)
	}
	other := captureGateway(t, target)
	waiting, release := pgtest.HoldWrites(t, testEnv(t, target).db, "operation_receipts")
	received := make(chan error, 1)
	go func() {
		_, err := target.CaptureReceive(ctx, in, content)
		received <- err
	}()
	waiting(1)
	aborted := make(chan abortOutcome, 1)
	go func() {
		r, err := other.CaptureAbortReceive(ctx, in)
		aborted <- abortOutcome{r, err}
	}()
	waiting(2)
	release()
	errReceive, abort := <-received, <-aborted
	if !errors.Is(errReceive, ErrConflict) || abort.err != nil || abort.receipt.State != "canceled" {
		t.Fatalf("receive: %v; abort: %+v", errReceive, abort)
	}
	stored, found, err := target.backend.(*dbNotebook).storedCapture(ctx, captureKey("capture-user", "receive", in.ID))
	if err != nil || !found || stored.State != "canceled" {
		t.Fatalf("the receive's receipt: %+v %v %v", stored, found, err)
	}
	filedOnce(t, source, target, in, false)
}
