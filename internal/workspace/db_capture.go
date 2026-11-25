package workspace

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"

	"forgetmenote/internal/attribution"
	"forgetmenote/internal/notestore"
)

// Group E: Quicknote capture and cross-notebook filing for notebooks kept in
// the notes store. Each method answers what its Git original answers. The
// capture records the Git backend kept as files under <data>/captures are
// notes store receipts of kind "capture", so every gateway serving the
// notebook sees the same reservations, and a restart keeps them.
//
// A record in a final state is a done receipt, which the notes store sweeps
// ReceiptTTL after it finished; only records still in flight count towards
// the capacity, so finished filings never stop new ones. Once a finished
// record is swept, a retry is answered from the revision its operation
// committed, which the Git backend consults for every retry too.

// captureReceiptKind is the notes store's receipt kind for capture records.
const captureReceiptKind = "capture"

// maxPendingCaptures bounds the records of filings still in flight, as the
// Git backend bounded its record files; tests lower it.
var maxPendingCaptures = 20000

// captureFinal reports whether a record's state is final: no retry puts it
// back in flight, so its receipt is done.
func captureFinal(state string) bool {
	return state == "received" || state == "done" || state == "canceled"
}

// parseCaptureReceipt reads a capture record from its receipt with the
// checks the Git backend applies to a record file it opens.
func (n *dbNotebook) parseCaptureReceipt(receipt notestore.Receipt) (captureRecord, error) {
	if len(receipt.Result) > 32768 {
		return captureRecord{}, errors.New("invalid capture receipt size")
	}
	var r captureRecord
	decoder := json.NewDecoder(bytes.NewReader(receipt.Result))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&r) != nil || r.Version != 1 || r.Actor == "" || !validCaptureMove(r.Request) || (r.Kind != "send" && r.Kind != "receive" && r.Kind != "move") {
		return captureRecord{}, errors.New("invalid capture receipt")
	}
	switch r.State {
	case "prepared", "receiving", "moving", "received", "done", "canceled":
	default:
		return captureRecord{}, errors.New("invalid capture receipt state")
	}
	if r.State == "received" || r.State == "done" {
		if !revisionPattern.MatchString(r.Revision) {
			return captureRecord{}, errors.New("invalid committed capture receipt")
		}
	}
	if (receipt.State == notestore.ReceiptDone) != captureFinal(r.State) {
		return captureRecord{}, errors.New("invalid capture receipt state")
	}
	if receipt.Operation != captureKey(r.Actor, r.Kind, r.Request.ID) || receipt.RequestHash != captureOperation(r).Request {
		return captureRecord{}, errors.New("invalid capture receipt identity")
	}
	if _, err := n.resolve(r.Request.From, false); err != nil {
		return captureRecord{}, err
	}
	if _, err := n.resolve(r.Request.To, false); err != nil {
		return captureRecord{}, err
	}
	return r, nil
}

// captureReservations reads the records of filings in flight, by key, the
// only ones that reserve paths; every write checks them, so the finished
// ones, kept until the sweep, are not read. Callers hold mu.
func (n *dbNotebook) captureReservations(ctx context.Context) (map[string]captureRecord, error) {
	receipts, err := n.notes.PendingReceipts(ctx, captureReceiptKind)
	if err != nil {
		return nil, err
	}
	records := map[string]captureRecord{}
	for _, receipt := range receipts {
		r, err := n.parseCaptureReceipt(receipt)
		if err != nil {
			return nil, err
		}
		records[receipt.Operation] = r
	}
	return records, nil
}

// storedCapture reads the record under key, if there is one. Callers hold mu.
func (n *dbNotebook) storedCapture(ctx context.Context, key string) (captureRecord, bool, error) {
	receipt, found, err := n.notes.Receipt(ctx, captureReceiptKind, key)
	if err != nil || !found {
		return captureRecord{}, false, err
	}
	r, err := n.parseCaptureReceipt(receipt)
	if err != nil {
		return captureRecord{}, false, err
	}
	return r, true, nil
}

// captureReceipt is r's receipt: done once r is final, and pending while
// the filing is in flight.
func captureReceipt(r captureRecord) (notestore.Receipt, error) {
	raw, err := json.Marshal(r)
	if err != nil {
		return notestore.Receipt{}, err
	}
	state := notestore.ReceiptPending
	if captureFinal(r.State) {
		state = notestore.ReceiptDone
	}
	op := captureOperation(r)
	return notestore.Receipt{Kind: captureReceiptKind, Operation: op.ID, RequestHash: op.Request, State: state, Result: raw}, nil
}

// persistCapture stores r as its receipt. A done receipt never becomes
// pending again. A new record of a filing in flight is refused while
// maxPendingCaptures others are, and once the notebook's authority record
// changed since checkAuthority read it: it is stored under the lock that
// orders it against a handoff's preparation (PrepareAuthority), so either
// the handoff sees the filing in flight and is refused, or the filing sees
// the handoff and is. A final record, such as the fence of a receive, is
// always recorded. Callers hold mu and checked authority.
func (n *dbNotebook) persistCapture(ctx context.Context, r captureRecord) error {
	receipt, err := captureReceipt(r)
	if err != nil {
		return err
	}
	if receipt.State == notestore.ReceiptPending {
		_, exists, err := n.notes.Receipt(ctx, captureReceiptKind, receipt.Operation)
		if err != nil {
			return err
		}
		if !exists {
			return n.reserveCapture(ctx, receipt)
		}
	}
	err = n.notes.PutReceipt(ctx, receipt)
	if errors.Is(err, notestore.ErrOperationMismatch) {
		return ErrConflict
	}
	return err
}

// reserveCapture stores the receipt of a new filing in flight. Callers hold
// mu and checked authority.
func (n *dbNotebook) reserveCapture(ctx context.Context, receipt notestore.Receipt) error {
	pending, err := n.notes.PendingReceipts(ctx, captureReceiptKind)
	if err != nil {
		return err
	}
	if len(pending) >= maxPendingCaptures {
		return capacityReached("capture receipt capacity reached")
	}
	checked := n.authorityVersion
	err = n.notes.SettleReceipt(ctx, "", func(s notestore.Settlement) (notestore.Receipt, error) {
		if s.Authority != checked {
			return notestore.Receipt{}, notestore.ErrAuthorityChanged
		}
		return receipt, nil
	})
	switch {
	case errors.Is(err, notestore.ErrAuthorityChanged):
		return n.authorityRefusal(ctx, err)
	case errors.Is(err, notestore.ErrOperationMismatch):
		return ErrConflict
	}
	return err
}

// settleCapture records how r's filing ended, in one transaction with the
// notebook's head row locked: committed, as committedState, when its
// operation committed a revision, and canceled otherwise. A receive or
// filing committing on another gateway (applyChanges) either landed first,
// and is the outcome, or finds its receipt settled and fails, so an abort
// never releases a note a commit also files. Callers hold mu.
func (n *dbNotebook) settleCapture(ctx context.Context, r captureRecord, committedState string) (CaptureReceipt, error) {
	op := captureOperation(r)
	err := n.notes.SettleReceipt(ctx, op.ID, func(s notestore.Settlement) (notestore.Receipt, error) {
		if s.Committed != nil {
			if trailerValue(s.Committed.Trailers, notestore.RequestTrailer) != op.Request {
				return notestore.Receipt{}, ErrConflict
			}
			r.State, r.Revision = committedState, s.Committed.ID.String()
		} else {
			r.State = "canceled"
		}
		return captureReceipt(r)
	})
	if errors.Is(err, notestore.ErrOperationMismatch) {
		return CaptureReceipt{}, ErrConflict
	}
	if err != nil {
		return CaptureReceipt{}, err
	}
	return r.CaptureReceipt, nil
}

// captureRecord validates a transfer intent and finds its record, or starts
// one. Callers hold mu.
func (n *dbNotebook) captureRecord(ctx context.Context, in CaptureMove, kind string) (captureRecord, bool, error) {
	actor := attribution.From(ctx).ID
	if actor == "" || !validCaptureMove(in) {
		return captureRecord{}, false, ErrForbidden
	}
	if err := n.resolveAtHead(ctx, in.From); err != nil {
		return captureRecord{}, false, err
	}
	if err := n.resolveAtHead(ctx, in.To); err != nil {
		return captureRecord{}, false, err
	}
	r, exists, err := n.storedCapture(ctx, captureKey(actor, kind, in.ID))
	if err != nil {
		return captureRecord{}, false, err
	}
	if exists && r.Request != in {
		return captureRecord{}, false, ErrConflict
	}
	if !exists {
		r = captureRecord{Version: 1, Actor: actor, Kind: kind, Request: in}
	}
	return r, exists, nil
}

// captureCommitted finds the revision r's operation committed, as
// "git log -1 --grep=Gitnotes-Operation: <id>" finds it, and refuses one
// made for another request.
func (n *dbNotebook) captureCommitted(ctx context.Context, r captureRecord) (string, error) {
	op := captureOperation(r)
	info, ok, err := n.operationCommit(ctx, op.ID)
	if err != nil || !ok {
		return "", err
	}
	if trailerValue(info.Trailers, notestore.RequestTrailer) != op.Request {
		return "", ErrConflict
	}
	return info.ID.String(), nil
}

// checkCaptureChanges refuses changes to a note a filing reserves, to its
// discussion, and to links that name it, whichever gateway reserved it. It
// runs under mu for every content write, through checkMutationChanges.
func (n *dbNotebook) checkCaptureChanges(ctx context.Context, changes map[string][]byte) error {
	if len(changes) == 0 {
		return nil
	}
	records, err := n.captureReservations(ctx)
	if err != nil {
		return err
	}
	allowed, _ := ctx.Value(captureMutationKey{}).(string)
	for key, r := range records {
		if key == allowed {
			continue
		}
		for _, reserved := range captureReservedPaths(r) {
			for path, after := range changes {
				if path == reserved || path == commentPath(reserved) {
					return fmt.Errorf("note is being filed; retry after recovery: %w", ErrConflict)
				}
				if after != nil && strings.HasSuffix(path, ".md") && !bytes.Equal(after, rewriteLinks(after, path, path, reserved, "quicknotes-reference-guard.md")) {
					return fmt.Errorf("referenced note is being filed: %w", ErrConflict)
				}
			}
		}
	}
	return nil
}

// checkCaptureLock refuses a lock rule that covers a note a filing
// reserves. It reads the store, so it takes the
// request's context. Callers hold mu.
func (n *dbNotebook) checkCaptureLock(ctx context.Context, path string) error {
	records, err := n.captureReservations(ctx)
	if err != nil {
		return err
	}
	for _, r := range records {
		for _, reserved := range captureReservedPaths(r) {
			if matchesPrefix(reserved, []string{path}) {
				return ErrConflict
			}
		}
	}
	return nil
}

// captureSource checks that the note to file is still exactly the reserved
// version at head, and that filing it breaks nothing. Callers hold mu.
func (n *dbNotebook) captureSource(ctx context.Context, in CaptureMove, head string) (Document, error) {
	visible, err := n.visibleAtHead(ctx, in.From)
	if err != nil {
		return Document{}, err
	}
	if !visible || !n.editable(in.From) {
		return Document{}, ErrForbidden
	}
	doc, err := n.read(ctx, in.From, head)
	if err != nil {
		return Document{}, err
	}
	if doc.Version != in.Version || ContentHash(doc.Content) != in.Hash {
		return Document{}, ErrConflict
	}
	if err = capturePortable(doc.Content); err != nil {
		return Document{}, err
	}
	if _, err = n.read(ctx, commentPath(in.From), doc.Revision); err == nil {
		return Document{}, fmt.Errorf("%w: filing cannot move a note with a discussion", ErrCaptureInput)
	} else if !errors.Is(err, os.ErrNotExist) {
		return Document{}, err
	}
	err = n.forEachCommittedContent(ctx, doc.Revision, func(path string) bool {
		return path != in.From && strings.HasSuffix(path, ".md")
	}, func(path string, content []byte) error {
		if !bytes.Equal(content, rewriteLinks(content, path, path, in.From, "quicknotes-reference-guard.md")) {
			return fmt.Errorf("%w: filing cannot rewrite links from other notes", ErrCaptureInput)
		}
		return nil
	})
	if err != nil {
		return Document{}, err
	}
	return doc, nil
}

// captureDestination checks that a filed note can be written at p in head:
// a visible, editable note path in an existing folder, with neither a file
// nor a discussion there yet. Callers hold mu.
func (n *dbNotebook) captureDestination(ctx context.Context, p, head string) error {
	if !n.visibleNote(p) || p == "AGENTS.md" || IsTrash(p) {
		return ErrForbidden
	}
	if _, err := n.resolve(p, true); err != nil {
		return err
	}
	if err := n.resolveAtHead(ctx, p); err != nil {
		return err
	}
	if parent := path.Dir(p); parent != "." {
		file, err := n.exists(ctx, head, parent)
		if err != nil {
			return err
		}
		if file {
			return ErrForbidden
		}
		dir, err := n.isDir(ctx, head, parent)
		if err != nil {
			return err
		}
		if !dir {
			return fmt.Errorf("%w: choose an existing destination folder", ErrCaptureInput)
		}
	}
	for _, taken := range []func() (bool, error){
		func() (bool, error) { return n.exists(ctx, head, p) },
		func() (bool, error) { return n.isDir(ctx, head, p) },
		func() (bool, error) { return n.exists(ctx, head, commentPath(p)) },
	} {
		occupied, err := taken()
		if err != nil {
			return err
		}
		if occupied {
			return ErrConflict
		}
	}
	return nil
}

func (n *dbNotebook) CapturePrepare(ctx context.Context, in CaptureMove) (Document, error) {
	ctx = n.mutationContext(ctx)
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := n.checkAuthority(ctx); err != nil {
		return Document{}, err
	}
	r, exists, err := n.captureRecord(ctx, in, "send")
	if err != nil {
		return Document{}, err
	}
	if exists {
		if r.State != "prepared" && r.State != "done" {
			return Document{}, ErrConflict
		}
		if _, err = n.captureCommitted(ctx, r); err != nil {
			return Document{}, err
		}
		return n.read(ctx, in.From, in.Version)
	}
	// A send that finished, whose done record was swept, replays as that
	// record would.
	if committed, err := n.captureCommitted(ctx, r); err != nil {
		return Document{}, err
	} else if committed != "" {
		return n.read(ctx, in.From, in.Version)
	}
	if err = n.loadLocks(ctx); err != nil {
		return Document{}, err
	}
	if err = n.clean(ctx); err != nil {
		return Document{}, err
	}
	head, err := n.mutateBase(ctx)
	if err != nil {
		return Document{}, err
	}
	doc, err := n.captureSource(ctx, in, head)
	if err != nil {
		return Document{}, err
	}
	if err = n.checkCaptureChanges(ctx, map[string][]byte{in.From: doc.Content}); err != nil {
		return Document{}, err
	}
	r.State = "prepared"
	if err = n.persistCapture(ctx, r); err != nil {
		return Document{}, err
	}
	return doc, nil
}

func (n *dbNotebook) CaptureReceive(ctx context.Context, in CaptureMove, content []byte) (Document, error) {
	ctx = n.mutationContext(ctx)
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := n.checkAuthority(ctx); err != nil {
		return Document{}, err
	}
	r, _, err := n.captureRecord(ctx, in, "receive")
	if err != nil {
		return Document{}, err
	}
	if r.State == "canceled" {
		return Document{}, ErrConflict
	}
	if ContentHash(content) != in.Hash || len(content) > MaxLiveBytes {
		return Document{}, ErrConflict
	}
	if err = capturePortable(content); err != nil {
		return Document{}, err
	}
	committed, err := n.captureCommitted(ctx, r)
	if err != nil {
		return Document{}, err
	}
	if committed != "" {
		r.State = "received"
		r.Revision = committed
		if err = n.persistCapture(ctx, r); err != nil {
			return Document{}, err
		}
		return n.read(ctx, in.To, committed)
	}
	if r.State == "received" {
		return Document{}, ErrConflict
	}
	if err = n.loadLocks(ctx); err != nil {
		return Document{}, err
	}
	if err = n.clean(ctx); err != nil {
		return Document{}, err
	}
	head, err := n.mutateBase(ctx)
	if err != nil {
		return Document{}, err
	}
	if err = n.captureDestination(ctx, in.To, head); err != nil {
		return Document{}, err
	}
	if err = n.checkCaptureChanges(ctx, map[string][]byte{in.To: content}); err != nil && r.State != "receiving" {
		return Document{}, err
	}
	r.State = "receiving"
	if err = n.persistCapture(ctx, r); err != nil {
		return Document{}, err
	}
	result, err := n.applyChanges(n.captureContext(ctx, r), head, "File Quicknote", []pathChange{{Path: in.To, Content: content}})
	if err != nil {
		return Document{}, err
	}
	committed = result.Head.Commit.String()
	r.State = "received"
	r.Revision = committed
	if err = n.persistCapture(ctx, r); err != nil {
		return Document{}, err
	}
	return n.read(ctx, in.To, committed)
}

func (n *dbNotebook) CaptureFinish(ctx context.Context, in CaptureMove) (CaptureReceipt, error) {
	ctx = n.mutationContext(ctx)
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := n.checkAuthority(ctx); err != nil {
		return CaptureReceipt{}, err
	}
	r, exists, err := n.captureRecord(ctx, in, "send")
	if err != nil {
		return CaptureReceipt{}, err
	}
	if r.State == "canceled" {
		return CaptureReceipt{}, ErrConflict
	}
	// A send with no record is refused, unless it finished and its done
	// record was swept.
	committed, err := n.captureCommitted(ctx, r)
	if err != nil {
		return CaptureReceipt{}, err
	}
	if committed != "" {
		r.State = "done"
		r.Revision = committed
		if err = n.persistCapture(ctx, r); err != nil {
			return CaptureReceipt{}, err
		}
		return r.CaptureReceipt, nil
	}
	if !exists || r.State != "prepared" {
		return CaptureReceipt{}, ErrConflict
	}
	if err = n.loadLocks(ctx); err != nil {
		return CaptureReceipt{}, err
	}
	if err = n.clean(ctx); err != nil {
		return CaptureReceipt{}, err
	}
	head, err := n.mutateBase(ctx)
	if err != nil {
		return CaptureReceipt{}, err
	}
	if _, err = n.captureSource(ctx, in, head); err != nil {
		return CaptureReceipt{}, err
	}
	result, err := n.applyChanges(n.captureContext(ctx, r), head, "File Quicknote to another notebook", []pathChange{{Path: in.From}})
	if err != nil {
		return CaptureReceipt{}, err
	}
	r.State = "done"
	r.Revision = result.Head.Commit.String()
	if err = n.persistCapture(ctx, r); err != nil {
		return CaptureReceipt{}, err
	}
	return r.CaptureReceipt, nil
}

// CaptureAbortReceive fences a delayed receive before the source is
// released. A committed receive wins over cancellation, including one whose
// acknowledgement was lost.
func (n *dbNotebook) CaptureAbortReceive(ctx context.Context, in CaptureMove) (CaptureReceipt, error) {
	ctx = n.mutationContext(ctx)
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := n.checkAuthority(ctx); err != nil {
		return CaptureReceipt{}, err
	}
	r, _, err := n.captureRecord(ctx, in, "receive")
	if err != nil {
		return CaptureReceipt{}, err
	}
	return n.settleCapture(ctx, r, "received")
}

// CaptureCancel releases the source once the gateway has fenced the
// destination's receive; the source stays the note it was.
func (n *dbNotebook) CaptureCancel(ctx context.Context, in CaptureMove) (CaptureReceipt, error) {
	ctx = n.mutationContext(ctx)
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := n.checkAuthority(ctx); err != nil {
		return CaptureReceipt{}, err
	}
	r, _, err := n.captureRecord(ctx, in, "send")
	if err != nil {
		return CaptureReceipt{}, err
	}
	return n.settleCapture(ctx, r, "done")
}

// CaptureCreate saves a new Quicknote as an edit group, so a retry replays.
func (n *dbNotebook) CaptureCreate(ctx context.Context, id, path string, content []byte) (Document, error) {
	ctx = n.mutationContext(ctx)
	if attribution.From(ctx).ID == "" || id == "" || len(id) > 96 || len(content) > MaxLiveBytes || strings.Contains(path, "/") || !strings.HasPrefix(path, "quicknote-") || !strings.HasSuffix(path, ".md") {
		return Document{}, ErrForbidden
	}
	result, err := n.ApplyGroup(ctx, captureCreateGroup(id, path, content))
	if err != nil {
		return Document{}, err
	}
	return n.Read(ctx, path, result.Revision)
}

// CaptureSaved recovers a capture's acknowledgement from the revision that
// saved it, so the gateway keeps no plaintext copy of the note.
func (n *dbNotebook) CaptureSaved(ctx context.Context, id, path, hash string) (Document, error) {
	actor := attribution.From(ctx).ID
	if actor == "" || id == "" || len(id) > 96 || strings.Contains(path, "/") || !strings.HasPrefix(path, "quicknote-") || !strings.HasSuffix(path, ".md") {
		return Document{}, ErrForbidden
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	info, ok, err := n.operationCommit(ctx, ContentHash([]byte(actor+"\x00quicknote-create:"+id)))
	if err != nil {
		return Document{}, err
	}
	if !ok {
		return Document{}, os.ErrNotExist
	}
	if err = n.resolveAtHead(ctx, path); err != nil {
		return Document{}, err
	}
	doc, err := n.read(ctx, path, info.ID.String())
	if err != nil {
		return Document{}, err
	}
	if ContentHash(doc.Content) != hash {
		return Document{}, ErrConflict
	}
	if _, err = n.groupReceipt(ctx, captureCreateGroup(id, path, doc.Content), actor); err != nil {
		return Document{}, err
	}
	return doc, nil
}

// CapturePreflight checks the destination before the source is reserved;
// the receive repeats every check under mu.
func (n *dbNotebook) CapturePreflight(ctx context.Context, in CaptureMove) (CaptureReceipt, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	r, _, err := n.captureRecord(ctx, in, "receive")
	if err != nil {
		return CaptureReceipt{}, err
	}
	if r.State == "canceled" {
		return CaptureReceipt{}, ErrConflict
	}
	revision, err := n.captureCommitted(ctx, r)
	if err != nil {
		return CaptureReceipt{}, err
	}
	if revision != "" {
		return CaptureReceipt{State: "received", Revision: revision}, nil
	}
	head, err := n.mutateBase(ctx)
	if err != nil {
		return CaptureReceipt{}, err
	}
	if err = n.captureDestination(ctx, in.To, head); err != nil {
		return CaptureReceipt{}, err
	}
	if err = n.checkCaptureChanges(n.captureContext(ctx, r), map[string][]byte{in.To: []byte{}}); err != nil {
		return CaptureReceipt{}, err
	}
	return CaptureReceipt{State: "ready"}, nil
}
