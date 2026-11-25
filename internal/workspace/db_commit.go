package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"time"

	"forgetmenote/internal/attribution"
	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
)

// A pathChange is one path a revision writes, deletes or renames.
type pathChange struct {
	Path string
	// Content is the new content; nil deletes Path unless From is set.
	Content []byte
	// From renames that path to Path, keeping its identity and history, and
	// its content when Content is nil.
	From string
}

// orderedChanges turns a mutation's changes, keyed by path with nil for a
// deletion, and its renames, each destination to its source, into the order
// a revision applies them: renames, then deletions, then writes, each by
// path. A rename's destination keeps its content from changes, and its
// source's nil entry is the rename.
func orderedChanges(changes map[string][]byte, renames map[string]string) []pathChange {
	var moves, deletes, writes []pathChange
	sources := map[string]bool{}
	for to, from := range renames {
		moves = append(moves, pathChange{Path: to, From: from, Content: changes[to]})
		sources[from] = true
	}
	for p, content := range changes {
		switch {
		case renames[p] != "" || sources[p] && content == nil:
		case content == nil:
			deletes = append(deletes, pathChange{Path: p})
		default:
			writes = append(writes, pathChange{Path: p, Content: content})
		}
	}
	for _, list := range [][]pathChange{moves, deletes, writes} {
		slices.SortFunc(list, func(a, b pathChange) int { return strings.Compare(a.Path, b.Path) })
	}
	return slices.Concat(moves, deletes, writes)
}

// applyChanges is the transaction owner for every write of a notebook kept
// in the notes store, as the Git backend's was for its own. Callers hold
// mu, check permissions, locks and base revisions against base, the head they
// read under mu, and leave live edits checkpointed. It commits on base alone,
// so a write another gateway made meanwhile fails it with ErrConflict, and
// always commits, as git commit --allow-empty does.
//
// Callers check locks against the rules loadLocks last read, so base must
// hold those very rules: a lock another gateway committed after the check
// and before the head a caller commits on fails the commit with ErrConflict
// instead of being bypassed. A live checkpoint commits edits accepted
// before any later lock, and is exempt.
func (n *dbNotebook) applyChanges(ctx context.Context, base, summary string, changes []pathChange) (notestore.CommitResult, error) {
	head, err := parseRevision(base)
	if err != nil || head.IsZero() {
		return notestore.CommitResult{}, ErrConflict
	}
	if _, checkpoint := ctx.Value(collaborationAuditKey{}).(string); !checkpoint {
		if err := n.checkLockRulesAt(ctx, head); err != nil {
			return notestore.CommitResult{}, err
		}
	}
	// The mutation as its checks see it: each path's content after it, nil
	// for one it removes. A path is written once, and moved away once before
	// anything is written there.
	after := map[string][]byte{}
	written, moved := map[string]bool{}, map[string]bool{}
	// Each path's Version at base, which the store checks again under the
	// head's row lock with base itself.
	versions := map[string]gitobj.ID{}
	files := make([]notestore.FileChange, len(changes))
	for i, c := range changes {
		for _, p := range []string{c.Path, c.From} {
			if p == "" {
				continue
			}
			if p != locksPath && !trashFile(p) {
				if _, err := n.resolve(p, false); err != nil {
					return notestore.CommitResult{}, err
				}
			}
			if p == c.Path && (c.Content != nil || c.From != "") && !gitPath(p) {
				return notestore.CommitResult{}, ErrForbidden
			}
			if _, seen := versions[p]; !seen {
				f, err := n.notes.Stat(ctx, head, p)
				if err != nil && !errors.Is(err, fs.ErrNotExist) {
					return notestore.CommitResult{}, err
				}
				versions[p] = f.Version
			}
		}
		if written[c.Path] || c.From != "" && (written[c.From] || moved[c.From]) {
			return notestore.CommitResult{}, fmt.Errorf("%s changes twice in one revision", c.Path)
		}
		content := c.Content
		if c.From != "" {
			moved[c.From], after[c.From] = true, nil
			if content == nil {
				if content, _, err = n.notes.Read(ctx, head, c.From); err != nil {
					return notestore.CommitResult{}, err
				}
			}
		}
		written[c.Path], after[c.Path] = true, content
		files[i] = notestore.FileChange{Path: c.Path, Content: c.Content, From: c.From}
		if c.From == "" && content != nil && (versions[c.Path].IsZero() || moved[c.Path]) {
			// A new note's fmn-id is its identity in the store, so its
			// history stays one entry wherever it moves.
			files[i].EntryID = NoteEntryID(c.Path, content)
		}
	}
	if err := n.prepareIdentities(ctx, after); err != nil {
		return notestore.CommitResult{}, err
	}
	if err := n.checkMutationChanges(ctx, after); err != nil {
		return notestore.CommitResult{}, err
	}
	subject, body, err := commitMessage(ctx, summary)
	if err != nil {
		return notestore.CommitResult{}, err
	}
	// The store checks again, under the head's row lock, that the authority
	// record checkMutationChanges read is still the record: a handoff another
	// gateway prepared or completed meanwhile fences this write too.
	authority := n.authorityVersion
	cs := notestore.ChangeSet{ExpectHead: &head, ExpectVersion: versions, Files: files, Author: signature(attribution.From(ctx), time.Now()), Subject: subject, Body: body, AllowEmpty: true, ExpectAuthority: &authority}
	if op, ok := ctx.Value(operationKey{}).(operationIdentity); ok {
		cs.OperationID, cs.RequestID = op.ID, op.Request
	}
	if key, ok := ctx.Value(captureMutationKey{}).(string); ok {
		// A filing commits only while its receipt is in flight, so an abort
		// that settled it on another gateway fences this commit.
		cs.Pending = &notestore.ReceiptKey{Kind: captureReceiptKind, Operation: key}
	}
	result, err := n.notes.Apply(ctx, cs)
	switch {
	case errors.Is(err, notestore.ErrAuthorityChanged):
		return notestore.CommitResult{}, n.authorityRefusal(ctx, err)
	case errors.Is(err, notestore.ErrConflict), errors.Is(err, notestore.ErrOperationMismatch):
		return notestore.CommitResult{}, fmt.Errorf("%w: %v", ErrConflict, err)
	case err != nil:
		return notestore.CommitResult{}, err
	}
	n.observe(result.Head, false)
	return result, nil
}

// authorityRefusal explains a write the store refused because the
// notebook's authority record changed after the write checked it, cause:
// read-only once the notebook is GitHub's, forbidden while a handoff is
// prepared, and otherwise a conflict to retry. Callers hold mu.
func (n *dbNotebook) authorityRefusal(ctx context.Context, cause error) error {
	if err := n.checkStoredAuthority(ctx); err != nil {
		return err
	}
	if n.authority.Phase == "prepared" {
		return ErrForbidden
	}
	return fmt.Errorf("%w: %v", ErrConflict, cause)
}

// NoteEntryID is the identity the notes store keeps a file under: a note's
// valid fmn-id, or "" for a file without one, which gets an identity of its
// own. Imports use it too, so an imported note keeps its history as one
// entry.
func NoteEntryID(path string, content []byte) string {
	if !strings.HasSuffix(path, ".md") || path == "AGENTS.md" {
		return ""
	}
	id, err := NoteID(content)
	if err != nil {
		return ""
	}
	return id
}

// commitMessage builds the Git backend's commit message: the summary on one
// line, then the attribution, operation and collaboration audit trailers in
// the order commit writes them. With no cleanup to do, the subject and body
// become exactly what "git commit -F -" stores for the same message.
func commitMessage(ctx context.Context, summary string) (subject, body string, err error) {
	identity := attribution.From(ctx)
	// Paths are data, not commit trailers.
	subject = strings.NewReplacer("\r", " ", "\n", " ").Replace(summary)
	if strings.TrimSpace(subject) == "" {
		return "", "", errors.New("a revision needs a summary")
	}
	body = "Gitnotes-Source: " + identity.Source
	if identity.ID != "" {
		body += "\nGitnotes-User: " + identity.ID
	}
	if identity.Client != "" {
		body += "\nGitnotes-Client: " + identity.Client
	}
	if op, ok := ctx.Value(operationKey{}).(operationIdentity); ok {
		body += "\nGitnotes-Operation: " + op.ID + "\nGitnotes-Request: " + op.Request
	}
	if audit, ok := ctx.Value(collaborationAuditKey{}).(string); ok {
		body += "\n" + audit
	}
	// Git refuses a NUL in a commit message, and so does git fsck, which a
	// notebook's export and backups must pass.
	if strings.ContainsRune(subject+body, 0) {
		return "", "", invalidInput("a revision message cannot contain NUL")
	}
	return subject, body, nil
}

// signature signs a revision as Git signs a commit made now in this
// process's time zone. The store cleans the name and email as Git does.
func signature(identity attribution.Identity, now time.Time) gitobj.Signature {
	return gitobj.NewSignature(identity.Name, identity.Email, now)
}

// operationCommit finds the newest revision of operation, as
// "git log -1 --grep=Gitnotes-Operation: <id>" finds it.
func (n *dbNotebook) operationCommit(ctx context.Context, operation string) (notestore.CommitInfo, bool, error) {
	return n.notes.CommitByOperation(ctx, operation)
}

// operationReceipt reports a revision op already made as replayed, and one
// made for another request under the same operation as a conflict.
func (n *dbNotebook) operationReceipt(ctx context.Context, op operationIdentity) (GroupResult, error) {
	info, ok, err := n.operationCommit(ctx, op.ID)
	if err != nil || !ok {
		return GroupResult{}, err
	}
	if trailerValue(info.Trailers, notestore.RequestTrailer) != op.Request {
		return GroupResult{}, ErrConflict
	}
	return GroupResult{Revision: info.ID.String(), Replayed: true}, nil
}

func (n *dbNotebook) groupReceipt(ctx context.Context, group EditGroup, identityID string) (GroupResult, error) {
	raw, _ := json.Marshal(group)
	return n.operationReceipt(ctx, operationIdentity{ID: ContentHash([]byte(identityID + "\x00" + group.ID)), Request: ContentHash(raw)})
}

// trailerValue is Git's %(trailers:key=<key>,valueonly) trimmed: the values
// of every trailer with that key, in any case, one per line.
func trailerValue(trailers []gitobj.Trailer, key string) string {
	var values []string
	for _, t := range trailers {
		if strings.EqualFold(t.Key, key) {
			values = append(values, t.Value)
		}
	}
	return strings.TrimSpace(strings.Join(values, "\n"))
}

// commitSubject is Git's %s: the message's first paragraph, its lines
// trimmed at the end and joined by spaces.
func commitSubject(message string) string {
	var lines []string
	started := false
	for _, line := range strings.SplitAfter(message, "\n") {
		line = strings.TrimRight(line, " \t\n\r")
		if line == "" {
			if started {
				break
			}
			continue
		}
		started = true
		lines = append(lines, line)
	}
	return strings.Join(lines, " ")
}

// isoTime is Git's %aI as production's Git (Debian bookworm) prints it:
// always a numeric offset, never Z.
func isoTime(s gitobj.Signature) string {
	return s.Time().Format("2006-01-02T15:04:05-07:00")
}

// commitRecord is the Commit the Git backend reads with its log format.
func commitRecord(info notestore.CommitInfo) Commit {
	c := info.Commit
	return Commit{
		AuthorName: c.Author.Name, AuthorEmail: c.Author.Email,
		CommitterName: c.Committer.Name, CommitterEmail: c.Committer.Email,
		AuthoredAt: isoTime(c.Author),
		Source:     trailerValue(info.Trailers, "Gitnotes-Source"),
		UserID:     trailerValue(info.Trailers, "Gitnotes-User"),
		Client:     trailerValue(info.Trailers, "Gitnotes-Client"),
		Revision:   info.ID.String(),
		Message:    commitSubject(c.Message),
	}
}
