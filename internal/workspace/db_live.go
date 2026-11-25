package workspace

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"forgetmenote/internal/attribution"
	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
)

// Live editing of a notebook kept in the notes store. The notes store keeps
// each note's live document under the note's entry ID: a snapshot, the
// updates accepted after it, and an epoch that fences updates made for a
// document it has since replaced. s.live holds the documents this store has
// loaded, as the Git backend holds its records, and every pending one, so
// writes checkpoint accepted edits first; other gateways' changes arrive as
// the notes store's live hints.

// liveReceiptKind is the notes store's receipt kind for checkpointed live
// operations. A pending operation's receipt travels in its stored update.
const liveReceiptKind = "live"

// The updates kept after a snapshot before they are compacted into a new
// one: the bounds of the Git backend's append log.
const (
	dbLiveLogEntries = 128
	dbLiveLogBytes   = 4 << 20
)

// dbLiveLimit is the largest note live editing takes. Since the editor draws
// only what is on screen in every mode, it is the ordinary read limit of a
// note, not the Git backend's 1 MiB.
func dbLiveLimit(path string) int { return MaxReadBytes(path) }

// dbLiveStateLimit bounds a document's encoded Yjs state and a client's
// update, which browsers send as their whole state. Tombstones stay until a
// checkpoint starts the document again, so it is twice the text limit plus
// the Git backend's 8 MiB.
func dbLiveStateLimit(path string) int { return 2*dbLiveLimit(path) + maxLiveStateBytes }

// dbLiveRestartBytes is the state past which a checkpoint starts a document
// again in a new generation, which stale clients reconcile: the Git
// backend's 4 MiB of history, plus twice the text, since a live note can now
// be far larger than 4 MiB.
func dbLiveRestartBytes(text int) int { return 4<<20 + 2*text }

// Stored live bytes are a line of JSON and then the engine's bytes as they
// are.
const dbLiveFormat = "fmn-live 1 "

// dbLiveSnapshot heads a stored snapshot: the record it restores but for its
// text, which the engine's state holds.
type dbLiveSnapshot struct {
	Path        string                 `json:"path"`
	Generation  string                 `json:"generation"`
	Sequence    uint64                 `json:"sequence"`
	GitVersion  string                 `json:"gitVersion"`
	Versions    []liveVersion          `json:"versions"`
	Receipts    map[string]liveReceipt `json:"receipts"`
	Uncommitted []string               `json:"uncommitted"`
}

// dbLiveUpdate heads a stored update: the operation it accepted, the
// sequence number of the document it changed, and a checksum of the text it
// left, which replay must reproduce. Its receipt's revision is its own
// sequence number, which the notes store assigns, so that is filled in when
// the update is read.
//
// The checksum is the text's CRC-32C, which costs a few hundredths of a
// millisecond in a 1.2 MB note where a SHA-256 cost more than the rest of
// a keystroke; it is a check of replay, not an identity. Updates stored
// before it carry the text's SHA-256 instead.
type dbLiveUpdate struct {
	Key         string      `json:"key"`
	Receipt     liveReceipt `json:"receipt"`
	After       uint64      `json:"after"`
	ContentHash string      `json:"contentHash,omitempty"`
	ContentCRC  string      `json:"contentCRC,omitempty"`
}

var liveCRCTable = crc32.MakeTable(crc32.Castagnoli)

func liveContentCRC(text []byte) string {
	return fmt.Sprintf("%08x", crc32.Checksum(text, liveCRCTable))
}

// reproduces reports whether text is the text h's update left, by every
// checksum h names.
func (h dbLiveUpdate) reproduces(text string) bool {
	if h.ContentCRC == "" && h.ContentHash == "" {
		return false
	}
	return (h.ContentCRC == "" || liveContentCRC([]byte(text)) == h.ContentCRC) && (h.ContentHash == "" || ContentHash([]byte(text)) == h.ContentHash)
}

func dbLiveEncode(header any, body []byte) ([]byte, error) {
	raw, err := json.Marshal(header)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, len(dbLiveFormat)+len(raw)+1+len(body))
	return append(append(append(append(out, dbLiveFormat...), raw...), '\n'), body...), nil
}

func dbLiveDecode(raw []byte, header any) ([]byte, error) {
	rest, ok := bytes.CutPrefix(raw, []byte(dbLiveFormat))
	line, body, found := bytes.Cut(rest, []byte{'\n'})
	if !ok || !found || json.Unmarshal(line, header) != nil {
		return nil, errors.New("unsupported or invalid stored live document; restore the complete notebook backup")
	}
	return body, nil
}

// A generation is the document's epoch in 16 hex digits and 16 random ones,
// so a document started again after its notebook was recreated never
// repeats one.
func dbLiveGeneration(epoch int64) string {
	var random [8]byte
	rand.Read(random[:])
	return fmt.Sprintf("%016x%s", epoch, hex.EncodeToString(random[:]))
}

func dbLiveEpoch(v *liveRecord) int64 {
	if len(v.Generation) != 32 {
		return -1
	}
	epoch, err := strconv.ParseInt(v.Generation[:16], 16, 64)
	if err != nil {
		return -1
	}
	return epoch
}

// retainLiveVersions keeps the patch bases of the last 15 minutes within
// 16 MiB, and always the newest, as the Git backend does.
func retainLiveVersions(versions []liveVersion) []liveVersion {
	budget := 0
	first := len(versions) - 1
	for i := len(versions) - 1; i >= 0; i-- {
		budget += len(versions[i].State)
		if budget > 16<<20 || time.Since(versions[i].Time) > 15*time.Minute {
			break
		}
		first = i
	}
	return versions[max(first, 0):]
}

// liveEntry is the entry ID path's live document is kept under.
func (n *dbNotebook) liveEntry(ctx context.Context, path string) (string, error) {
	f, err := n.notes.Stat(ctx, gitobj.ID{}, path)
	return f.EntryID, err
}

// liveLoad reads path's live document, kept under entry, or nil when there
// is none, and the stored epoch, 0 when the note was never live. A stored
// document that does not replay exactly is an error.
func (n *dbNotebook) liveLoad(ctx context.Context, path, entry string) (*liveRecord, int64, error) {
	st, err := n.notes.LiveState(ctx, entry)
	if err != nil {
		return nil, 0, err
	}
	if st.Snapshot == nil {
		if len(st.Updates) > 0 {
			return nil, st.Epoch, errors.New("live updates are stored without their snapshot; restore the complete notebook backup")
		}
		// Never live, or started again and not stored yet.
		return nil, st.Epoch, nil
	}
	v, err := liveReplayStored(st)
	return v, st.Epoch, err
}

// liveReplayStored restores the record st stores: its snapshot, then each
// update after it.
func liveReplayStored(st notestore.LiveState) (*liveRecord, error) {
	var header dbLiveSnapshot
	state, err := dbLiveDecode(st.Snapshot, &header)
	if err != nil {
		return nil, err
	}
	v := &liveRecord{Path: header.Path, Generation: header.Generation, Sequence: header.Sequence, GitVersion: header.GitVersion,
		Versions: header.Versions, Receipts: header.Receipts, Uncommitted: header.Uncommitted, State: state}
	if dbLiveEpoch(v) != st.Epoch || v.Sequence != uint64(st.SnapshotSeq) || v.Receipts == nil || len(v.Versions) == 0 || len(v.Uncommitted) != len(v.Receipts) {
		return nil, errors.New("invalid stored live snapshot; restore the complete notebook backup")
	}
	doc, err := yjsLive.Load(state, nil)
	if err != nil {
		return nil, err
	}
	var last dbLiveUpdate
	previous := v.Sequence
	for _, u := range st.Updates {
		var h dbLiveUpdate
		delta, err := dbLiveDecode(u.Update, &h)
		if err != nil {
			return nil, err
		}
		_, exists := v.Receipts[h.Key]
		at, timeErr := time.Parse(time.RFC3339Nano, h.Receipt.Time)
		if _, e := hex.DecodeString(h.Key); len(h.Key) != 64 || e != nil || exists || timeErr != nil || u.Seq <= int64(v.Sequence) {
			return nil, errors.New("invalid stored live update sequence or receipt; restore the complete notebook backup")
		}
		if err = doc.Apply(delta); err != nil {
			return nil, fmt.Errorf("invalid stored live update: %w", err)
		}
		previous, last = v.Sequence, h
		v.Sequence = uint64(u.Seq)
		h.Receipt.Revision = v.revision()
		v.Receipts[h.Key] = h.Receipt
		v.Uncommitted = append(v.Uncommitted, h.Key)
		v.Versions = append(v.Versions, liveVersion{Revision: v.revision(), State: doc.Version(), Time: at})
		v.logEntries++
		v.logBytes += int64(len(u.Update))
	}
	v.Content = doc.Text()
	v.CheckpointContent = v.Content
	if len(st.Updates) > 0 {
		// The last update reproduces its writer's text unless another
		// gateway's update came between it and the document it changed.
		if last.After == previous && !last.reproduces(v.Content) {
			return nil, errors.New("stored live updates do not reproduce their text; restore the complete notebook backup")
		}
		v.State = doc.Snapshot()
	}
	v.doc = doc
	v.Versions = retainLiveVersions(v.Versions)
	return v, nil
}

// liveCompact stores v as entry's snapshot, replacing the updates it
// includes. A snapshot with pending edits has no base revision, so a listing
// tells which documents a restart loads.
func (n *dbNotebook) liveCompact(ctx context.Context, entry string, v *liveRecord) error {
	raw, err := dbLiveEncode(dbLiveSnapshot{Path: v.Path, Generation: v.Generation, Sequence: v.Sequence, GitVersion: v.GitVersion,
		Versions: v.Versions, Receipts: v.Receipts, Uncommitted: v.Uncommitted}, v.state())
	if err != nil {
		return err
	}
	var base gitobj.ID
	if len(v.Uncommitted) == 0 {
		if base, err = parseRevision(v.GitVersion); err != nil {
			return err
		}
	}
	if err = n.notes.LiveCompact(ctx, entry, dbLiveEpoch(v), raw, int64(v.Sequence), base); err != nil {
		return err
	}
	v.logEntries, v.logBytes = 0, 0
	return nil
}

// liveStart starts path's live document again from its committed note, in a
// new epoch, so updates made for the replaced document fail. A normal save,
// import, move or restore starts a new generation: stale offline clients
// must reconcile, and old updates cannot resurrect a deleted path.
func (n *dbNotebook) liveStart(ctx context.Context, entry string, current Document) (*liveRecord, error) {
	if len(current.Content) > dbLiveLimit(current.Path) {
		return nil, errors.New("live editing supports notes up to FMN_LIMIT_FILE_BYTES; use ordinary revision-checked saves for larger notes")
	}
	epoch, err := n.notes.LiveReset(ctx, entry)
	if err != nil {
		return nil, err
	}
	st, err := n.notes.LiveState(ctx, entry)
	if err != nil {
		return nil, err
	}
	if st.Epoch != epoch {
		return nil, fmt.Errorf("another gateway started the live note again; retry: %w", ErrConflict)
	}
	doc := yjsLive.New(string(current.Content))
	v := &liveRecord{Path: current.Path, Generation: dbLiveGeneration(epoch), Sequence: uint64(st.SnapshotSeq), GitVersion: current.Version,
		Content: string(current.Content), State: doc.Snapshot(), Receipts: map[string]liveReceipt{}, doc: doc}
	v.CheckpointContent = v.Content
	v.Versions = []liveVersion{{v.revision(), doc.Version(), time.Now().UTC()}}
	if err = n.liveCompact(ctx, entry, v); err != nil {
		return nil, err
	}
	return v, nil
}

// liveRecord is the Git backend's liveRecord: path's current live document,
// started again when Git changed the note and reconciled with a checkpoint
// that committed before the document recorded it. Callers hold mu.
func (n *dbNotebook) liveRecord(ctx context.Context, path string) (*liveRecord, error) {
	if n.liveStorageFailed {
		return nil, errors.New("live storage failed; restart the notebook owner to recover accepted operations")
	}
	// Browse exclusions do not restrict direct access or recovery of accepted
	// edits. read still validates the path and the committed file mode.
	if !strings.HasSuffix(path, ".md") || path == "AGENTS.md" {
		return nil, ErrForbidden
	}
	sync := n.liveSync()
	if v := n.live[path]; v != nil && !sync.stale[path] && ctx.Err() == nil {
		// The note is unchanged since the document was loaded, so reading it
		// would succeed: answer from memory. While the notebook's head is
		// the one the note was last found unchanged at, it still is.
		if _, err := n.resolve(path, false); err == nil {
			head, err := n.notes.Head(ctx)
			if err == nil && !v.head.IsZero() && head.Commit == v.head {
				return v, nil
			}
			f, err := n.notes.Stat(ctx, gitobj.ID{}, path)
			if err == nil && f.Mode == gitobj.ModeFile && f.Size <= int64(MaxReadBytes(path)) && f.Version.String() == v.GitVersion {
				v.head, v.entry = head.Commit, f.EntryID
				return v, nil
			}
		}
	}
	current, err := n.read(ctx, path, "")
	if err != nil {
		return nil, err
	}
	entry, err := n.liveEntry(ctx, path)
	if err != nil {
		return nil, err
	}
	// Anything but the current document is decided on the stored one, so an
	// edit another gateway accepted is never started again from Git.
	v, epoch, err := n.liveLoad(ctx, path, entry)
	if err != nil {
		return nil, err
	}
	if held := n.live[path]; held != nil && len(held.Uncommitted) > 0 && (epoch == 0 || epoch == dbLiveEpoch(held) && (v == nil || v.Sequence < held.Sequence)) {
		// Edits this store acknowledged are gone from the notes store, which
		// no one started again: never drop them silently.
		return nil, errors.New("accepted live edits are missing from the notes store; restore the complete notebook backup")
	}
	delete(sync.stale, path)
	if v != nil && v.Path != path {
		// The note's identity moved here with its old path's document.
		if len(v.Uncommitted) > 0 {
			return nil, fmt.Errorf("Git changed while live edits were pending; retain the collaboration backup and reconcile: %w", ErrConflict)
		}
		v = nil
	}
	if v == nil {
		delete(n.live, path)
	} else {
		// Reading the document again, for another gateway's change or a
		// hint, keeps what the tabs were answered with.
		if held := n.live[path]; held != nil && held.Generation == v.Generation {
			v.tabs = held.tabs
		}
		n.live[path] = v
		if err = n.trimLive(path); err != nil {
			delete(n.live, path)
			return nil, err
		}
	}
	if v != nil && (v.GitVersion == current.Version || n.liveBaseNewer(ctx, v.GitVersion, current.Version)) {
		return v, nil
	}
	if v != nil && len(v.Uncommitted) > 0 {
		if v.Content != string(current.Content) {
			return nil, fmt.Errorf("Git changed while live edits were pending; retain the collaboration backup and reconcile: %w", ErrConflict)
		}
		checkpoint, err := n.verifiedLiveCheckpoint(ctx, v)
		if err != nil {
			return nil, err
		}
		if checkpoint.Revision == "" || checkpoint.Version != current.Version {
			return nil, fmt.Errorf("Git changed without the expected live checkpoint; retain the collaboration backup and reconcile: %w", ErrConflict)
		}
		next := *v
		if err = n.archiveLiveReceipts(ctx, v); err != nil {
			return nil, err
		}
		next.Uncommitted = nil
		next.Receipts = map[string]liveReceipt{}
		next.GitVersion = current.Version
		next.CheckpointContent = next.Content
		if err = n.liveCompact(ctx, entry, &next); err != nil {
			return nil, err
		}
		n.live[path] = &next
		n.notifyLive(path)
		return &next, nil
	}
	if v != nil {
		if err = n.archiveLiveReceipts(ctx, v); err != nil {
			return nil, err
		}
	}
	next, err := n.liveStart(ctx, entry, current)
	if err != nil {
		return nil, err
	}
	n.live[path] = next
	if err = n.trimLive(path); err != nil {
		delete(n.live, path)
		return nil, err
	}
	return next, nil
}

// liveBaseNewer reports whether version, a stored document's revision,
// changed its note later than current did: this gateway has not yet heard
// of another gateway's checkpoint, which the document already records, so
// it is current rather than to be started again from an older note.
func (n *dbNotebook) liveBaseNewer(ctx context.Context, version, current string) bool {
	a, err := parseRevision(version)
	if err != nil || a.IsZero() {
		return false
	}
	b, err := parseRevision(current)
	if err != nil || b.IsZero() {
		return false
	}
	newer, err := n.notes.Commit(ctx, a)
	if err != nil {
		return false
	}
	older, err := n.notes.Commit(ctx, b)
	return err == nil && newer.Seq > older.Seq
}

// liveOutput answers with v. since is the live revision a browser holds, or
// empty: while
// v keeps that revision as a patch base, the answer is only the Yjs update
// since it, without the Markdown, so its size follows the change rather than
// the note. Otherwise, and for every reader that names no revision, it is
// the whole Markdown and Yjs state.
func (n *dbNotebook) liveOutput(ctx context.Context, v *liveRecord, since string) (LiveDocument, error) {
	if scope := mutationScopeFrom(ctx); scope != nil {
		if err := scope.checkContent([]byte(v.Content)); err != nil {
			return LiveDocument{}, err
		}
	}
	head, err := n.head(ctx)
	out := LiveDocument{Path: v.Path, Revision: head, Version: v.GitVersion, LiveRevision: v.revision(), Pending: len(v.Uncommitted) > 0}
	if base := v.base(since); base != nil && err == nil {
		doc, err := v.loadDoc()
		if err != nil {
			return LiveDocument{}, err
		}
		if out.Update, err = doc.EncodeSince(base); err != nil {
			return LiveDocument{}, err
		}
		out.Incremental = true
		return out, nil
	}
	out.Content, out.Update = v.Content, v.state()
	return out, err
}

// base is the patch base v keeps of live revision since, or nil.
func (v *liveRecord) base(since string) []byte {
	if since == "" {
		return nil
	}
	for _, version := range v.Versions {
		if version.Revision == since {
			return version.State
		}
	}
	return nil
}

func (n *dbNotebook) LiveRead(ctx context.Context, path string) (LiveDocument, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := n.checkMutationTarget(ctx, path, false); err != nil {
		return LiveDocument{}, err
	}
	v, err := n.liveRecord(ctx, path)
	if err != nil {
		return LiveDocument{}, err
	}
	return n.liveOutput(ctx, v, "")
}

// LivePoll avoids sending Markdown and Yjs state when a browser already has both
// the live revision and its current Git checkpoint. known is
// liveRevision/version/pending; incremental asks for only what changed after
// that live revision (liveOutput).
func (n *dbNotebook) LivePoll(ctx context.Context, path, known string, incremental bool) (LiveDocument, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.livePoll(ctx, path, known, incremental)
}

// livePoll is LivePoll for callers that hold mu.
func (n *dbNotebook) livePoll(ctx context.Context, path, known string, incremental bool) (LiveDocument, error) {
	if err := n.checkMutationTarget(ctx, path, false); err != nil {
		return LiveDocument{}, err
	}
	v, err := n.liveRecord(ctx, path)
	if err != nil {
		return LiveDocument{}, err
	}
	if known == v.known() {
		return LiveDocument{Unchanged: true}, nil
	}
	since := ""
	if incremental {
		since = liveKnownRevision(known)
	}
	return n.liveOutput(ctx, v, since)
}

func (n *dbNotebook) liveWritable(ctx context.Context, path string) error {
	if err := n.loadLocks(ctx); err != nil {
		return err
	}
	if _, err := n.resolve(path, true); err != nil {
		return err
	}
	// An unrelated pending note does not block live peers.
	n.checkpointing = true
	defer func() { n.checkpointing = false }()
	return n.clean(ctx)
}

// liveActor names who made an update in the notes store's update log.
func liveActor(ctx context.Context) string {
	actor := attribution.From(ctx)
	if actor.ID != "" {
		return actor.ID
	}
	return actor.Source
}

// liveDiscard is called when a change to v's document, taken from v, did
// not become v's next record: v is read again from storage unless its
// encoded state still describes it.
func (n *dbNotebook) liveDiscard(v *liveRecord) {
	if v.State == nil {
		n.liveSync().stale[v.Path] = true
	}
}

// acceptLive stores operation key, which made doc of v by delta, with its
// receipt for request. It owns the checks of the text and state an
// operation leaves. doc is taken from v (takeDoc); the record after it
// holds doc, and since is the live revision the browser holds.
func (n *dbNotebook) acceptLive(ctx context.Context, v *liveRecord, doc liveDoc, delta []byte, key, request, since string) (LiveDocument, error) {
	accepted := false
	defer func() {
		if !accepted {
			n.liveDiscard(v)
		}
	}()
	content := doc.Text()
	raw := []byte(content)
	if err := n.prepareIdentities(ctx, map[string][]byte{v.Path: raw}); err != nil {
		return LiveDocument{}, fmt.Errorf("%w: %v", ErrLiveOperationRejected, err)
	}
	if err := n.checkMutationChanges(ctx, map[string][]byte{v.Path: raw}); err != nil {
		return LiveDocument{}, err
	}
	// The store checks again, as it stores the update, that the authority
	// record checkMutationChanges read is still the record, as applyChanges
	// has it check for a revision: a handoff another gateway prepared or
	// completed meanwhile refuses this update too.
	authority := n.authorityVersion
	if len(content) > dbLiveLimit(v.Path) || !utf8.ValidString(content) || strings.ContainsRune(content, 0) {
		return LiveDocument{}, fmt.Errorf("%w: live note must be UTF-8 Markdown within FMN_LIMIT_FILE_BYTES", ErrLiveOperationRejected)
	}
	// The state is encoded when something needs it, not after every key:
	// until then its size is bounded by the updates'. Integrating an update
	// can split existing runs of text for each structure or deletion it
	// carries, and each split costs an item's header: scattered deletions
	// grew a state six times as much as their update
	// (TestLiveStateBoundCoversScatteredDeletes). Ten times the update is the
	// bound, and the state is encoded once the bound passes half the limit,
	// so even a growth of twenty times cannot pass it unseen.
	var state []byte
	bound := v.stateSize() + 10*len(delta)
	if bound > dbLiveStateLimit(v.Path)/2 {
		state = doc.Snapshot()
		bound = len(state)
		if len(state) > dbLiveStateLimit(v.Path) {
			return LiveDocument{}, fmt.Errorf("%w: Yjs state exceeds its limit; checkpoint then reconnect to compact its state", ErrLiveOperationRejected)
		}
	}
	entry := v.entry
	if head, err := n.notes.Head(ctx); err != nil || head.Commit != v.head || entry == "" {
		if entry, err = n.liveEntry(ctx, v.Path); err != nil {
			return LiveDocument{}, err
		}
	}
	receipt := liveReceipt{Request: request, Actor: attribution.From(ctx), Time: time.Now().UTC().Format(time.RFC3339Nano)}
	body, err := dbLiveEncode(dbLiveUpdate{Key: key, Receipt: receipt, After: v.Sequence, ContentCRC: liveContentCRC(raw)}, delta)
	if err != nil {
		return LiveDocument{}, err
	}
	next := *v
	next.Sequence++
	next.Content = content
	next.State, next.stateBound, next.doc = state, bound, doc
	receipt.Revision = next.revision()
	next.Receipts = make(map[string]liveReceipt, len(v.Receipts)+1)
	for k, r := range v.Receipts {
		next.Receipts[k] = r
	}
	next.Receipts[key] = receipt
	next.Uncommitted = append(append([]string{}, v.Uncommitted...), key)
	next.Versions = retainLiveVersions(append(append([]liveVersion{}, v.Versions...), liveVersion{next.revision(), doc.Version(), time.Now().UTC()}))
	if err := ctx.Err(); err != nil {
		return LiveDocument{}, err
	}
	n.live[v.Path] = &next
	if err := n.trimLive(v.Path); err != nil {
		n.live[v.Path] = v
		return LiveDocument{}, err
	}
	n.live[v.Path] = v
	sync := n.liveSync()
	seq, err := n.notes.LiveAppendFenced(ctx, entry, dbLiveEpoch(v), body, liveActor(ctx), authority)
	if err != nil {
		// Whether or not the update was stored, read the document again.
		sync.stale[v.Path] = true
		if errors.Is(err, notestore.ErrAuthorityChanged) {
			return LiveDocument{}, n.authorityRefusal(ctx, err)
		}
		if errors.Is(err, notestore.ErrConflict) {
			return LiveDocument{}, fmt.Errorf("the live note was started again; read it and reconcile: %w", ErrConflict)
		}
		return LiveDocument{}, err
	}
	accepted = true
	ctx = context.WithoutCancel(ctx)
	next.logEntries++
	next.logBytes += int64(len(body))
	if uint64(seq) != next.Sequence {
		// Another gateway's updates came first: hold the stored document,
		// which has them and this one.
		fresh, _, err := n.liveLoad(ctx, v.Path, entry)
		if err != nil || fresh == nil {
			sync.stale[v.Path] = true
			return LiveDocument{}, errors.Join(errors.New("the live note changed elsewhere while this edit was stored; read it again"), err)
		}
		next = *fresh
		next.tabs = v.tabs
	} else if next.logEntries >= dbLiveLogEntries || next.logBytes > dbLiveLogBytes {
		// The update is stored; a document that cannot compact now is read
		// again, and compacted, later.
		if n.liveCompact(ctx, entry, &next) != nil {
			sync.stale[v.Path] = true
		}
	}
	n.live[v.Path] = &next
	n.liveAnswered(&next, liveAnswerFrom(ctx).Tab, since, true)
	return n.liveOutput(ctx, &next, since)
}

// errLiveMissing refuses an update that builds on structures the document
// does not hold.
var errLiveMissing = errors.New("Yjs update has missing dependencies; resend complete state")

func (n *dbNotebook) LiveApply(ctx context.Context, in LiveUpdate) (LiveDocument, error) {
	ctx = n.mutationContext(ctx)

	n.mu.Lock()
	defer n.mu.Unlock()
	if err := n.checkMutationTarget(ctx, in.Path, false); err != nil {
		return LiveDocument{}, err
	}
	if len(in.Update) > dbLiveStateLimit(in.Path) {
		return LiveDocument{}, fmt.Errorf("%w: Yjs update exceeds its limit", ErrLiveOperationRejected)
	}
	key, request, err := liveOperation(ctx, in.OperationID, in)
	if err != nil {
		return LiveDocument{}, err
	}
	if err = n.liveWritable(ctx, in.Path); err != nil {
		return LiveDocument{}, err
	}
	since := liveAnswerFrom(ctx).Since
	for reread := false; ; reread = true {
		v, err := n.liveRecord(ctx, in.Path)
		if err != nil {
			return LiveDocument{}, err
		}
		if replay, err := n.liveReplay(ctx, key, request); err != nil {
			return LiveDocument{}, err
		} else if replay {
			return n.liveOutput(ctx, v, since)
		}
		if in.Generation != v.Generation {
			return LiveDocument{}, fmt.Errorf("note was saved, moved, imported or restored; reconcile the retained draft: %w", ErrConflict)
		}
		doc, err := v.takeDoc()
		if err != nil {
			n.liveDiscard(v)
			return LiveDocument{}, err
		}
		prior := doc.Version()
		if err = doc.Apply(in.Update); err != nil {
			n.liveDiscard(v)
			// A browser whose requests reach several gateways builds on
			// updates another gateway stored and this one has not heard of
			// yet: read the stored document, which has them, once.
			if errors.Is(err, errLiveMissing) && !reread {
				n.liveSync().stale[in.Path] = true
				continue
			}
			return LiveDocument{}, err
		}
		delta, err := doc.EncodeSince(prior)
		if err != nil {
			n.liveDiscard(v)
			return LiveDocument{}, err
		}
		return n.acceptLive(ctx, v, doc, delta, key, request, since)
	}
}

func (n *dbNotebook) LivePatch(ctx context.Context, in TargetedPatch) (LiveDocument, error) {
	ctx = n.mutationContext(ctx)

	n.mu.Lock()
	defer n.mu.Unlock()
	if err := n.checkMutationTarget(ctx, in.Path, false); err != nil {
		return LiveDocument{}, err
	}
	if len(in.Edits) < 1 || len(in.Edits) > 50 {
		return LiveDocument{}, fmt.Errorf("%w: patch requires 1–50 targeted edits", ErrLiveOperationRejected)
	}
	key, request, err := liveOperation(ctx, in.OperationID, in)
	if err != nil {
		return LiveDocument{}, err
	}
	if err = n.liveWritable(ctx, in.Path); err != nil {
		return LiveDocument{}, err
	}
	v, err := n.liveRecord(ctx, in.Path)
	if err != nil {
		return LiveDocument{}, err
	}
	if replay, err := n.liveReplay(ctx, key, request); err != nil {
		return LiveDocument{}, err
	} else if replay {
		return n.liveOutput(ctx, v, "")
	}
	var base []byte
	for _, version := range v.Versions {
		if version.Revision == in.Base {
			base = version.State
			break
		}
	}
	if base == nil {
		return LiveDocument{}, fmt.Errorf("live base expired or was replaced; read the live note and reconcile: %w", ErrConflict)
	}
	doc, err := v.takeDoc()
	if err != nil {
		n.liveDiscard(v)
		return LiveDocument{}, err
	}
	prior := doc.Version()
	delta, err := doc.Patch(base, in.Edits)
	if err != nil {
		// A patch that does not apply changes nothing (liveDoc.Patch).
		v.doc = doc
		return LiveDocument{}, err
	}
	if len(delta) == 0 {
		// An edit that changes nothing still records its operation.
		if delta, err = doc.EncodeSince(prior); err != nil {
			n.liveDiscard(v)
			return LiveDocument{}, err
		}
	}
	return n.acceptLive(ctx, v, doc, delta, key, request, "")
}

// dbLiveCheckpointIdentity is the checkpoint operation of v's pending edits.
// It binds v's text rather than its encoded state, so every gateway that
// loads the document finds the same one.
func dbLiveCheckpointIdentity(v *liveRecord) operationIdentity {
	request := ContentHash([]byte(v.Content))
	return operationIdentity{ID: ContentHash([]byte(v.Path + "\x00" + v.revision() + "\x00" + request + "\x00" + strings.Join(v.Uncommitted, "\n"))), Request: request}
}

// checkpointLive commits a note's accepted live edits, with their receipts
// in the commit message, records the commit in its live document and
// returns that. clean calls it only when edits are pending. Callers hold mu.
func (n *dbNotebook) checkpointLive(ctx context.Context, path string) (*liveRecord, error) {
	// Commit what the store holds, which another gateway may have added to.
	n.liveSync().stale[path] = true
	v, err := n.liveRecord(ctx, path)
	if err != nil {
		return nil, err
	}
	if len(v.Uncommitted) == 0 {
		return v, nil
	}
	if err = validateContainerWrite([]byte(v.Content)); err != nil {
		return nil, err
	}
	n.checkpointing = true
	defer func() { n.checkpointing = false }()
	if err = n.clean(ctx); err != nil {
		return nil, err
	}
	// Checkpointing only materializes edits already authorized and durable.
	// Lock/revocation after acceptance does not erase those accepted edits.
	summary := "Checkpoint live edits to " + strings.NewReplacer("\r", " ", "\n", " ").Replace(path)
	var audit strings.Builder
	for _, id := range v.Uncommitted {
		r := v.Receipts[id]
		raw, _ := json.Marshal(struct {
			Operation string      `json:"operation"`
			Receipt   liveReceipt `json:"receipt"`
		}{id, r})
		audit.WriteString("Collaboration-Receipt: ")
		audit.Write(raw)
		audit.WriteByte('\n')
	}
	op := dbLiveCheckpointIdentity(v)
	ctx = context.WithValue(context.WithValue(ctx, operationKey{}, op), collaborationAuditKey{}, audit.String())
	ctx = attribution.With(ctx, attribution.Identity{Name: "ForgetMeNote collaboration", Email: "forgetmenote@localhost", Source: "collaboration"})
	// Even a no-op edit checkpoints its receipt in Git.
	prior, err := n.verifiedLiveCheckpoint(ctx, v)
	if err != nil {
		return nil, err
	}
	if prior.Revision == "" {
		head, err := n.head(ctx)
		if err != nil {
			return nil, err
		}
		if _, err = n.applyChanges(ctx, head, summary, []pathChange{{Path: path, Content: []byte(v.Content)}}); err != nil {
			return nil, err
		}
	}
	// The commit stands. What follows records it; a failure leaves the edits
	// pending, for a retry or a restart to reconcile with that commit.
	ctx = context.WithoutCancel(ctx)
	current, err := n.read(ctx, path, "")
	if err != nil {
		return nil, err
	}
	if string(current.Content) != v.Content {
		return nil, fmt.Errorf("checkpoint does not contain the accepted live text: %w", ErrConflict)
	}
	entry, err := n.liveEntry(ctx, path)
	if err != nil {
		return nil, err
	}
	if err = n.archiveLiveReceipts(ctx, v); err != nil {
		return nil, err
	}
	next := *v
	next.GitVersion = current.Version
	next.CheckpointContent = next.Content
	next.Uncommitted = nil
	next.Receipts = map[string]liveReceipt{}
	if len(next.state()) > dbLiveRestartBytes(len(next.Content)) {
		// Checkpointed Markdown and permanent receipts survive. A new
		// generation makes stale clients reconcile their retained local drafts.
		fresh, err := n.liveStart(ctx, entry, current)
		if err != nil {
			return nil, err
		}
		next = *fresh
	} else if err = n.liveCompact(ctx, entry, &next); err != nil {
		return nil, err
	}
	n.live[path] = &next
	n.notifyLive(path)
	return &next, nil
}

// verifiedLiveCheckpoint finds the commit of v's checkpoint, or an empty
// revision when there is none. Both checkpoint retries and recovery require
// the same exact operation, request and content.
func (n *dbNotebook) verifiedLiveCheckpoint(ctx context.Context, v *liveRecord) (Document, error) {
	op := dbLiveCheckpointIdentity(v)
	info, ok, err := n.operationCommit(ctx, op.ID)
	if err != nil || !ok {
		return Document{}, err
	}
	committed, readErr := n.read(ctx, v.Path, info.ID.String())
	if trailerValue(info.Trailers, notestore.OperationTrailer) != op.ID || trailerValue(info.Trailers, notestore.RequestTrailer) != op.Request || readErr != nil || string(committed.Content) != v.Content {
		return Document{}, fmt.Errorf("checkpoint marker does not match the live document: %w", ErrConflict)
	}
	return committed, nil
}

// archiveLiveReceipts keeps v's receipts once their edits are committed, as
// the notes store's receipts, so identical retries stay replays.
func (n *dbNotebook) archiveLiveReceipts(ctx context.Context, v *liveRecord) error {
	for key, receipt := range v.Receipts {
		if _, err := hex.DecodeString(key); len(key) != 64 || err != nil {
			return errors.New("invalid live operation receipt key")
		}
		raw, _ := json.Marshal(receipt)
		err := n.notes.PutReceipt(ctx, notestore.Receipt{Kind: liveReceiptKind, Operation: key, RequestHash: receipt.Request, State: notestore.ReceiptDone, Result: raw})
		if errors.Is(err, notestore.ErrOperationMismatch) {
			return fmt.Errorf("operation receipt collision: %w", ErrConflict)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// liveReplay reports whether operation key already took effect for
// request, and refuses its reuse for another request.
func (n *dbNotebook) liveReplay(ctx context.Context, key, request string) (bool, error) {
	found := false
	// Pending documents are never evicted. Search the notebook, not only the
	// requested note, because operation IDs and permanent receipts are global.
	for _, record := range n.live {
		if receipt, ok := record.Receipts[key]; ok {
			if receipt.Request != request {
				return false, ErrConflict
			}
			found = true
		}
	}
	receipt, ok, err := n.notes.Receipt(ctx, liveReceiptKind, key)
	if err != nil || !ok {
		return found, err
	}
	if receipt.RequestHash != request {
		return false, ErrConflict
	}
	return true, nil
}

func (n *dbNotebook) LiveCheckpoint(ctx context.Context, path string) (LiveDocument, error) {
	ctx = n.mutationContext(ctx)

	n.mu.Lock()
	defer n.mu.Unlock()
	if err := n.checkMutationTarget(ctx, path, false); err != nil {
		return LiveDocument{}, err
	}
	if err := n.loadLocks(ctx); err != nil {
		return LiveDocument{}, err
	}
	if _, err := n.resolve(path, true); err != nil {
		return LiveDocument{}, err
	}
	v, err := n.checkpointLive(ctx, path)
	if err != nil {
		return LiveDocument{}, err
	}
	// The checkpoint woke every long poll on the note; the tab that asked
	// for it gets it in this answer.
	answer := liveAnswerFrom(ctx)
	n.liveAnswered(v, answer.Tab, answer.Since, false)
	return n.liveOutput(ctx, v, answer.Since)
}

// WaitLive waits outside the notebook lock. Each request performs one
// validated catch-up read, then waits for this store's or another gateway's
// change to the document, or for the end of the bounded request. incremental
// is LivePoll's. A long poll of a browser tab (LiveAnswer.Tab) is not
// answered with that tab's own updates (liveAnswered).
func (n *dbNotebook) WaitLive(ctx context.Context, path, known string, milliseconds int, incremental bool) (LiveDocument, error) {
	if milliseconds < 0 || milliseconds > 25000 {
		return LiveDocument{}, errors.New("live wait must be between 0 and 25000 milliseconds")
	}
	if milliseconds == 0 || known == "" {
		return n.LivePoll(ctx, path, known, incremental)
	}

	n.mu.Lock()
	if n.liveClosed {
		n.mu.Unlock()
		return LiveDocument{}, context.Canceled
	}
	if _, err := n.resolve(path, false); err != nil {
		n.mu.Unlock()
		return LiveDocument{}, err
	}
	if n.liveWaiting >= maxLiveWaiters {
		n.mu.Unlock()
		return LiveDocument{}, ErrLiveCapacity
	}
	if n.liveWaits == nil {
		n.liveWaits = map[string]*liveWaiter{}
	}
	waiter := n.liveWaits[path]
	if waiter == nil {
		waiter = &liveWaiter{readers: map[*liveReader]bool{}}
		n.liveWaits[path] = waiter
	}
	tab := liveAnswerFrom(ctx).Tab
	reader := &liveReader{tab: tab, known: n.liveTabKnown(path, tab, known), changed: make(chan struct{})}
	waiter.readers[reader] = true
	n.liveWaiting++
	n.mu.Unlock()
	defer func() {
		n.mu.Lock()
		defer n.mu.Unlock()
		delete(waiter.readers, reader)
		n.liveWaiting--
		if len(waiter.readers) == 0 && n.liveWaits[path] == waiter {
			delete(n.liveWaits, path)
		}
	}()
	timer := time.NewTimer(time.Duration(milliseconds) * time.Millisecond)
	defer timer.Stop()
	for {
		// What the reader knows and its wake-up are read with the poll, so
		// a change between them wakes it.
		n.mu.Lock()
		hint := reader.changed
		value, err := n.livePoll(ctx, path, reader.known, incremental)
		n.mu.Unlock()
		if err != nil || !value.Unchanged {
			return value, err
		}
		select {
		case <-ctx.Done():
			return LiveDocument{}, ctx.Err()
		case <-timer.C:
			return LiveDocument{Unchanged: true}, nil
		case <-hint:
			n.mu.Lock()
			closed := n.liveClosed
			n.mu.Unlock()
			if closed {
				return LiveDocument{}, context.Canceled
			}
		}
	}
}

// liveRecover holds the notebook's stored documents with pending edits the
// first time this store writes, so the write checkpoints them first although
// nobody has used them since the gateway started; later changes arrive as the
// notes store's live hints. Callers hold mu.
func (n *dbNotebook) liveRecover(ctx context.Context) error {
	sync := n.liveSync()
	if sync.recovered {
		return nil
	}
	if _, err := n.livePending(ctx); err != nil {
		return err
	}
	sync.recovered = true
	return nil
}

// livePending holds the stored documents as liveAdopt does, so this store
// holds every stored document with pending edits and none whose edits
// another gateway has checkpointed, and returns the sum of the stored
// documents' sequence numbers, which every update stored later raises
// (notestore.AuthorityWrite.ExpectLive). Callers hold mu.
func (n *dbNotebook) livePending(ctx context.Context) (int64, error) {
	docs, err := n.notes.LiveDocs(ctx)
	if err != nil {
		return 0, err
	}
	var entries map[string]string
	var sum int64
	for _, d := range docs {
		sum += d.LastSeq
		if !liveDocPending(d) && !n.hasLivePending() {
			continue
		}
		if entries == nil {
			if entries, err = n.liveEntries(ctx); err != nil {
				return 0, err
			}
		}
		if err = n.liveAdopt(ctx, entries[d.EntryID], d); err != nil {
			return 0, err
		}
	}
	return sum, nil
}

// liveDocPending reports whether d holds edits waiting for a checkpoint:
// updates after its snapshot, or a snapshot without a base revision.
func liveDocPending(d notestore.LiveDoc) bool {
	return d.LastSeq != d.SnapshotSeq || d.Snapshot && d.Base.IsZero()
}

// liveAdopt holds path's document as d stores it when this store holds it
// as it was before another gateway changed it: d's pending edits when this
// store holds none of them, or no pending edits once another gateway
// checkpointed the ones this store holds. Pending edits held here while d
// has some too, and held ones d no longer reaches, are liveRecord's to
// reconcile. Callers hold mu.
func (n *dbNotebook) liveAdopt(ctx context.Context, path string, d notestore.LiveDoc) error {
	if path == "" {
		return nil
	}
	held := n.live[path]
	if liveDocPending(d) {
		if held != nil && (len(held.Uncommitted) > 0 || dbLiveEpoch(held) == d.Epoch && int64(held.Sequence) >= d.LastSeq) {
			return nil
		}
	} else if held == nil || len(held.Uncommitted) == 0 || dbLiveEpoch(held) == d.Epoch && d.LastSeq < int64(held.Sequence) {
		return nil
	}
	v, _, err := n.liveLoad(ctx, path, d.EntryID)
	if err != nil || v == nil || v.Path != path || held == nil && len(v.Uncommitted) == 0 {
		return err
	}
	if held != nil && held.Generation == v.Generation {
		v.tabs = held.tabs
	}
	n.live[path] = v
	if err = n.trimLive(""); err != nil {
		if held != nil {
			n.live[path] = held
		} else {
			delete(n.live, path)
		}
	}
	return err
}

// liveEntries maps the entry ID of each note at the head to its path.
func (n *dbNotebook) liveEntries(ctx context.Context) (map[string]string, error) {
	files, err := n.notes.Files(ctx, gitobj.ID{})
	if err != nil {
		return nil, err
	}
	entries := make(map[string]string, len(files))
	for _, f := range files {
		if strings.HasSuffix(f.Path, ".md") && f.Path != "AGENTS.md" {
			entries[f.EntryID] = f.Path
		}
	}
	return entries, nil
}

// The stores open on each notebook, for the notes store's live hints. A
// notes store's watcher cannot be removed, so each gets one that dispatches
// to the stores open now; a closed store leaves with the next hint.
var dbLiveWatches struct {
	sync.Mutex
	stores map[*notestore.Store]*dbLiveRegistry
}

type dbLiveRegistry struct {
	mu        sync.Mutex
	notebooks map[string]map[*dbNotebook]*dbLiveSync
}

// dbLiveSync is what a store knows of other gateways' live changes: the
// loaded documents they changed since, and whether it has held the stored
// documents with pending edits (liveRecover). The store's mu guards it.
type dbLiveSync struct {
	stale     map[string]bool
	recovered bool
}

// liveSync returns this store's dbLiveSync, first registering the store for
// the notes store's live hints. Callers hold mu.
func (n *dbNotebook) liveSync() *dbLiveSync {
	dbLiveWatches.Lock()
	if dbLiveWatches.stores == nil {
		dbLiveWatches.stores = map[*notestore.Store]*dbLiveRegistry{}
	}
	r := dbLiveWatches.stores[n.noteStore]
	if r == nil {
		r = &dbLiveRegistry{notebooks: map[string]map[*dbNotebook]*dbLiveSync{}}
		dbLiveWatches.stores[n.noteStore] = r
		n.noteStore.WatchLive(r.changed)
	}
	dbLiveWatches.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	stores := r.notebooks[n.notebook]
	if stores == nil {
		stores = map[*dbNotebook]*dbLiveSync{}
		r.notebooks[n.notebook] = stores
	}
	s := stores[n]
	if s == nil {
		s = &dbLiveSync{stale: map[string]bool{}}
		stores[n] = s
	}
	return s
}

// changed hands a hint to the stores open on its notebook, or on every
// notebook after the listener reconnected. It takes each store's lock, so it
// collects them first and holds no registry lock meanwhile.
func (r *dbLiveRegistry) changed(notebook string, c notestore.LiveChange) {
	type target struct {
		n    *dbNotebook
		sync *dbLiveSync
	}
	var targets []target
	r.mu.Lock()
	for id, stores := range r.notebooks {
		if notebook == "" || id == notebook {
			for n, s := range stores {
				targets = append(targets, target{n, s})
			}
		}
	}
	r.mu.Unlock()
	for _, t := range targets {
		t.n.mu.Lock()
		if t.n.liveClosed {
			r.mu.Lock()
			delete(r.notebooks[t.n.notebook], t.n)
			if len(r.notebooks[t.n.notebook]) == 0 {
				delete(r.notebooks, t.n.notebook)
			}
			r.mu.Unlock()
		} else {
			t.n.liveHeard(t.sync, c)
		}
		t.n.mu.Unlock()
	}
}

// liveHeard marks the loaded document a hint changed stale, unless this
// store made the change, and wakes its waiters. A document it does not hold
// is loaded when it has pending edits, so writes here checkpoint them first.
// Callers hold mu.
func (n *dbNotebook) liveHeard(sync *dbLiveSync, c notestore.LiveChange) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if c.EntryID == "" {
		// Hints may have been missed: read every document again.
		for path := range n.live {
			sync.stale[path] = true
		}
		n.livePending(ctx)
		n.notifyLive("")
		return
	}
	for path, v := range n.live {
		if f, err := n.notes.Stat(ctx, gitobj.ID{}, path); err != nil || f.EntryID != c.EntryID {
			continue
		}
		current := dbLiveEpoch(v) == c.Epoch && int64(v.Sequence) >= c.Seq && (c.Base.IsZero() || v.GitVersion == c.Base.String() && len(v.Uncommitted) == 0)
		if !current {
			sync.stale[path] = true
			n.notifyLive(path)
		}
		return
	}
	// Only an update or a compaction of pending edits has no base revision.
	if c.Base.IsZero() {
		if entries, err := n.liveEntries(ctx); err == nil {
			n.liveAdopt(ctx, entries[c.EntryID], notestore.LiveDoc{EntryID: c.EntryID, Epoch: c.Epoch, LastSeq: c.Seq})
		}
	}
}
