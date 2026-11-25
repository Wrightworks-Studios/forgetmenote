package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf16"

	"forgetmenote/internal/attribution"
	"forgetmenote/internal/gitobj"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/encoding"
)

// Markdown stays an ordinary file at each checkpoint. The fsynced record is the
// authority between checkpoints, including operation receipts and CRDT identities.
const MaxLiveBytes = 1 << 20
const maxLiveStateBytes = 8 << 20

// liveCapacity bounds the live records one notebook keeps in memory. Tests lower
// it with the sizes they use, so a capacity case need not write 43 MiB through Git.
var liveCapacity = 128 << 20

var ErrLivePending = errors.New("live edits are waiting for a Git checkpoint; explicitly checkpoint the live note")

// ErrLiveOperationRejected means validation rejected the update before any
// content or receipt was accepted. Transport and persistence failures must not
// use it: their callers still need to retry the identical operation.
var ErrLiveOperationRejected = errors.New("live operation rejected")

type LiveDocument struct {
	Unchanged bool `json:"unchanged,omitempty"`
	// Incremental answers carry an empty Content, and Update holds only
	// what changed after the live revision the reader named.
	Incremental  bool   `json:"incremental,omitempty"`
	Path         string `json:"path"`
	Content      string `json:"content"`
	Revision     string `json:"revision"`
	Version      string `json:"version"`
	LiveRevision string `json:"liveRevision"`
	Update       []byte `json:"update"`
	Pending      bool   `json:"pending"`
}
type LiveUpdate struct {
	Path        string `json:"path"`
	Generation  string `json:"generation"`
	OperationID string `json:"operationId"`
	Update      []byte `json:"update"`
}

// LiveAnswer is how a browser asks to be answered. It travels beside a
// request (in headers, or a long poll's URL) and is no part of an
// operation, so it changes no request hash and a gateway that predates it
// answers as before. Since is
// the live revision the browser holds, for an answer of only what changed
// after it (LiveApply, LiveCheckpoint; a long poll names it in known). Tab
// names the browser tab, so that the tab's own long poll is not answered
// with the tab's own update.
type LiveAnswer struct {
	Since string
	Tab   string
}

type liveAnswerKey struct{}

// WithLiveAnswer asks the live requests made with ctx to answer as a says.
func WithLiveAnswer(ctx context.Context, a LiveAnswer) context.Context {
	return context.WithValue(ctx, liveAnswerKey{}, a)
}

func liveAnswerFrom(ctx context.Context) LiveAnswer {
	a, _ := ctx.Value(liveAnswerKey{}).(LiveAnswer)
	if len(a.Tab) > 64 {
		a.Tab = ""
	}
	// A tab is its user's: another who names it cannot move its long polls.
	if a.Tab != "" {
		a.Tab = liveActor(ctx) + "\x00" + a.Tab
	}
	return a
}

type TextEdit struct {
	Start int    `json:"start"`
	End   int    `json:"end"`
	Old   string `json:"old"`
	Text  string `json:"text"`
}
type TargetedPatch struct {
	Path        string     `json:"path"`
	Base        string     `json:"base"`
	OperationID string     `json:"operationId"`
	Edits       []TextEdit `json:"edits"`
}
type liveVersion struct {
	Revision string    `json:"revision"`
	State    []byte    `json:"state"`
	Time     time.Time `json:"time"`
}
type collaborationAuditKey struct{}
type liveReceipt struct {
	Request  string               `json:"request"`
	Revision string               `json:"revision"`
	Actor    attribution.Identity `json:"actor"`
	Time     string               `json:"time"`
}
type liveRecord struct {
	logBytes   int64
	logEntries int
	// doc is the engine's document of the record while it is loaded. It
	// is then what State encodes, and State is encoded from it only when
	// something needs the bytes (state), not after every update. A change
	// takes doc from the record (takeDoc), so one that fails never leaves
	// the record out of step: without State, the record is read again.
	doc liveDoc
	// stateBound bounds len(State) while State is not encoded.
	stateBound int
	// head is the notebook head at which the record was last found to be
	// its note's document, and entry the note's entry ID there.
	head  gitobj.ID
	entry string
	// tabs holds what each browser tab's last update was answered with.
	tabs map[string]liveTabAnswer

	Path              string                 `json:"path"`
	Generation        string                 `json:"generation"`
	Sequence          uint64                 `json:"sequence"`
	GitVersion        string                 `json:"gitVersion"`
	CheckpointContent string                 `json:"checkpointContent"`
	Content           string                 `json:"content"`
	State             []byte                 `json:"state"`
	Versions          []liveVersion          `json:"versions"`
	Receipts          map[string]liveReceipt `json:"receipts"`
	Uncommitted       []string               `json:"uncommitted"`
}

func (v *liveRecord) revision() string { return fmt.Sprintf("%s:%d", v.Generation, v.Sequence) }

// known is what a browser holding v names in a long poll: its live
// revision, Git version and whether edits are pending.
func (v *liveRecord) known() string {
	return v.revision() + "/" + v.GitVersion + fmt.Sprint("/", len(v.Uncommitted) > 0)
}

// state is v's encoded Yjs state, encoded from its document if need be.
func (v *liveRecord) state() []byte {
	if v.State == nil && v.doc != nil {
		v.State = v.doc.Snapshot()
	}
	return v.State
}

// stateSize is len(v.State), or a bound of it while it is not encoded.
func (v *liveRecord) stateSize() int {
	if v.State != nil {
		return len(v.State)
	}
	return v.stateBound
}

// loadDoc returns v's document, loading it and keeping it loaded.
func (v *liveRecord) loadDoc() (liveDoc, error) {
	if v.doc == nil {
		if v.State == nil {
			return nil, errors.New("the live document is not loaded; read it again")
		}
		doc, err := yjsLive.Load(v.State, nil)
		if err != nil {
			return nil, err
		}
		v.doc = doc
	}
	return v.doc, nil
}

// takeDoc returns v's document for a change, which v no longer holds.
func (v *liveRecord) takeDoc() (liveDoc, error) {
	doc, err := v.loadDoc()
	v.doc = nil
	return doc, err
}

// liveDocBytes bounds the memory a loaded document of an encoded state of n
// bytes holds besides it. Each item of the document (a run of text one
// client inserted at once) holds about ten times what it takes encoded, so a
// document of many small scattered edits, as two people typing in one note
// make, holds up to 13 times its state (TestLiveLoadedDocumentAccounts-
// ForTheMemoryItHolds); one inserted whole holds little more than its text.
// Sixteen times leaves a margin and keeps a manuscript's document loaded:
// F1's 2.8 MB state counts 45 MB of a notebook's 128 MiB, and even at the
// 6.4 MiB past which its checkpoint starts it again it fits. A document
// past the capacity is not kept loaded.
func liveDocBytes(n int) int { return 16 * n }

func (s *Store) trimLive(keep string) error {
	size := func(v *liveRecord) int {
		n := v.stateSize() + len(v.Content) + len(v.CheckpointContent) + len(v.Receipts)*1024
		if v.doc != nil {
			n += liveDocBytes(v.stateSize())
		}
		for _, base := range v.Versions {
			n += len(base.State)
		}
		return n
	}
	total := 0
	for _, v := range s.live {
		total += size(v)
	}
	// Other notes' loaded documents go first, then idle committed records,
	// then keep's loaded document.
	unload := func(v *liveRecord) {
		total -= size(v)
		v.state()
		v.doc = nil
		total += size(v)
	}
	for path, v := range s.live {
		if total > liveCapacity && path != keep && v.doc != nil {
			unload(v)
		}
	}
	for path, v := range s.live {
		if total > liveCapacity && path != keep && len(v.Uncommitted) == 0 {
			total -= size(v)
			delete(s.live, path)
		}
	}
	if v := s.live[keep]; total > liveCapacity && v != nil && v.doc != nil {
		unload(v)
	}
	if total > liveCapacity {
		return capacityReached("live notebook capacity reached; checkpoint pending notes before opening more live notes")
	}
	return nil
}

func (s *Store) hasLivePending() bool {
	for _, v := range s.live {
		if len(v.Uncommitted) > 0 {
			return true
		}
	}
	return false
}

func liveOperation(ctx context.Context, id string, in any) (string, string, error) {
	if len(id) < 1 || len(id) > 128 || strings.ContainsAny(id, "\r\n\x00") {
		return "", "", fmt.Errorf("%w: operationId must contain 1–128 characters", ErrLiveOperationRejected)
	}
	raw, _ := json.Marshal(in)
	actor := attribution.From(ctx)
	return ContentHash([]byte(actor.ID + "\x00" + actor.Source + "\x00" + actor.Client + "\x00" + id)), ContentHash(raw), nil
}
func utf16Slice(text string, start, end int) (string, bool) {
	if start < 0 || end < start {
		return "", false
	}
	units := utf16.Encode([]rune(text))
	if end > len(units) {
		return "", false
	}
	if start > 0 && start < len(units) && utf16.IsSurrogate(rune(units[start])) && units[start] >= 0xdc00 {
		return "", false
	}
	if end > 0 && end < len(units) && units[end] >= 0xdc00 && units[end] <= 0xdfff {
		return "", false
	}
	return string(utf16.Decode(units[start:end])), true
}

func liveSnapshotBase(doc *crdt.Doc, snapshot *crdt.Snapshot) (*crdt.Doc, error) {
	base, err := crdt.CreateDocFromSnapshot(doc, snapshot)
	if err != nil {
		return nil, err
	}
	// The pinned snapshot encoder includes whole item runs. A browser can merge
	// a later insertion into a run spanning the retained base clock. Tombstone
	// those suffix IDs so new identical text cannot become an original anchor.
	clocks := base.StateVector()
	clients := []crdt.ClientID{}
	for client, clock := range clocks {
		if clock > snapshot.StateVector[client] {
			clients = append(clients, client)
		}
	}
	if len(clients) == 0 {
		return base, nil
	}
	sort.Slice(clients, func(i, j int) bool { return clients[i] < clients[j] })
	update := encoding.NewEncoder()
	update.WriteVarUint(0) // Yjs V1 update with no inserted structs, only deletes.
	update.WriteVarUint(uint64(len(clients)))
	for _, client := range clients {
		start := snapshot.StateVector[client]
		update.WriteVarUint(uint64(client))
		update.WriteVarUint(1)
		update.WriteVarUint(start)
		update.WriteVarUint(clocks[client] - start)
	}
	if err = crdt.ApplyUpdateV1(base, update.Bytes(), nil); err != nil {
		return nil, err
	}
	return base, nil
}
