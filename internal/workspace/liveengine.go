package workspace

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/reearth/ygo/crdt"
)

// Live editing keeps each note's Markdown as one text in a CRDT document.
// liveEngine and liveDoc are the boundary to the CRDT library (storage v2
// design §9), so live storage and the methods above it never handle the
// library's types. The engine is Yjs, through github.com/reearth/ygo, by
// decision D1; browsers keep the same document with the Yjs library
// (frontend/collaboration.mjs) and exchange its V1 updates.
//
// Two deviations from the design's sketch: Apply takes no limits, because
// the caller bounds an update's size before it checks the operation ID, as
// the Git backend does, and the text and state after it, which a patch
// changes too; and there is no Attribution, because with Yjs blame comes
// from revision diffs (D1).
type liveEngine interface {
	// New starts a document holding text.
	New(text string) liveDoc
	// Load restores a document from a snapshot and the updates this server
	// accepted after it.
	Load(snapshot []byte, updates [][]byte) (liveDoc, error)
}

type liveDoc interface {
	// Apply integrates a client's update. It refuses one that is not plain
	// Markdown text, is malformed or needs updates the document lacks, with
	// an error wrapping ErrLiveOperationRejected (and errLiveMissing for the
	// last); the document may then hold part of it, so the caller discards
	// the document.
	Apply(update []byte) error
	// Patch replaces UTF-16 ranges of the text at base, a Version, mapped
	// onto the current text, and returns the update it made. It fails with
	// an error wrapping ErrConflict when a target or an insertion gap was
	// changed since base, and then changes nothing.
	Patch(base []byte, edits []TextEdit) ([]byte, error)
	Text() string
	// Version names the current state as a patch base.
	Version() []byte
	// EncodeSince returns what a document at version lacks.
	EncodeSince(version []byte) ([]byte, error)
	// Snapshot encodes the whole document, as Load and browsers take it.
	Snapshot() []byte
}

var yjsLive liveEngine = yjsEngine{}

type yjsEngine struct{}

// yjsMaxPending bounds the items a load parks while their origins are
// missing. ygo decodes one client at a time, so a state with many clients
// parks every item whose origin belongs to a later one: the 1,024 the Git
// backend allows cannot load F1 after its edit history (D1 finding 4).
// Update and state size limits already bound the work.
const yjsMaxPending = 1 << 20

const yjsText = "markdown"

func newYjsDoc() *crdt.Doc {
	doc := crdt.New(crdt.WithGC(false), crdt.WithMaxPendingItems(yjsMaxPending))
	doc.GetText(yjsText)
	return doc
}

func (yjsEngine) New(text string) liveDoc {
	doc := newYjsDoc()
	doc.Transact(func(tx *crdt.Transaction) { tx.GetText(yjsText).Insert(tx, 0, text, nil) })
	return &yjsDoc{doc}
}

func (yjsEngine) Load(snapshot []byte, updates [][]byte) (liveDoc, error) {
	doc := newYjsDoc()
	for _, update := range append([][]byte{snapshot}, updates...) {
		if len(update) == 0 {
			continue
		}
		if err := crdt.ApplyUpdateV1(doc, update, nil); err != nil {
			return nil, fmt.Errorf("invalid stored live document: %w", err)
		}
	}
	if stats := doc.PendingStats(); stats.Items > 0 || stats.DeleteRanges > 0 {
		return nil, errors.New("invalid stored live document: updates are missing")
	}
	return &yjsDoc{doc}, nil
}

type yjsDoc struct{ y *crdt.Doc }

func (d *yjsDoc) Text() string { return d.y.GetText(yjsText).ToString() }

// Version is the encoded Yjs snapshot, the state vector plus the delete set:
// deletions do not advance a state vector, so it alone cannot name a version.
func (d *yjsDoc) Version() []byte { return crdt.EncodeSnapshot(crdt.CaptureSnapshot(d.y)) }

func (d *yjsDoc) EncodeSince(version []byte) ([]byte, error) {
	snapshot, err := crdt.DecodeSnapshot(version)
	if err != nil {
		return nil, err
	}
	return crdt.EncodeStateAsUpdateV1(d.y, snapshot.StateVector), nil
}

func (d *yjsDoc) Snapshot() []byte { return crdt.EncodeStateAsUpdateV1(d.y, nil) }

func (d *yjsDoc) Apply(update []byte) error {
	if err := validateMarkdownUpdate(update); err != nil {
		return fmt.Errorf("%w: %v", ErrLiveOperationRejected, err)
	}
	if err := crdt.ApplyUpdateV1(d.y, update, nil); err != nil {
		return fmt.Errorf("%w: invalid Yjs update", ErrLiveOperationRejected)
	}
	if stats := d.y.PendingStats(); stats.Items > 0 || stats.DeleteRanges > 0 {
		return fmt.Errorf("%w: %w", ErrLiveOperationRejected, errLiveMissing)
	}
	return nil
}

// Patch is the Git backend's targeted patch: each range of the base text is
// resolved through relative positions into the current text, and must still
// be the same original items.
func (d *yjsDoc) Patch(base []byte, in []TextEdit) ([]byte, error) {
	snapshot, err := crdt.DecodeSnapshot(base)
	if err != nil {
		return nil, err
	}
	at, err := liveSnapshotBase(d.y, snapshot)
	if err != nil {
		return nil, err
	}
	defer at.Destroy()
	text := at.GetText(yjsText)
	current := d.Text()
	deleted := crdt.CaptureSnapshot(d.y).DeleteSet
	// Retain only items that existed at the requested base, with today's deletes.
	// Comparing lengths in this view proves every original target item survives;
	// newly inserted identical text cannot fill a deleted interior and pass.
	survivingDeletes := crdt.CaptureSnapshot(at).DeleteSet
	survivingDeletes.Merge(deleted)
	survivingBase, err := crdt.CreateDocFromSnapshot(at, &crdt.Snapshot{StateVector: snapshot.StateVector, DeleteSet: survivingDeletes})
	if err != nil {
		return nil, err
	}
	defer survivingBase.Destroy()
	before := text.ToString()
	edits := append([]TextEdit{}, in...)
	sort.Slice(edits, func(i, j int) bool { return edits[i].Start < edits[j].Start })
	last := -1
	for i := range edits {
		e := &edits[i]
		old, ok := utf16Slice(before, e.Start, e.End)
		if !ok || old != e.Old || e.Start <= last || !utf8.ValidString(e.Text) || strings.ContainsRune(e.Text, 0) {
			return nil, fmt.Errorf("patch ranges or expected text do not match: %w", ErrConflict)
		}
		last = e.End - 1
		a := crdt.CreateRelativePositionFromIndex(text, e.Start, 0)
		b := crdt.CreateRelativePositionFromIndex(text, e.End, -1)
		start, okA := crdt.ToAbsolutePosition(d.y, a)
		end, okB := crdt.ToAbsolutePosition(d.y, b)
		if e.Start == e.End {
			// Relative positions also resolve deleted items. A zero-width patch
			// needs the original neighboring items to remain alive, otherwise its
			// empty old-text guard would accept an overwritten insertion gap.
			if a.Item != nil && deleted.IsDeleted(*a.Item) || b.Item != nil && deleted.IsDeleted(*b.Item) {
				return nil, fmt.Errorf("the insertion gap was overwritten; reconcile the live note: %w", ErrConflict)
			}
			end = start
			okB = okA
		} else {
			survivingStart, aliveStart := crdt.ToAbsolutePosition(survivingBase, a)
			survivingEnd, aliveEnd := crdt.ToAbsolutePosition(survivingBase, b)
			if a.Item == nil || b.Item == nil || deleted.IsDeleted(*a.Item) || deleted.IsDeleted(*b.Item) || !aliveStart || !aliveEnd || survivingEnd.Index-survivingStart.Index != e.End-e.Start {
				return nil, fmt.Errorf("another editor deleted or replaced the original target; reconcile the live note: %w", ErrConflict)
			}
		}
		actual, ok := utf16Slice(current, start.Index, end.Index)
		if !okA || !okB || !ok || actual != e.Old {
			return nil, fmt.Errorf("another editor changed the target; reconcile or submit a review proposal: %w", ErrConflict)
		}
		e.Start = start.Index
		e.End = end.Index
	}
	for i := 1; i < len(edits); i++ {
		if edits[i].Start < edits[i-1].End {
			return nil, ErrConflict
		}
	}
	var update []byte
	stop := d.y.OnUpdate(func(u []byte, _ any) { update = append(update[:0], u...) })
	defer stop()
	d.y.Transact(func(tx *crdt.Transaction) {
		t := tx.GetText(yjsText)
		for i := len(edits) - 1; i >= 0; i-- {
			e := edits[i]
			if e.End > e.Start {
				t.Delete(tx, e.Start, e.End-e.Start)
			}
			if e.Text != "" {
				t.Insert(tx, e.Start, e.Text, nil)
			}
		}
	})
	return update, nil
}
