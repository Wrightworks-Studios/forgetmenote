package workspace

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"forgetmenote/internal/attribution"
	"forgetmenote/internal/notestore"

	"github.com/reearth/ygo/crdt"
)

// liveTab is a browser tab as frontend/collaboration.mjs keeps one: its
// document, what the server is known to hold (the updates it answered with
// and the ones it acknowledged), and the live revision it holds, which it
// names when it asks to be answered.
type liveTab struct {
	t       *testing.T
	name    string
	doc     *crdt.Doc
	server  *crdt.Doc
	held    LiveDocument
	refused int
	keys    int
}

func newLiveTab(t *testing.T, name string, first LiveDocument) *liveTab {
	t.Helper()
	doc, err := browserDoc(first.Update)
	if err != nil {
		t.Fatal(err)
	}
	server, _ := browserDoc(first.Update)
	return &liveTab{t: t, name: name, doc: doc, server: server, held: first}
}

func (b *liveTab) answer(ctx context.Context) context.Context {
	return WithLiveAnswer(ctx, LiveAnswer{Since: b.held.LiveRevision, Tab: b.name})
}

func (b *liveTab) receive(v LiveDocument) {
	b.t.Helper()
	for _, doc := range []*crdt.Doc{b.doc, b.server} {
		if err := crdt.ApplyUpdateV1(doc, v.Update, nil); err != nil {
			b.t.Fatal(err)
		}
	}
	if liveSequence(v.LiveRevision) >= liveSequence(b.held.LiveRevision) {
		b.held = v
	}
}

func (b *liveTab) text() string { return b.doc.GetText("markdown").ToString() }

// key types one character at the end of the tab's text and sends it to
// store, encoded against what the server holds, as the tab does; a refusal
// for missing dependencies resends the whole state, as the tab does.
func (b *liveTab) key(ctx context.Context, store *Store, c string) {
	b.t.Helper()
	text := b.doc.GetText("markdown")
	b.doc.Transact(func(tx *crdt.Transaction) { tx.GetText("markdown").Insert(tx, text.Len(), c, nil) })
	b.keys++
	in := LiveUpdate{Path: b.held.Path, Generation: strings.Split(b.held.LiveRevision, ":")[0], OperationID: fmt.Sprintf("%s-%d", b.name, b.keys), Update: crdt.EncodeStateAsUpdateV1(b.doc, b.server.StateVector())}
	v, err := store.LiveApply(b.answer(ctx), in)
	if errors.Is(err, errLiveMissing) {
		b.refused++
		in.OperationID += "-complete"
		in.Update = crdt.EncodeStateAsUpdateV1(b.doc, nil)
		v, err = store.LiveApply(b.answer(ctx), in)
	}
	if err != nil {
		b.t.Fatal(err)
	}
	if err = crdt.ApplyUpdateV1(b.server, in.Update, nil); err != nil {
		b.t.Fatal(err)
	}
	b.receive(v)
}

func liveSequence(revision string) int {
	var n int
	fmt.Sscan(revision[strings.LastIndexByte(revision, ':')+1:], &n)
	return n
}

// countingEngine counts the documents the store loads from their encoded
// state.
type countingEngine struct {
	liveEngine
	loads *atomic.Int64
}

func (e countingEngine) Load(snapshot []byte, updates [][]byte) (liveDoc, error) {
	e.loads.Add(1)
	return e.liveEngine.Load(snapshot, updates)
}

func countLiveLoads(t *testing.T) *atomic.Int64 {
	loads := &atomic.Int64{}
	engine := yjsLive
	yjsLive = countingEngine{engine, loads}
	t.Cleanup(func() { yjsLive = engine })
	return loads
}

// Typing keeps the note's document loaded: a key neither loads it from its
// encoded state nor encodes it again, past a compaction and polls alike;
// what the keys made checkpoints and starts again as before.
func TestLiveTypingKeepsTheDocumentLoaded(t *testing.T) {
	s, ctx, _ := liveTestStore(t)
	first, err := s.LiveRead(ctx, "note.md")
	if err != nil {
		t.Fatal(err)
	}
	tab := newLiveTab(t, "writer", first)
	// The hints of this store's own changes reach it after them, and one
	// naming the document's start, or the listener's first, makes it read
	// the document again. Count from when it has heard its own keys and
	// read it.
	heard := make(chan int64, 1024)
	liveDB(s).noteStore.WatchLive(func(_ string, c notestore.LiveChange) {
		select {
		case heard <- c.Seq:
		default:
		}
	})
	for settled := false; !settled; {
		if tab.keys > 50 {
			t.Fatal("the store never heard its own keys")
		}
		tab.key(ctx, s, "~")
		for wait := time.After(200 * time.Millisecond); !settled; {
			select {
			case seq := <-heard:
				settled = seq >= int64(liveSequence(tab.held.LiveRevision))
				continue
			case <-wait:
			}
			break
		}
	}
	if _, err := s.WaitLive(ctx, "note.md", liveKnown(tab.held), 0, true); err != nil {
		t.Fatal(err)
	}
	loads := countLiveLoads(t)
	for i := range dbLiveLogEntries + 20 {
		tab.key(ctx, s, string(rune('a'+i%26)))
		if v, err := s.WaitLive(ctx, "note.md", liveKnown(tab.held), 0, true); err != nil || !v.Unchanged {
			t.Fatalf("a poll after key %d: %+v %v", i, v, err)
		}
	}
	if n := loads.Load(); n != 0 {
		t.Fatalf("typing loaded the document %d times", n)
	}
	n := liveDB(s)
	n.mu.Lock()
	v := n.live["note.md"]
	encoded := v.State != nil
	n.mu.Unlock()
	if encoded {
		t.Fatal("a key encoded the whole state")
	}
	stored := liveStateOf(t, s, "note.md")
	if len(stored.Updates) >= dbLiveLogEntries {
		t.Fatalf("%d updates stored after the snapshot: the log did not compact", len(stored.Updates))
	}
	checkpoint, err := s.LiveCheckpoint(tab.answer(ctx), "note.md")
	if err != nil || !checkpoint.Incremental || checkpoint.Pending {
		t.Fatalf("checkpoint: %+v %v", checkpoint, err)
	}
	tab.receive(checkpoint)
	reopened, err := reopenLiveStore(t, s)
	if err != nil {
		t.Fatal(err)
	}
	again, err := reopened.LiveRead(ctx, "note.md")
	if err != nil || again.Content != tab.text() || again.Pending {
		t.Fatalf("the stored document after typing: %q, the tab's %q (%v)", again.Content, tab.text(), err)
	}
	// A save starts a new generation, which the loaded document does not
	// outlive.
	saved, err := reopened.Save(ctx, "note.md", []byte("saved over\n"), checkpoint.Revision, "")
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := reopened.LiveRead(ctx, "note.md")
	if err != nil || restarted.Content != string(saved.Content) || strings.Split(restarted.LiveRevision, ":")[0] == strings.Split(checkpoint.LiveRevision, ":")[0] {
		t.Fatalf("after a save: %+v %v", restarted, err)
	}
	if _, err := reopened.LiveApply(tab.answer(ctx), LiveUpdate{Path: "note.md", Generation: strings.Split(checkpoint.LiveRevision, ":")[0], OperationID: "old-generation", Update: crdt.EncodeStateAsUpdateV1(tab.doc, nil)}); !errors.Is(err, ErrConflict) {
		t.Fatalf("an update for the old generation: %v", err)
	}
}

// A note whose loaded document alone passes the notebook's live capacity,
// which counts it as sixteen times its state, is not kept loaded: each key
// unloads it, encoding its state, and loads it again, which is what every
// key cost before documents stayed loaded.
func TestLiveDocumentPastTheCapacityIsLoadedAtEveryKey(t *testing.T) {
	s, ctx, _ := liveTestStore(t)
	text := strings.Repeat("A line of the manuscript.\n", 4000)
	if _, err := s.Save(ctx, "long.md", []byte(text), "", ""); err != nil {
		t.Fatal(err)
	}
	first, err := s.LiveRead(ctx, "long.md")
	if err != nil {
		t.Fatal(err)
	}
	tab := newLiveTab(t, "writer", first)
	tab.key(ctx, s, "~")
	n := liveDB(s)
	n.mu.Lock()
	state := n.live["long.md"].stateSize()
	n.mu.Unlock()
	// The record fits without its document (its state and twice its text)
	// but not with it.
	capacity := liveCapacity
	liveCapacity = 4*len(text) + 8*state
	t.Cleanup(func() { liveCapacity = capacity })
	loads := countLiveLoads(t)
	const keys = 5
	for range keys {
		tab.key(ctx, s, "x")
	}
	if n := loads.Load(); n < keys {
		t.Fatalf("%d keys loaded the document %d times", keys, n)
	}
}

// A refused update leaves the loaded document as it was, and an agent's
// patch that conflicts changes nothing either.
func TestLiveRefusalsLeaveTheLoadedDocumentAsItWas(t *testing.T) {
	s, ctx, _ := liveTestStore(t)
	first, err := s.LiveRead(ctx, "note.md")
	if err != nil {
		t.Fatal(err)
	}
	tab := newLiveTab(t, "writer", first)
	tab.key(ctx, s, "!")
	// Text with a NUL is refused after the update applied to the document.
	tab.doc.Transact(func(tx *crdt.Transaction) { tx.GetText("markdown").Insert(tx, 0, "\x00", nil) })
	refused := crdt.EncodeStateAsUpdateV1(tab.doc, tab.server.StateVector())
	if _, err := s.LiveApply(tab.answer(ctx), LiveUpdate{Path: "note.md", Generation: strings.Split(first.LiveRevision, ":")[0], OperationID: "nul", Update: refused}); !errors.Is(err, ErrLiveOperationRejected) {
		t.Fatalf("an update leaving a NUL: %v", err)
	}
	if _, err := s.LivePatch(ctx, TargetedPatch{Path: "note.md", Base: first.LiveRevision, OperationID: "stale-agent", Edits: []TextEdit{{Start: 3, End: 8, Old: "gamma", Text: "x"}}}); !errors.Is(err, ErrConflict) {
		t.Fatalf("a patch of text that is not there: %v", err)
	}
	v, err := s.LiveRead(ctx, "note.md")
	if err != nil || v.Content != "😀 alpha\n\nbeta\n!" {
		t.Fatalf("the document after refusals: %q %v", v.Content, err)
	}
	other := newLiveTab(t, "other", v)
	other.key(ctx, s, "?")
	if v, err = s.LiveRead(ctx, "note.md"); err != nil || v.Content != "😀 alpha\n\nbeta\n!?" {
		t.Fatalf("an update after refusals: %q %v", v.Content, err)
	}
}

// waitAnswer starts store's long poll for tab and returns its answer.
func waitAnswer(ctx context.Context, store *Store, tab *liveTab) <-chan LiveDocument {
	answer := make(chan LiveDocument, 1)
	known := liveKnown(tab.held)
	go func() {
		v, err := store.WaitLive(tab.answer(ctx), "note.md", known, 25000, true)
		if err != nil {
			v.Path = "error: " + err.Error()
		}
		answer <- v
	}()
	return answer
}

// A tab's long poll is not answered with the tab's own update, which the
// update's answer brought; another tab's is, and the tab's own poll then
// answers what others change, after its update.
func TestLiveLongPollIsNotAnsweredWithTheTabsOwnUpdate(t *testing.T) {
	s, _, _ := liveTestStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	first, err := s.LiveRead(ctx, "note.md")
	if err != nil {
		t.Fatal(err)
	}
	writer, reader := newLiveTab(t, "writer", first), newLiveTab(t, "reader", first)
	own, others := waitAnswer(ctx, s, writer), waitAnswer(ctx, s, reader)
	waitForLiveReaders(t, s, 2)
	writer.key(ctx, s, "!")
	reader.receive(<-others)
	if reader.text() != writer.text() {
		t.Fatalf("the reader's text %q, the writer's %q", reader.text(), writer.text())
	}
	select {
	case v := <-own:
		t.Fatalf("the writer's poll was answered with its own update: %+v", v)
	case <-time.After(300 * time.Millisecond):
	}
	// A hint makes the gateway read the document again, which keeps what the
	// writer was answered with: a poll the writer starts before its update's
	// answer arrived waits too.
	n := liveDB(s)
	n.mu.Lock()
	n.liveSync().stale["note.md"] = true
	n.mu.Unlock()
	if _, err := s.LiveRead(ctx, "note.md"); err != nil {
		t.Fatal(err)
	}
	stale := *writer
	stale.held = first
	late := waitAnswer(ctx, s, &stale)
	waitForLiveReaders(t, s, 2)
	reader.key(ctx, s, "?")
	for _, answer := range []<-chan LiveDocument{own, late} {
		v := <-answer
		if !v.Incremental || v.LiveRevision != reader.held.LiveRevision {
			t.Fatalf("the writer's poll after the reader's key: %+v", v)
		}
		writer.receive(v)
	}
	if writer.text() != "😀 alpha\n\nbeta\n!?" {
		t.Fatalf("the writer's text: %q", writer.text())
	}
	// A tab that names no revision is answered as before.
	anonymous := *reader
	anonymous.name = ""
	plain := waitAnswer(ctx, s, &anonymous)
	waitForLiveReaders(t, s, 1)
	anonymous.key(ctx, s, ".")
	if v := <-plain; v.Unchanged || !strings.HasSuffix(v.LiveRevision, fmt.Sprint(":", liveSequence(anonymous.held.LiveRevision))) {
		t.Fatalf("a poll without a tab: %+v", v)
	}
}

// A tab whose updates reach two gateways builds on what one of them stored
// and the other has not heard of yet; the other reads the stored document
// again instead of refusing the update, so the tab never resends its whole
// state.
func TestLiveUpdatesAcrossTwoGatewaysNeedNoCompleteResend(t *testing.T) {
	s, ctx, _ := liveTestStore(t)
	other := openGateway(t, s)
	gateways := []*Store{s, other}
	first, err := s.LiveRead(ctx, "note.md")
	if err != nil {
		t.Fatal(err)
	}
	tabs := []*liveTab{newLiveTab(t, "one", first), newLiveTab(t, "two", first)}
	random := rand.New(rand.NewSource(1))
	for i := range 400 {
		tab := tabs[random.Intn(len(tabs))]
		gateway := gateways[random.Intn(len(gateways))]
		if random.Intn(4) == 0 {
			// The tab polls the gateway its next request reaches.
			v, err := gateway.WaitLive(tab.answer(ctx), "note.md", liveKnown(tab.held), 0, true)
			if err != nil {
				t.Fatal(err)
			}
			if !v.Unchanged {
				tab.receive(v)
			}
			continue
		}
		tab.key(ctx, gateway, string(rune('a'+i%26)))
	}
	for _, tab := range tabs {
		if tab.refused > 0 {
			t.Errorf("tab %s resent its whole state %d times", tab.name, tab.refused)
		}
	}
	stored, err := reopenLiveStore(t, s)
	if err != nil {
		t.Fatal(err)
	}
	v, err := stored.LiveRead(ctx, "note.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, tab := range tabs {
		tab.receive(v)
		if tab.text() != v.Content {
			t.Fatalf("tab %s holds %q, the stored note %q", tab.name, tab.text(), v.Content)
		}
	}
	if keys := tabs[0].keys + tabs[1].keys; len([]rune(v.Content))-len([]rune(first.Content)) != keys {
		t.Fatalf("%d keys made %q", keys, v.Content)
	}
}

// appendStoredUpdate stores tab's next key as an update headed by header,
// past LiveApply, as an earlier build or a damaged store would have.
func appendStoredUpdate(t *testing.T, s *Store, tab *liveTab, header dbLiveUpdate) {
	t.Helper()
	ctx := context.Background()
	text := tab.doc.GetText("markdown")
	tab.doc.Transact(func(tx *crdt.Transaction) { tx.GetText("markdown").Insert(tx, text.Len(), "#", nil) })
	update := crdt.EncodeStateAsUpdateV1(tab.doc, tab.server.StateVector())
	if err := crdt.ApplyUpdateV1(tab.server, update, nil); err != nil {
		t.Fatal(err)
	}
	n := liveDB(s)
	n.mu.Lock()
	defer n.mu.Unlock()
	v := n.live["note.md"]
	entry, err := n.liveEntry(ctx, "note.md")
	if err != nil {
		t.Fatal(err)
	}
	header.After = v.Sequence
	header.Receipt = liveReceipt{Time: time.Now().UTC().Format(time.RFC3339Nano)}
	body, err := dbLiveEncode(header, update)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = n.notes.LiveAppend(ctx, entry, dbLiveEpoch(v), body, "test"); err != nil {
		t.Fatal(err)
	}
}

// Replay reproduces the text each stored update's writer left: a checksum
// that does not match is refused, and updates stored with the text's
// SHA-256, before the CRC, still replay.
func TestStoredLiveUpdatesReproduceTheirText(t *testing.T) {
	s, ctx, _ := liveTestStore(t)
	first, err := s.LiveRead(ctx, "note.md")
	if err != nil {
		t.Fatal(err)
	}
	tab := newLiveTab(t, "writer", first)
	tab.key(ctx, s, "!")
	stored := liveStateOf(t, s, "note.md")
	var header dbLiveUpdate
	if _, err := dbLiveDecode(stored.Updates[len(stored.Updates)-1].Update, &header); err != nil || header.ContentCRC != liveContentCRC([]byte(tab.text())) || header.ContentHash != "" {
		t.Fatalf("a stored update's header: %+v %v", header, err)
	}
	expected := tab.text() + "#"
	appendStoredUpdate(t, s, tab, dbLiveUpdate{Key: strings.Repeat("a", 64), ContentHash: ContentHash([]byte(expected))})
	reopened, err := reopenLiveStore(t, s)
	if err != nil {
		t.Fatal(err)
	}
	if v, err := reopened.LiveRead(ctx, "note.md"); err != nil || v.Content != expected {
		t.Fatalf("an update stored with its text's SHA-256: %q %v", v.Content, err)
	}
	appendStoredUpdate(t, reopened, tab, dbLiveUpdate{Key: strings.Repeat("b", 64), ContentCRC: liveContentCRC([]byte("other text"))})
	again := reopenTestStore(t, reopened)
	if _, err := again.LiveRead(ctx, "note.md"); err == nil || !strings.Contains(err.Error(), "do not reproduce their text") {
		t.Fatalf("an update whose text replay does not reproduce: %v", err)
	}
}

// A tab is its user's: another user who names it cannot move its long poll
// past an update, which would then never answer the tab with that update.
func TestLiveTabIsBoundToItsUser(t *testing.T) {
	s, alice, _ := liveTestStore(t)
	ctx, cancel := context.WithTimeout(alice, 20*time.Second)
	defer cancel()
	first, err := s.LiveRead(ctx, "note.md")
	if err != nil {
		t.Fatal(err)
	}
	tab := newLiveTab(t, "tab", first)
	poll := waitAnswer(ctx, s, tab)
	waitForLiveReaders(t, s, 1)
	bob := attribution.With(ctx, attribution.Identity{ID: "bob", Name: "Bob", Source: "browser"})
	impostor := newLiveTab(t, "tab", first)
	impostor.key(bob, s, "!")
	select {
	case v := <-poll:
		tab.receive(v)
	case <-time.After(5 * time.Second):
		t.Fatal("the tab's poll was not answered with another user's update naming the tab")
	}
	if tab.text() != impostor.text() {
		t.Fatalf("the tab's text %q, the other user's %q", tab.text(), impostor.text())
	}
}
