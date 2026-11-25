package workspace

import (
	"reflect"
	"strings"
	"testing"

	"github.com/reearth/ygo/encoding"
)

const wireSafeInteger = uint64(1<<53 - 1)

func wireTextClients(ids ...uint64) []byte {
	w := encoding.NewEncoder()
	w.WriteVarUint(uint64(len(ids)))
	for i, id := range ids {
		w.WriteVarUint(1)
		w.WriteVarUint(id)
		w.WriteVarUint(0)
		w.WriteUint8(4)
		w.WriteVarUint(1)
		w.WriteVarString("markdown")
		w.WriteVarString(string(rune('X' + i)))
	}
	w.WriteVarUint(0)
	return w.Bytes()
}

func wireClockRange(clock, prefix uint64, content string, deleted uint64) []byte {
	w := encoding.NewEncoder()
	w.WriteVarUint(1)
	count := uint64(1)
	if prefix > 0 {
		count++
	}
	w.WriteVarUint(count)
	w.WriteVarUint(42)
	w.WriteVarUint(clock)
	if prefix > 0 {
		w.WriteUint8(0)
		w.WriteVarUint(prefix)
	}
	if deleted > 0 {
		w.WriteUint8(1)
	} else {
		w.WriteUint8(4)
	}
	w.WriteVarUint(1)
	w.WriteVarString("markdown")
	if deleted > 0 {
		w.WriteVarUint(deleted)
	} else {
		w.WriteVarString(content)
	}
	w.WriteVarUint(0)
	return w.Bytes()
}

func TestLiveRejectsUnsafeWireIntegersAndRangesWithoutReceipts(t *testing.T) {
	deleteRange := func(clock, length uint64) []byte {
		w := encoding.NewEncoder()
		for _, n := range []uint64{0, 1, 42, 1, clock, length} {
			w.WriteVarUint(n)
		}
		return w.Bytes()
	}
	for name, raw := range map[string][]byte{
		"colliding-client-identifiers": wireTextClients(1<<53, 1<<53+1),
		"unsafe-start-clock":           wireClockRange(1<<53, 0, "X", 0),
		"unsafe-gc-length":             wireClockRange(0, 1<<53, "X", 0),
		"unsafe-derived-text-end":      wireClockRange(0, wireSafeInteger, "X", 0),
		"unsafe-utf16-text-end":        wireClockRange(0, wireSafeInteger-1, "😀", 0),
		"unsafe-deleted-content-end":   wireClockRange(0, wireSafeInteger-1, "", 2),
		"unsafe-delete-clock":          deleteRange(1<<53, 1),
		"unsafe-delete-length":         deleteRange(0, 1<<53),
		"unsafe-derived-delete-end":    deleteRange(wireSafeInteger, 1),
	} {
		t.Run(name, func(t *testing.T) {
			s, ctx, _ := liveTestStore(t)
			base, err := s.LiveRead(ctx, "note.md")
			if err != nil {
				t.Fatal(err)
			}
			before := liveStored(t, s, "note.md")
			input := LiveUpdate{Path: "note.md", Generation: strings.Split(base.LiveRevision, ":")[0], OperationID: "rejected-then-valid", Update: raw}
			if accepted, err := s.LiveApply(ctx, input); err == nil {
				t.Fatalf("accepted a JavaScript-unsafe update: %+v", accepted)
			}
			after, err := s.LiveRead(ctx, "note.md")
			if err != nil || !reflect.DeepEqual(base, after) {
				t.Fatalf("rejected wire changed public state: %+v %v", after, err)
			}
			if liveStored(t, s, "note.md") != before {
				t.Fatal("rejected wire changed durable state or receipts")
			}
			input.Update = wireTextClients(wireSafeInteger)
			valid, err := s.LiveApply(ctx, input)
			if err != nil || !strings.Contains(valid.Content, "X") {
				t.Fatalf("rejected operation ID cannot be reused with valid boundary input: %+v %v", valid, err)
			}
			if _, err = s.LiveCheckpoint(ctx, "note.md"); err != nil {
				t.Fatal(err)
			}
			if replay, err := s.LiveApply(ctx, input); err != nil || replay.Content != valid.Content || replay.LiveRevision != valid.LiveRevision {
				t.Fatalf("valid boundary update lost idempotent receipt: %+v %v", replay, err)
			}
		})
	}
}

func TestLiveAcceptsSafeBoundaryClockRanges(t *testing.T) {
	s, ctx, _ := liveTestStore(t)
	base, err := s.LiveRead(ctx, "note.md")
	if err != nil {
		t.Fatal(err)
	}
	input := LiveUpdate{Path: "note.md", Generation: strings.Split(base.LiveRevision, ":")[0], OperationID: "safe-clock-boundary", Update: wireClockRange(0, wireSafeInteger-2, "😀", 0)}
	accepted, err := s.LiveApply(ctx, input)
	if err != nil || strings.Count(accepted.Content, "😀") != 2 {
		t.Fatalf("valid JavaScript-safe UTF-16 clock boundary rejected: %+v %v", accepted, err)
	}
}
