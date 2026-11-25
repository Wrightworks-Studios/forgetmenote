package workspace

import (
	"math/rand"
	"runtime"
	"strings"
	"testing"

	"github.com/reearth/ygo/crdt"
)

// heapInUse is the live heap after collecting garbage.
func heapInUse() int {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return int(m.HeapAlloc)
}

// scatteredEdit inserts or deletes one character at a random place, as two
// people typing in different parts of one note do.
func scatteredEdit(doc *crdt.Doc, random *rand.Rand) {
	n := doc.GetText(yjsText).Len()
	doc.Transact(func(tx *crdt.Transaction) {
		text := tx.GetText(yjsText)
		if random.Intn(3) == 0 && n > 10 {
			text.Delete(tx, random.Intn(n-1), 1)
		} else {
			text.Insert(tx, random.Intn(n+1), "x", nil)
		}
	})
}

// A document of many small scattered edits holds far more memory than its
// encoded state, and it now stays loaded between requests: the memory a
// notebook accounts for it must cover what it holds, whether it was loaded
// from its state or built update by update.
func TestLiveLoadedDocumentAccountsForTheMemoryItHolds(t *testing.T) {
	random := rand.New(rand.NewSource(1))
	base := yjsLive.New(strings.Repeat("A line of a note.\n", 1000)).Snapshot()
	// A browser's document, with a random 32-bit client ID as Yjs picks,
	// drawn from the test's source so that the encoded sizes repeat.
	client := func(state []byte) *crdt.Doc {
		doc := crdt.New(crdt.WithGC(false), crdt.WithClientID(crdt.ClientID(random.Uint32())))
		doc.GetText(yjsText)
		if err := crdt.ApplyUpdateV1(doc, state, nil); err != nil {
			t.Fatal(err)
		}
		return doc
	}
	var updates [][]byte
	// One typist, then many short sessions, each a Yjs client of its own.
	typist := client(base)
	for i := 0; i < 15000; i++ {
		before := typist.StateVector()
		scatteredEdit(typist, random)
		// Copied: an encoded update can share a far larger buffer.
		updates = append(updates, append([]byte(nil), crdt.EncodeStateAsUpdateV1(typist, before)...))
	}
	sessions, err := yjsLive.Load(base, updates)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 300; i++ {
		session := client(sessions.Snapshot())
		before := session.StateVector()
		for j := 0; j < 10; j++ {
			scatteredEdit(session, random)
		}
		update := append([]byte(nil), crdt.EncodeStateAsUpdateV1(session, before)...)
		if err := sessions.Apply(update); err != nil {
			t.Fatal(err)
		}
		updates = append(updates, update)
	}
	state := sessions.Snapshot()
	sessions, typist = nil, nil

	start := heapInUse()
	loaded, err := yjsLive.Load(state, nil)
	if err != nil {
		t.Fatal(err)
	}
	held := heapInUse() - start
	runtime.KeepAlive(loaded)
	loaded = nil
	t.Logf("state %d B; loaded document holds %d B (%.1f times), accounted %d B", len(state), held, float64(held)/float64(len(state)), liveDocBytes(len(state)))
	if liveDocBytes(len(state)) < held {
		t.Fatalf("a loaded document of a %d B state holds %d B, but counts %d B", len(state), held, liveDocBytes(len(state)))
	}

	start = heapInUse()
	built, err := yjsLive.Load(base, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, update := range updates {
		if err := built.Apply(update); err != nil {
			t.Fatal(err)
		}
	}
	held = heapInUse() - start
	runtime.KeepAlive(built)
	runtime.KeepAlive(updates)
	t.Logf("built update by update, it holds %d B (%.1f times)", held, float64(held)/float64(len(state)))
	if liveDocBytes(len(state)) < held {
		t.Fatalf("a document built by updates to a %d B state holds %d B, but counts %d B", len(state), held, liveDocBytes(len(state)))
	}
}

// Until a record's state is encoded again, its size is a bound reckoned from
// the updates since: what trimLive counts and the state limit is checked
// against. Deleting scattered characters splits a run of text for each one,
// which grows the state six times as much as the update that deletes them.
func TestLiveStateBoundCoversScatteredDeletes(t *testing.T) {
	s, ctx, _ := liveTestStore(t)
	content := strings.Repeat("abcdefghij", 24000)
	if _, err := s.Save(ctx, "long.md", []byte(content), "", ""); err != nil {
		t.Fatal(err)
	}
	base, err := s.LiveRead(ctx, "long.md")
	if err != nil {
		t.Fatal(err)
	}
	writer, _ := browserDoc(base.Update)
	known := writer.StateVector()
	writer.Transact(func(tx *crdt.Transaction) {
		text := tx.GetText(yjsText)
		for i := len(content) - 13; i > 0; i -= 13 {
			text.Delete(tx, i, 1)
		}
	})
	update := crdt.EncodeStateAsUpdateV1(writer, known)
	in := LiveUpdate{Path: "long.md", Generation: strings.Split(base.LiveRevision, ":")[0], OperationID: "scattered", Update: update}
	// A browser's update, answered with only the change.
	if _, err := s.LiveApply(WithLiveAnswer(ctx, LiveAnswer{Since: base.LiveRevision}), in); err != nil {
		t.Fatal(err)
	}
	n := liveDB(s)
	n.mu.Lock()
	defer n.mu.Unlock()
	v := n.live["long.md"]
	if v == nil || v.doc == nil || v.State != nil {
		t.Fatal("the document did not stay loaded, its state not encoded")
	}
	if actual := len(v.doc.Snapshot()); v.stateSize() < actual {
		t.Fatalf("a %d B update grew the state to %d B, past its bound %d B", len(update), actual, v.stateSize())
	}
}
