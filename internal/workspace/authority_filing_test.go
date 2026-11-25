package workspace

import (
	"context"
	"errors"
	"strings"
	"testing"
)

const filingHandoff = "0123456789abcdef0123456789abcdef"

// prepareGitHubHandoff prepares s's first handoff to GitHub at its head.
func prepareGitHubHandoff(t *testing.T, s *Store) (AuthorityState, error) {
	t.Helper()
	return s.PrepareAuthority(context.Background(), AuthorityRequest{ID: filingHandoff, Generation: 1, From: "forgetmenote", To: "github", Revision: headOf(t, s)})
}

// refusedForFiling fails unless err refuses a handoff over a filing in
// flight, and s has no handoff prepared.
func refusedForFiling(t *testing.T, s *Store, what string, err error) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "finish pending cross-notebook moves") {
		t.Fatalf("%s: a handoff was prepared over a filing in flight: %v", what, err)
	}
	if state, err := s.AuthorityStatus(context.Background()); err != nil || state.Phase != "" {
		t.Fatalf("%s: a refused handoff left %+v %v", what, state, err)
	}
}

// filingStores is a source notebook with a note to file and a target
// notebook with the folder it is filed into.
func filingStores(t *testing.T) (source, target *Store, in CaptureMove, content []byte) {
	t.Helper()
	source, target = newTestStore(t), newTestStore(t)
	doc := saveNote(t, source, "capture.md", "# A capture\n")
	saveNote(t, target, "notes/existing.md", "existing")
	return source, target, captureMove(doc), doc.Content
}

// A filing in flight keeps a handoff off its source notebook after a
// restart, on every gateway, until it finishes; a finished filing, whose
// receipts are kept until the sweep, does not.
func TestPendingFilingRefusesHandoffAfterRestartAndOnEveryGateway(t *testing.T) {
	ctx := captureContext()
	source, target, in, content := filingStores(t)
	if _, err := source.CapturePrepare(ctx, in); err != nil {
		t.Fatal(err)
	}
	restarted := reopenTestStore(t, source)
	_, err := prepareGitHubHandoff(t, restarted)
	refusedForFiling(t, restarted, "restarted source", err)
	other := captureGateway(t, restarted)
	_, err = prepareGitHubHandoff(t, other)
	refusedForFiling(t, other, "another gateway", err)
	// The destination received; the source's send is still in flight.
	if _, err = target.CaptureReceive(ctx, in, content); err != nil {
		t.Fatal(err)
	}
	again := reopenTestStore(t, restarted)
	_, err = prepareGitHubHandoff(t, again)
	refusedForFiling(t, again, "source after the destination received", err)
	if _, err = other.CaptureFinish(ctx, in); err != nil {
		t.Fatal(err)
	}
	// The gateway that finished the filing knows the head it committed.
	for name, s := range map[string]*Store{"source": other, "target": target} {
		if state, err := prepareGitHubHandoff(t, s); err != nil || state.Phase != "prepared" {
			t.Fatalf("%s after a finished filing: %+v %v", name, state, err)
		}
	}
}

// A receive that failed after it reserved its destination, as one a crash
// cut short, keeps a handoff off the destination after a restart; once the
// filing is aborted, the handoff goes ahead.
func TestPendingReceiveRefusesHandoffOfItsDestination(t *testing.T) {
	ctx := captureContext()
	source, target, in, content := filingStores(t)
	if _, err := source.CapturePrepare(ctx, in); err != nil {
		t.Fatal(err)
	}
	release := failCommits(t, target)
	if _, err := target.CaptureReceive(ctx, in, content); err == nil {
		t.Fatal("a receive whose commit failed succeeded")
	}
	release()
	restarted := reopenTestStore(t, target)
	_, err := prepareGitHubHandoff(t, restarted)
	refusedForFiling(t, restarted, "restarted destination", err)
	if r, err := restarted.CaptureAbortReceive(ctx, in); err != nil || r.State != "canceled" {
		t.Fatalf("abort: %+v %v", r, err)
	}
	if r, err := source.CaptureCancel(ctx, in); err != nil || r.State != "canceled" {
		t.Fatalf("cancel: %+v %v", r, err)
	}
	for name, s := range map[string]*Store{"source": source, "target": restarted} {
		if state, err := prepareGitHubHandoff(t, s); err != nil || state.Phase != "prepared" {
			t.Fatalf("%s after an aborted filing: %+v %v", name, state, err)
		}
	}
}

// A filing that another gateway checked before a handoff was prepared, and
// reserves after, is refused, so no filing is ever stranded in a notebook
// handed to GitHub.
func TestFilingCheckedBeforeAHandoffCannotReserveAfterIt(t *testing.T) {
	ctx := captureContext()
	source, _, in, _ := filingStores(t)
	other := captureGateway(t, source)
	n := other.backend.(*dbNotebook)
	n.mu.Lock()
	defer n.mu.Unlock()
	mctx := n.mutationContext(ctx)
	if err := n.checkAuthority(mctx); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareGitHubHandoff(t, source); err != nil {
		t.Fatal(err)
	}
	r := captureRecord{Version: 1, Actor: "capture-user", Kind: "send", Request: in, CaptureReceipt: CaptureReceipt{State: "prepared"}}
	if err := n.persistCapture(mctx, r); !errors.Is(err, ErrForbidden) {
		t.Fatal("a filing reserved its note in a notebook being handed off:", err)
	}
	if _, found, err := source.notes.Receipt(ctx, captureReceiptKind, captureKey(r.Actor, r.Kind, in.ID)); err != nil || found {
		t.Fatalf("the refused reservation left a receipt: %v %v", found, err)
	}
}
