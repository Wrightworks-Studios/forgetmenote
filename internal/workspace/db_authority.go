package workspace

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"time"

	"forgetmenote/internal/attribution"
	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
)

// A notebook keeps its authority handoff in the notes store's authority
// record: the receipt, and while a handoff to ForgetMeNote is prepared, the
// validated snapshot it installs. Every gateway reads that one record, so a
// handoff one gateway prepares fences writes through all of them. The
// catalog installs the generation requests are fenced on and whether the
// notebook is read-only; a record changes them only when this gateway moves
// the handoff on.

// loadAuthority reads the notebook's record into authority, and refuses one
// this service did not write. Callers hold mu.
func (n *dbNotebook) loadAuthority(ctx context.Context) (notestore.Authority, error) {
	record, err := n.notes.Authority(ctx)
	if err != nil {
		return record, err
	}
	var state AuthorityState
	if record.Version != 0 && (json.Unmarshal(record.State, &state) != nil || !validAuthorityState(state)) {
		return record, errInvalidAuthority
	}
	n.authority, n.authorityVersion = state, record.Version
	return record, nil
}

// checkStoredAuthority is checkAuthority's view of the record: a GitHub
// notebook is read-only, whatever this gateway was told, and a prepared
// handoff fences writes. Callers hold mu.
func (n *dbNotebook) checkStoredAuthority(ctx context.Context) error {
	if _, err := n.loadAuthority(ctx); err != nil {
		return err
	}
	if n.authority.Type == "github" {
		return ErrReadOnly
	}
	return nil
}

// currentAuthority is authorityState for this backend: a notebook without a
// record, such as one brought over from a working tree, is at the
// generation the catalog installed.
func (n *dbNotebook) currentAuthority() AuthorityState {
	out := n.authorityState()
	if n.authority.Generation == 0 {
		out.Generation = max(1, n.authorityGeneration.Load())
	}
	return out
}

// errPendingFilings refuses a handoff while a filing is in flight, which
// could neither finish nor be canceled in a notebook handed to GitHub.
var errPendingFilings = errors.New("finish pending cross-notebook moves before changing authority")

// storeAuthority replaces record with state and stage, nil dropping the
// stage, and then applies state as saveAuthority does. With head set, the
// record is stored only while the notebook is at head, and no revision can
// land until it is. Callers hold mu.
func (n *dbNotebook) storeAuthority(ctx context.Context, record notestore.Authority, state AuthorityState, stage []byte, head string) error {
	return n.putAuthority(ctx, notestore.AuthorityWrite{Version: record.Version, Stage: stage}, state, head)
}

// putAuthority stores w with state as storeAuthority does. Callers hold mu.
func (n *dbNotebook) putAuthority(ctx context.Context, w notestore.AuthorityWrite, state AuthorityState, head string) error {
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	w.State = raw
	if head != "" {
		id, err := parseRevision(head)
		if err != nil {
			return err
		}
		w.ExpectHead = &id
	}
	version, err := n.notes.PutAuthority(ctx, w)
	if err != nil {
		if errors.Is(err, notestore.ErrPending) {
			return errPendingFilings
		}
		if errors.Is(err, notestore.ErrLiveChanged) {
			return ErrLivePending
		}
		if errors.Is(err, notestore.ErrConflict) {
			return fmt.Errorf("%w: %v", ErrConflict, err)
		}
		return err
	}
	n.authority, n.authorityVersion = state, version
	n.authorityGeneration.Store(state.Generation)
	n.readOnly.Store(state.Type == "github")
	return nil
}

func (n *dbNotebook) AuthorityStatus(ctx context.Context) (AuthorityState, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if _, err := n.loadAuthority(ctx); err != nil {
		return AuthorityState{}, err
	}
	return n.currentAuthority(), nil
}

func (n *dbNotebook) PrepareAuthority(ctx context.Context, in AuthorityRequest) (AuthorityState, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	record, err := n.loadAuthority(ctx)
	if err != nil {
		return AuthorityState{}, err
	}
	state := n.currentAuthority()
	// Initial notebook Kind belongs to the catalog; subsequent handoffs must
	// match the durable receipt and generation exactly.
	if record.Version == 0 && in.Generation == 1 && in.From == "github" {
		state.Type = "github"
	}
	if state.Transfer == in.ID && state.Phase == "prepared" && state.Generation == in.Generation && state.To == in.To {
		return state, nil
	}
	if !ValidNoteID(in.ID) || in.From == in.To || (in.To != "github" && in.To != "forgetmenote") || in.From != state.Type || in.Generation != state.Generation || state.Phase == "prepared" {
		return state, ErrConflict
	}
	// Live edits refuse the handoff as the store holds them, whichever
	// gateway accepted or checkpointed them, also before its hint arrives.
	// One stored after this look refuses the record (ExpectLive), and the
	// record refuses any later one (acceptLive).
	live, err := n.livePending(ctx)
	if err != nil {
		return state, err
	}
	if n.hasLivePending() {
		return state, ErrLivePending
	}
	if err := n.clean(ctx); err != nil {
		return state, err
	}
	head, err := n.head(ctx)
	if err != nil {
		return state, err
	}
	if head != in.Revision {
		return state, ErrConflict
	}
	// A filing in flight, whichever gateway reserved it, refuses the handoff
	// under the same lock that orders it against new reservations
	// (persistCapture), which the prepared record then fences. So none is in
	// flight while the handoff is prepared, and none can start before it
	// completes or aborts; FinishAuthority, which publication makes
	// irreversible, need not check again.
	state.Transfer, state.Phase, state.To, state.Revision, state.StageHash, state.Committed = in.ID, "prepared", in.To, head, "", ""
	return state, n.putAuthority(ctx, notestore.AuthorityWrite{Version: record.Version, RefusePending: captureReceiptKind, ExpectLive: &live}, state, head)
}

func (n *dbNotebook) StageAuthority(ctx context.Context, id string, snapshot NotebookSnapshot) (AuthorityState, error) {
	if err := ValidateNotebookSnapshot(ctx, snapshot); err != nil {
		return AuthorityState{}, err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	record, err := n.loadAuthority(ctx)
	if err != nil {
		return AuthorityState{}, err
	}
	state := n.currentAuthority()
	if state.Transfer != id || state.Phase != "prepared" || state.To != "forgetmenote" {
		return state, ErrConflict
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return state, err
	}
	hash := ContentHash(raw)
	if state.StageHash != "" && state.StageHash != hash {
		return state, ErrConflict
	}
	if state.StageHash == hash && record.Staged {
		return state, nil
	}
	state.StageHash = hash
	return state, n.storeAuthority(ctx, record, state, raw, "")
}

func (n *dbNotebook) FinishAuthority(ctx context.Context, id string, commit bool) (AuthorityState, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	record, err := n.loadAuthority(ctx)
	if err != nil {
		return AuthorityState{}, err
	}
	state := n.currentAuthority()
	if state.Transfer != id {
		return state, ErrConflict
	}
	if state.Phase == "complete" || state.Phase == "aborted" {
		if commit != (state.Phase == "complete") {
			return state, ErrConflict
		}
		if !record.Staged {
			return state, nil
		}
		// Retain the small receipt, not a second content store.
		return state, n.storeAuthority(ctx, record, state, nil, "")
	}
	if state.Phase != "prepared" {
		return state, ErrConflict
	}
	if !commit {
		state.Phase = "aborted"
		state.StageHash = ""
		return state, n.storeAuthority(ctx, record, state, nil, "")
	}
	if err := n.clean(ctx); err != nil {
		return state, err
	}
	head, err := n.head(ctx)
	if err != nil {
		return state, err
	}
	if state.To == "forgetmenote" {
		if head, err = n.installAuthorityStage(ctx, record, state, head); err != nil {
			return state, err
		}
	} else if head != state.Revision {
		return state, ErrConflict
	}
	state.Type = state.To
	state.Generation++
	state.Phase = "complete"
	state.Committed = head
	return state, n.storeAuthority(ctx, record, state, nil, head)
}

// installAuthorityStage commits the staged snapshot over the prepared
// revision, or finds the import an earlier attempt committed, and returns
// the head. Callers hold mu.
func (n *dbNotebook) installAuthorityStage(ctx context.Context, record notestore.Authority, state AuthorityState, head string) (string, error) {
	raw, err := n.notes.AuthorityStage(ctx, record.Version)
	if errors.Is(err, notestore.ErrConflict) {
		return "", fmt.Errorf("%w: %v", ErrConflict, err)
	}
	if err != nil {
		return "", err
	}
	if raw == nil {
		return "", fmt.Errorf("staged notebook snapshot: %w", fs.ErrNotExist)
	}
	if len(raw) > MaxAuthorityBytes*2 {
		return "", errors.New("invalid notebook authority file")
	}
	if state.StageHash == "" || ContentHash(raw) != state.StageHash {
		return "", ErrConflict
	}
	var snapshot NotebookSnapshot
	if json.Unmarshal(raw, &snapshot) != nil {
		return "", ErrConflict
	}
	if head != state.Revision {
		// A committed import may precede its completion receipt. Never overwrite a
		// later commit or guess whether an unrelated mutation was our transaction.
		id, err := parseRevision(head)
		if err != nil {
			return "", ErrConflict
		}
		info, err := n.notes.Commit(ctx, id)
		message := info.Commit.Message
		if err != nil || len(info.Commit.Parents) == 0 || info.Commit.Parents[0].String() != state.Revision || !strings.Contains(message, "Gitnotes-Operation: "+state.Transfer+"\n") || !strings.Contains(message, "Gitnotes-Request: "+state.StageHash+"\n") {
			return "", ErrConflict
		}
		return head, nil
	}
	previous, err := n.notebookSnapshot(ctx)
	if err != nil {
		return "", err
	}
	changes := map[string][]byte{}
	for p := range previous.Files {
		if _, ok := snapshot.Files[p]; !ok {
			changes[p] = nil
		}
	}
	for p, b := range snapshot.Files {
		if old, ok := previous.Files[p]; !ok || !bytes.Equal(old, b) {
			changes[p] = append([]byte{}, b...)
		}
	}
	if len(changes) > 0 {
		// The fenced notebook installs the validated complete tree as one
		// revision. It preserves bytes and IDs instead of treating this as edits.
		ctx = context.WithValue(ctx, operationKey{}, operationIdentity{ID: state.Transfer, Request: state.StageHash})
		summary := "Import notebook from " + snapshot.Repository + " at " + snapshot.Revision
		if err = n.commitAuthorityImport(ctx, head, changes, summary, record.Version); err != nil {
			return "", err
		}
	}
	return n.head(ctx)
}

// commitAuthorityImport commits a handoff's import over base, past the
// mutation checks the fence would refuse and keeping bytes and identities as
// they are, with the commit message every revision gets. A note new to the notebook keeps its fmn-id
// as its identity in the store, so a note renamed upstream keeps its
// history, unless a note the import keeps already has it. It commits only
// while the authority record is still at authority, the version of the
// prepared and staged record. Callers hold mu.
func (n *dbNotebook) commitAuthorityImport(ctx context.Context, base string, changes map[string][]byte, summary string, authority int64) error {
	head, err := parseRevision(base)
	if err != nil || head.IsZero() {
		return ErrConflict
	}
	current, err := n.files(ctx, base)
	if err != nil {
		return err
	}
	exists, kept := map[string]bool{}, map[string]bool{}
	for _, f := range current {
		exists[f.Path] = true
		if after, changing := changes[f.Path]; !changing || after != nil {
			kept[f.EntryID] = true
		}
	}
	var files []notestore.FileChange
	for _, c := range orderedChanges(changes, nil) {
		if c.Path != locksPath && !trashFile(c.Path) {
			if _, err := n.resolve(c.Path, false); err != nil {
				return err
			}
		}
		f := notestore.FileChange{Path: c.Path, Content: c.Content}
		if c.Content != nil && !exists[c.Path] {
			if id := NoteEntryID(c.Path, c.Content); id != "" && !kept[id] {
				f.EntryID, kept[id] = id, true
			}
		}
		files = append(files, f)
	}
	subject, body, err := commitMessage(ctx, summary)
	if err != nil {
		return err
	}
	cs := notestore.ChangeSet{ExpectHead: &head, Files: files, Author: signature(attribution.From(ctx), time.Now()), Subject: subject, Body: body, AllowEmpty: true, ExpectAuthority: &authority}
	if op, ok := ctx.Value(operationKey{}).(operationIdentity); ok {
		cs.OperationID, cs.RequestID = op.ID, op.Request
	}
	result, err := n.notes.Apply(ctx, cs)
	switch {
	case errors.Is(err, notestore.ErrConflict), errors.Is(err, notestore.ErrOperationMismatch):
		return fmt.Errorf("%w: %v", ErrConflict, err)
	case err != nil:
		return err
	}
	n.observe(result.Head, false)
	return nil
}

func (n *dbNotebook) NotebookSnapshot(ctx context.Context) (NotebookSnapshot, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if _, err := n.livePending(ctx); err != nil {
		return NotebookSnapshot{}, err
	}
	if n.hasLivePending() {
		return NotebookSnapshot{}, ErrLivePending
	}
	if err := n.clean(ctx); err != nil {
		return NotebookSnapshot{}, err
	}
	return n.notebookSnapshot(ctx)
}

// notebookSnapshot reads every file at the head within the handoff's
// budgets, refusing any but a regular file, as the Git backend refused what
// ls-tree listed as anything else. Callers hold mu.
func (n *dbNotebook) notebookSnapshot(ctx context.Context) (NotebookSnapshot, error) {
	head, err := n.head(ctx)
	out := NotebookSnapshot{Revision: head, Files: map[string][]byte{}}
	if err != nil {
		return out, err
	}
	files, err := n.files(ctx, head)
	if err != nil {
		return out, err
	}
	var total int64
	limits := make([]int, len(files))
	for i, f := range files {
		if f.Mode != gitobj.ModeFile {
			return out, invalidInput("handoff does not support executable files, symlinks or submodules")
		}
		if f.Size < 0 || f.Size > int64(MaxReadBytes(f.Path)) || total+f.Size > MaxAuthorityBytes || i >= 10000 {
			return out, invalidInput("handoff exceeds the configured file/source byte budget, 64 MiB total, or 10,000 files")
		}
		total += f.Size
		limits[i] = MaxReadBytes(f.Path)
	}
	err = n.readBlobsBounded(ctx, files, limits, func(i int, b []byte) { out.Files[files[i].Path] = b })
	return out, err
}
