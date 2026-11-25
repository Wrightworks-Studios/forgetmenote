package workspace

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"forgetmenote/internal/gitexport"
	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
)

// Purge permanently removes content from every version of a notebook kept in
// the notes store (storage v2 design §11), the one authorized rewrite of
// history: a whole note, chosen versions of one, or text redacted from
// selected notes or the whole notebook. PreparePurge plans it without
// changing anything, and ExecutePurge runs exactly that plan when given its
// confirmation, a reason and who runs it. Who may purge (a notebook Owner or
// an Admin, never an agent alone) is the caller's to check.

// Purge kinds.
const (
	PurgeNote     = "note"
	PurgeVersions = "versions"
	PurgeText     = "text"
)

// purgedText stands where redacted text was, so the document keeps its
// shape.
const purgedText = "[purged]"

// ErrPurgeConfirmation is a purge confirmation that is not the plan's.
var ErrPurgeConfirmation = errors.New("the confirmation does not match this purge plan; prepare the purge again")

// ErrPurgeInvalid is a purge the workspace refuses as asked: an unknown
// kind, text or pattern it cannot use, versions that are not versions, no
// actor or reason, or a plan that removes nothing. A plan that is blocked is
// ErrPurgeBlocked, which matches it too.
var ErrPurgeInvalid = errors.New("invalid purge")

// ErrPurgeBlocked is a purge whose plan says why it cannot run.
var ErrPurgeBlocked = fmt.Errorf("%w: the purge plan is blocked", ErrPurgeInvalid)

// PurgeScope says what a purge removes.
type PurgeScope struct {
	Kind string `json:"kind"`
	// Path names the file a note or versions purge removes, at the head or
	// where it last was.
	Path string `json:"path,omitempty"`
	// Versions are the file's versions a versions purge removes: the
	// revisions that wrote them, as a note's history lists them.
	Versions []string `json:"versions,omitempty"`
	// Text is the exact text a text purge removes, or Pattern a regular
	// expression (RE2) it removes every match of.
	Text    string `json:"text,omitempty"`
	Pattern string `json:"pattern,omitempty"`
	// Paths are the files a text purge covers, at the head or where they
	// last were; none covers the whole notebook. Either way it redacts every
	// commit message of the notebook that holds the text.
	Paths []string `json:"paths,omitempty"`
}

// A PurgePlan is what a purge will do, bound to the head it was made at by
// its confirmation.
type PurgePlan struct {
	Scope PurgeScope `json:"scope"`
	// Revision is the head the plan was made at; From is the first revision
	// the purge rewrites, and RevisionsRewritten how many it rewrites, each
	// of which gets a new ID.
	Revision           string `json:"revision"`
	From               string `json:"from,omitempty"`
	RevisionsRewritten int    `json:"revisionsRewritten"`
	// Notes are the files whose versions the purge changes or removes, and
	// Versions how many versions that is.
	Notes    []PurgeFile `json:"notes"`
	Versions int         `json:"versions"`
	// Messages counts the commit messages the purge redacts: those holding a
	// text purge's text, and those naming a purged note or its discussion by
	// path while it was there.
	Messages int `json:"messages"`
	// Elsewhere lists where a text purge's text also appears outside its
	// scope, or the notes that may still name a purged note in a link;
	// Unredactable the files in scope that hold it but are not text,
	// which only purging them whole removes; Shared the other files holding
	// content a note or versions purge removes, which keep it.
	Elsewhere    []PurgeOccurrence `json:"elsewhere"`
	Unredactable []PurgeOccurrence `json:"unredactable"`
	Shared       []PurgeOccurrence `json:"shared"`
	// Live lists the live documents the purge resets, as the plan found
	// them. Before a purge that rewrites revisions runs, someone may open a
	// note in its scope, whose live document it resets too, so its
	// confirmation binds only those holding edits not saved as a revision,
	// which the reset discards (Bound); the result counts every one reset.
	// A purge that rewrites no revision resets only the live documents it
	// lists, so its confirmation binds them all, with whether they hold
	// such edits.
	Live []PurgeLive `json:"live"`
	// ReferencesToRemap counts the references the notebook's current files
	// make to revisions the purge rewrites, which resolve through the purge's
	// mapping afterwards.
	ReferencesToRemap int `json:"referencesToRemap"`
	// ExportForcePush reports that the Git export may hold revisions the
	// purge rewrites by the time it runs, so the backup worker may replace
	// the remote branch once. The export catches up with the notebook in the
	// background, so a plan with an export says so whenever the purge
	// rewrites a revision, whether or not the export has caught up yet; the
	// result says whether it had (PurgeResult.ExportForcePush).
	ExportForcePush bool `json:"exportForcePush"`
	// Blocked says why the purge cannot run as planned.
	Blocked      string `json:"blocked,omitempty"`
	Confirmation string `json:"confirmation"`
}

// A PurgeFile is one file a purge changes: its path at the head, or where it
// last was, and the versions it changes, newest first.
type PurgeFile struct {
	Path     string   `json:"path"`
	Current  bool     `json:"current"`
	Versions []string `json:"versions"`
}

// A PurgeOccurrence is a file, file name or live document holding what a
// purge removes, and in how many versions; or another notebook of the
// instance, by ID as Path, that stores the same file versions, which stay
// stored for it; or, with no Path, the commits whose author or committer
// holds a text purge's text, which a purge keeps, and how many; or another
// note whose versions may link to a purged note by name.
type PurgeOccurrence struct {
	Path     string `json:"path"`
	Kind     string `json:"kind"` // file, name, live, notebook, author or link
	Versions int    `json:"versions,omitempty"`
}

// A PurgeLive is a live document a purge resets. Pending means it holds
// edits not saved as a revision, which the reset discards.
type PurgeLive struct {
	Path    string `json:"path"`
	Pending bool   `json:"pending"`
}

// A PurgeResult is what a purge did.
type PurgeResult struct {
	Purge     string `json:"purge"`
	Revision  string `json:"revision"`
	Rewritten int    `json:"rewritten"`
	LiveReset int    `json:"liveReset"`
	// Export reports that the Git export was rebuilt and flagged for the
	// backup worker. ExportForcePush reports that its flag lets the worker
	// replace the remote branch once, as the export held revisions this
	// purge, or an earlier one the worker has not pushed, rewrote; until the
	// export is rebuilt, it is the plan's.
	Export          bool `json:"export"`
	ExportForcePush bool `json:"exportForcePush"`
	RemovedVersions int  `json:"removedVersions"`
	RemovedBlocks   int  `json:"removedBlocks"`
}

// purgeSummary is what the purges table keeps of a purge's scope: never the
// text, paths or anything else of the purged content.
type purgeSummary struct {
	Kind     string   `json:"kind"`
	Pattern  bool     `json:"pattern,omitempty"`
	Notebook bool     `json:"notebook,omitempty"`
	Entries  []string `json:"entries,omitempty"`
	Versions []string `json:"versions,omitempty"`
	Messages int      `json:"messages,omitempty"`
	Live     int      `json:"live,omitempty"`
}

// purgeWork is a plan with what running it needs.
type purgeWork struct {
	PurgePlan
	head    notestore.Head
	fromSeq int64
	fn      notestore.RewriteFunc
	message func(seq int64, message string) (string, error)
	reset   []string // live documents to reset besides those of changed entries
	paths   []string // paths of every entry the purge touches
	summary purgeSummary
}

// The most versions a plan lists for one file; it counts them all.
const maxPlanVersions = 200

// PreparePurge plans the purge scope describes on the notebook's current
// head and returns the plan with its confirmation. It changes nothing.
// export, when the instance has one, is the Git export the purge rebuilds.
func (s *Store) PreparePurge(ctx context.Context, scope PurgeScope, export *gitexport.Exporter) (PurgePlan, error) {
	work, err := s.planPurge(ctx, scope, export)
	return work.PurgePlan, err
}

// ExecutePurge runs plan, which PreparePurge returned with confirmation, as
// actor for reason. It plans again and refuses unless that plan is the same
// as far as a confirmation binds it (PurgePlan.Bound): a notebook that
// changed since is ErrConflict, and any other difference
// ErrPurgeConfirmation. It rewrites the history, resets the live documents
// the plan it made again lists, drops this store's caches of the old
// content and, with export, rebuilds the notebook's Git export and flags it
// for the backup worker. The rewrite commits only while the notebook's
// authority record is the one checked first, so a handoff another gateway
// prepared or completed meanwhile refuses it as it refuses a save.
func (s *Store) ExecutePurge(ctx context.Context, plan PurgePlan, confirmation, actor, reason string, export *gitexport.Exporter) (PurgeResult, error) {
	return s.ExecutePurgeFenced(ctx, plan, confirmation, actor, reason, export, nil)
}

// ExecutePurgeFenced is ExecutePurge with fence, when set, run once every
// check has passed, right before the history is rewritten: what fence ends,
// such as the bundle exports that hold the notebook as it is, a refused
// purge leaves alone. An error from fence refuses the purge.
func (s *Store) ExecutePurgeFenced(ctx context.Context, plan PurgePlan, confirmation, actor, reason string, export *gitexport.Exporter, fence func(context.Context) error) (PurgeResult, error) {
	if strings.TrimSpace(actor) == "" || strings.TrimSpace(reason) == "" {
		return PurgeResult{}, fmt.Errorf("%w: a purge needs who runs it and a reason", ErrPurgeInvalid)
	}
	if confirmation == "" || confirmation != plan.Confirmation {
		return PurgeResult{}, ErrPurgeConfirmation
	}
	s.mu.Lock()
	err := s.checkAuthority(ctx)
	// The rewrite's transaction checks again that the record checkAuthority
	// read is still the notebook's, as applyChanges does.
	authority := s.backend.(*dbNotebook).authorityVersion
	s.mu.Unlock()
	if err != nil {
		return PurgeResult{}, err
	}
	work, err := s.planPurge(ctx, plan.Scope, export)
	if err != nil {
		return PurgeResult{}, err
	}
	if work.Revision != plan.Revision {
		return PurgeResult{}, fmt.Errorf("%w: the notebook changed since the purge was planned", ErrConflict)
	}
	if work.Confirmation != confirmation {
		return PurgeResult{}, ErrPurgeConfirmation
	}
	if work.Blocked != "" {
		return PurgeResult{}, fmt.Errorf("%w: %s", ErrPurgeBlocked, work.Blocked)
	}
	if work.fromSeq == 0 && len(work.reset) == 0 {
		return PurgeResult{}, fmt.Errorf("%w: the purge plan removes nothing", ErrPurgeInvalid)
	}
	if fence != nil {
		if err := fence(ctx); err != nil {
			return PurgeResult{}, err
		}
	}
	raw := make([]byte, 16)
	rand.Read(raw)
	result := PurgeResult{Purge: "purge-" + hex.EncodeToString(raw), Revision: work.Revision, ExportForcePush: work.ExportForcePush}
	var reset []string
	var removalErr error
	if work.fromSeq > 0 {
		rewrite, err := s.notes.Rewrite(ctx, work.fromSeq, result.Purge, work.fn, notestore.RewriteOptions{
			ExpectHead: &work.head.Commit, Message: work.message, Actor: actor, Reason: reason, Scope: work.summary, ResetLive: work.reset, ExpectAuthority: &authority})
		if err != nil && rewrite.Head == (notestore.Head{}) {
			return PurgeResult{}, s.purgeRefusal(ctx, err)
		}
		removalErr = err
		result.Revision, result.Rewritten = rewrite.Head.Commit.String(), len(rewrite.Rewritten)
		result.RemovedVersions, result.RemovedBlocks = rewrite.Removed.Versions, rewrite.Removed.Blocks
		reset = rewrite.LiveReset
	} else {
		// Only live documents hold the content: no revision changes.
		purged, err := s.notes.PurgeLive(ctx, result.Purge, notestore.RewriteOptions{
			ExpectHead: &work.head.Commit, Actor: actor, Reason: reason, Scope: work.summary, ResetLive: work.reset, ExpectAuthority: &authority})
		if err != nil {
			return PurgeResult{}, s.purgeRefusal(ctx, err)
		}
		reset = purged.LiveReset
	}
	result.LiveReset = len(reset)
	s.mu.Lock()
	s.forgetPurged(work.paths)
	if head, err := s.notes.Head(ctx); err == nil {
		s.backend.(*dbNotebook).observe(head, true)
	}
	s.mu.Unlock()
	if export != nil && work.fromSeq > 0 {
		// The rewritten notebook no longer extends its export, which stays
		// stopped until rebuilt, so a caller's deadline does not cut the
		// rebuild short.
		if err := export.Rebuild(context.WithoutCancel(ctx), s.notebook, result.Purge); err != nil {
			return result, errors.Join(removalErr, fmt.Errorf("the notebook was purged, but rebuilding its Git export failed; run the export's Rebuild for purge %s again: %w", result.Purge, err))
		}
		result.Export = true
		// The flag the rebuild wrote names the commits it replaces that the
		// remote may hold. One the worker already spent may have replaced
		// them, as may one that cannot be read.
		if flag, ok, err := gitexport.ReadPurge(export.Path(s.notebook)); err == nil && ok && flag.Purge == result.Purge {
			result.ExportForcePush = len(flag.Replaces) > 0
		}
	}
	return result, removalErr
}

// purgeRefusal is the workspace's error for a purge the store refused: a
// handoff since the purge checked the notebook's authority as a write
// refused for it is (authorityRefusal), and another change as a conflict.
func (s *Store) purgeRefusal(ctx context.Context, err error) error {
	switch {
	case errors.Is(err, notestore.ErrAuthorityChanged):
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.backend.(*dbNotebook).authorityRefusal(ctx, err)
	case errors.Is(err, notestore.ErrConflict):
		return fmt.Errorf("%w: %v", ErrConflict, err)
	}
	return err
}

// forgetPurged drops this store's caches that may hold purged content, and
// the live records of paths, whose stored documents the purge reset. Callers
// hold mu.
func (s *Store) forgetPurged(paths []string) {
	s.committedReads.clear()
	s.identityHead, s.identityIDs, s.identityObjects = "", nil, nil
	s.metadataHead, s.metadataObjects, s.metadataCache, s.metadataPaths = "", nil, nil, nil
	s.trashRevision, s.trashCache, s.locksRevision = "", TrashIndex{}, ""
	db := s.backend.(*dbNotebook)
	db.locksBlob, db.trashBlob = gitobj.ID{}, gitobj.ID{}
	for _, p := range paths {
		delete(s.live, p)
		s.notifyLive(p)
	}
}

func (c *committedReads) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries, c.bytes, c.rewrites = nil, 0, nil
}

// ResolvePurgedRevision returns the revision that stands for revision now:
// revision itself while the notebook has it, or what purges rewrote it into.
// purged reports that a purge removed path's version at revision, as a pin
// to a purged note or version reads. A revision the notebook never had is an
// fs.ErrNotExist error, as is a path a rewritten revision lacks when no note
// purge rewrote it. After a note purge such a path reads as purged, as the
// record keeps no path that would tell it from one revision never had.
func (s *Store) ResolvePurgedRevision(ctx context.Context, path, revision string) (string, bool, error) {
	id, err := parseRevision(revision)
	if err != nil || id.IsZero() {
		return "", false, fmt.Errorf("revision %q: %w", revision, fs.ErrNotExist)
	}
	if _, err := s.notes.Commit(ctx, id); err == nil {
		return revision, false, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", false, err
	}
	rec, ok, err := s.notes.Rewritten(ctx, id)
	if err != nil {
		return "", false, err
	}
	if !ok {
		return "", false, fmt.Errorf("revision %s: %w", revision, fs.ErrNotExist)
	}
	if rec.Commit.IsZero() {
		return "", true, nil
	}
	if path == "" {
		return rec.Commit.String(), false, nil
	}
	f, err := s.notes.Stat(ctx, rec.Commit, path)
	if errors.Is(err, fs.ErrNotExist) {
		// Only a note purge removes files, so a file the rewritten revision
		// lacks is one a note purge took, or one revision never had; the
		// purge record keeps no path to tell which.
		for _, purge := range rec.Purges {
			record, err := s.notes.Purge(ctx, purge)
			if err != nil {
				return "", false, err
			}
			var summary purgeSummary
			if json.Unmarshal(record.Scope, &summary) == nil && summary.Kind == PurgeNote {
				return rec.Commit.String(), true, nil
			}
		}
		return "", false, fmt.Errorf("%s at revision %s: %w", path, revision, fs.ErrNotExist)
	}
	if err != nil {
		return "", false, err
	}
	// Walk the version back through each purge, newest first, and ask the
	// versions purges among them whether they removed it.
	version := f.Version
	for i := len(rec.Purges) - 1; i >= 0; i-- {
		purge := rec.Purges[i]
		if old, ok, err := s.notes.Original(ctx, version, purge); err != nil {
			return "", false, err
		} else if ok {
			version = old
		}
		record, err := s.notes.Purge(ctx, purge)
		if err != nil {
			return "", false, err
		}
		var summary purgeSummary
		if json.Unmarshal(record.Scope, &summary) == nil && summary.Kind == PurgeVersions &&
			slices.Contains(summary.Entries, f.EntryID) && slices.Contains(summary.Versions, version.String()) {
			return rec.Commit.String(), true, nil
		}
	}
	return rec.Commit.String(), false, nil
}

// purgeUse is a file version a revision wrote: an add, modify or rename.
type purgeUse struct {
	seq    int64
	commit gitobj.ID
	entry  string
	path   string
	blob   gitobj.ID
	mode   uint32
}

// purgeHistory is the notebook's whole history as a plan reads it.
type purgeHistory struct {
	head     notestore.Head
	files    map[string]notestore.File // at the head
	uses     []purgeUse                // oldest first
	paths    map[string][]string       // every path each entry had
	last     map[string]string         // each entry's newest path
	atPath   map[string]string         // each path's newest entry
	seqs     map[gitobj.ID]int64
	messages map[int64]string
	people   map[int64][]string          // each revision's author's and committer's names and emails
	order    []int64                     // every revision's seq, oldest first
	parent   map[int64]int64             // each revision's first parent, 0 for none
	moves    map[string]map[int64]string // each entry's path after each revision that changed it, "" once deleted
}

func (s *Store) purgeHistory(ctx context.Context) (*purgeHistory, error) {
	head, err := s.notes.Head(ctx)
	if err != nil {
		return nil, err
	}
	files, err := s.notes.Files(ctx, head.Commit)
	if err != nil {
		return nil, err
	}
	h := &purgeHistory{head: head, files: map[string]notestore.File{}, paths: map[string][]string{}, last: map[string]string{},
		atPath: map[string]string{}, seqs: map[gitobj.ID]int64{}, messages: map[int64]string{}, people: map[int64][]string{}, parent: map[int64]int64{}, moves: map[string]map[int64]string{}}
	for _, f := range files {
		h.files[f.Path] = f
	}
	seen := map[[2]string]bool{}
	note := func(entry, path string) {
		if key := [2]string{entry, path}; !seen[key] {
			seen[key] = true
			h.paths[entry] = append(h.paths[entry], path)
		}
	}
	for after := int64(0); after < head.Seq; {
		infos, err := s.notes.Revisions(ctx, after, 1000)
		if err != nil {
			return nil, err
		}
		if len(infos) == 0 {
			break
		}
		for _, info := range infos {
			after = info.Seq
			if info.Seq > head.Seq {
				break
			}
			h.seqs[info.ID], h.messages[info.Seq] = info.Seq, info.Commit.Message
			h.people[info.Seq] = []string{info.Commit.Author.Name, info.Commit.Author.Email, info.Commit.Committer.Name, info.Commit.Committer.Email}
			h.order = append(h.order, info.Seq)
			if len(info.Commit.Parents) > 0 {
				h.parent[info.Seq] = h.seqs[info.Commit.Parents[0]]
			}
			for _, c := range info.Changes {
				note(c.EntryID, c.Path)
				if c.OldPath != "" {
					note(c.EntryID, c.OldPath)
				}
				h.last[c.EntryID], h.atPath[c.Path] = c.Path, c.EntryID
				if h.moves[c.EntryID] == nil {
					h.moves[c.EntryID] = map[int64]string{}
				}
				if c.Op == notestore.OpDelete {
					h.moves[c.EntryID][info.Seq] = ""
				} else {
					h.moves[c.EntryID][info.Seq] = c.Path
					h.uses = append(h.uses, purgeUse{seq: info.Seq, commit: info.ID, entry: c.EntryID, path: c.Path, blob: c.Blob, mode: c.Mode})
				}
			}
		}
	}
	return h, nil
}

// entryAt is the entry at path at the head, or the last one there.
func (h *purgeHistory) entryAt(path string) (string, error) {
	if f, ok := h.files[path]; ok {
		return f.EntryID, nil
	}
	if entry, ok := h.atPath[path]; ok {
		return entry, nil
	}
	return "", fmt.Errorf("%s: %w", path, fs.ErrNotExist)
}

// pathsOver returns where entry is at every revision, "" where it is not,
// following each revision's first parent as its changes do.
func (h *purgeHistory) pathsOver(entry string) map[int64]string {
	moves := h.moves[entry]
	at := make(map[int64]string, len(h.order))
	for _, seq := range h.order {
		if p, ok := moves[seq]; ok {
			at[seq] = p
		} else if p := at[h.parent[seq]]; p != "" {
			at[seq] = p
		}
	}
	return at
}

// pathOf is where entry is at the head, or where it last was.
func (h *purgeHistory) pathOf(entry string) (string, bool) {
	p := h.last[entry]
	f, ok := h.files[p]
	return p, ok && f.EntryID == entry
}

// systemFile is a file the notebook keeps for itself, which only a text
// purge changes.
func systemFile(path string) bool {
	return path == "AGENTS.md" || path == locksPath || trashFile(path) || strings.HasPrefix(path, "comments/")
}

// A purgeMatcher finds a text purge's text.
type purgeMatcher struct {
	text []byte
	re   *regexp.Regexp
}

func newPurgeMatcher(scope PurgeScope) (purgeMatcher, error) {
	switch {
	case scope.Text != "" && scope.Pattern != "":
		return purgeMatcher{}, fmt.Errorf("%w: a text purge takes text or a pattern, not both", ErrPurgeInvalid)
	case scope.Text != "":
		// Text the placeholder holds would still be there after the purge.
		if strings.Contains(purgedText, scope.Text) {
			return purgeMatcher{}, fmt.Errorf("%w: the purge text must not be part of %s", ErrPurgeInvalid, purgedText)
		}
		return purgeMatcher{text: []byte(scope.Text)}, nil
	case scope.Pattern != "":
		re, err := regexp.Compile(scope.Pattern)
		if err != nil {
			return purgeMatcher{}, fmt.Errorf("%w: the pattern: %v", ErrPurgeInvalid, err)
		}
		if re.MatchString("") || re.MatchString(purgedText) {
			return purgeMatcher{}, fmt.Errorf("%w: a purge pattern must match neither empty text nor %s", ErrPurgeInvalid, purgedText)
		}
		return purgeMatcher{re: re}, nil
	}
	return purgeMatcher{}, fmt.Errorf("%w: a text purge needs the text or a pattern", ErrPurgeInvalid)
}

func (m purgeMatcher) count(b []byte) int {
	if m.re != nil {
		return len(m.re.FindAllIndex(b, -1))
	}
	return bytes.Count(b, m.text)
}

func (m purgeMatcher) replace(b []byte) []byte {
	if m.re != nil {
		return m.re.ReplaceAllLiteral(b, []byte(purgedText))
	}
	return bytes.ReplaceAll(b, m.text, []byte(purgedText))
}

// holds reports whether a file's content holds what m finds: in its bytes,
// or, in a JSON document, in a string JSON stores escaped, such as a comment
// with a quote, a backslash, an ampersand or an angle bracket in it.
func (m purgeMatcher) holds(content []byte) bool {
	if m.count(content) > 0 {
		return true
	}
	held := false
	if jsonDocument(content) {
		jsonStrings(content, func(start, end int) {
			if value, ok := escapedJSONString(content[start:end]); ok && !held {
				held = m.count([]byte(value)) > 0
			}
		})
	}
	return held
}

// redact replaces what m finds in a file's content with the placeholder. In
// a JSON document it first replaces it in each string JSON stores escaped,
// which it writes again in the escaping that string had.
func (m purgeMatcher) redact(content []byte) []byte {
	if jsonDocument(content) {
		var out []byte
		last := 0
		jsonStrings(content, func(start, end int) {
			literal := content[start:end]
			value, ok := escapedJSONString(literal)
			if !ok || m.count([]byte(value)) == 0 {
				return
			}
			lower := bytes.ToLower(literal)
			html := bytes.Contains(lower, []byte(`\u003c`)) || bytes.Contains(lower, []byte(`\u003e`)) || bytes.Contains(lower, []byte(`\u0026`))
			out = append(append(out, content[last:start]...), jsonString(string(m.replace([]byte(value))), html)...)
			last = end
		})
		if out != nil {
			content = append(out, content[last:]...)
		}
	}
	return m.replace(content)
}

// jsonDocument reports whether content is a JSON object or array.
func jsonDocument(content []byte) bool {
	trimmed := bytes.TrimLeft(content, " \t\r\n")
	return len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[') && json.Valid(content)
}

// jsonStrings calls fn with where each string of a valid JSON document
// starts and ends, its quotes included.
func jsonStrings(doc []byte, fn func(start, end int)) {
	for i := 0; i < len(doc); i++ {
		if doc[i] != '"' {
			continue
		}
		j := i + 1
		for ; j < len(doc) && doc[j] != '"'; j++ {
			if doc[j] == '\\' {
				j++
			}
		}
		fn(i, j+1)
		i = j
	}
}

// escapedJSONString decodes a JSON string literal that holds an escape. One
// without is its own bytes, which a match on the whole content finds.
func escapedJSONString(literal []byte) (string, bool) {
	if bytes.IndexByte(literal, '\\') < 0 {
		return "", false
	}
	var value string
	return value, json.Unmarshal(literal, &value) == nil
}

// jsonString encodes value as a JSON string, escaping HTML characters as
// json.Marshal does when html is set.
func jsonString(value string, html bool) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(html)
	enc.Encode(value)
	return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
}

// A purgeFileKind is how a text purge redacts a file, by its path: the
// notebook's own files as their readers parse them, which a purge must
// leave readable, and any other file as its bytes.
type purgeFileKind int

const (
	purgeBytes        purgeFileKind = iota
	purgeNoteFile                   // a note, whose fmn-id is its identity
	purgeTrashIndex                 // the Trash index
	purgeTrashArchive               // a Trash archive
	purgeLocks                      // the lock policy
	purgeComments                   // a note's discussion
)

func purgeKindOf(path string) purgeFileKind {
	switch {
	case path == trashIndexPath:
		return purgeTrashIndex
	case trashFile(path):
		return purgeTrashArchive
	case path == locksPath:
		return purgeLocks
	case strings.HasPrefix(path, "comments/") && strings.HasSuffix(path, ".json"):
		return purgeComments
	case strings.HasSuffix(path, ".md"):
		return purgeNoteFile
	}
	return purgeBytes
}

// freeText reports, for the notebook's own JSON files, whether the string
// value at keys (an array's elements are "*") is text people wrote, which a
// text purge redacts. Every other part of such a file is its structure,
// which its readers parse: keys, IDs, paths, revisions, dates, hashes,
// numbers and flags. Other kinds of file have no such structure: nil.
func (k purgeFileKind) freeText() func(keys []string) bool {
	switch k {
	case purgeTrashIndex, purgeTrashArchive:
		// A Trash record's title, and who moved the note to the Trash.
		return func(keys []string) bool {
			return len(keys) == 3 && keys[0] == "records" && keys[1] == "*" && (keys[2] == "title" || keys[2] == "deletedBy")
		}
	case purgeLocks:
		// Only paths, which a purge cannot rename.
		return func([]string) bool { return false }
	case purgeComments:
		// What a comment, its replies, its anchor and its finding say, and
		// who wrote them.
		return func(keys []string) bool {
			if len(keys) == 0 {
				return false
			}
			switch keys[len(keys)-1] {
			case "id", "created", "revision", "path", "sourcePath", "sourceRevision", "key", "origin", "noteHash", "requestHash", "hash":
				return false
			}
			return true
		}
	}
	return nil
}

// A redactProblem is why a text purge cannot redact a file version.
type redactProblem int

const (
	redactable        redactProblem = iota
	redactInvalidJSON               // the redacted file is no longer JSON
	redactReforms                   // the placeholder and what surrounds it form the text again
	redactStructure                 // the text is in the structure of one of the notebook's own files
	redactIdentity                  // the text is in a note's fmn-id
	redactUnreadable                // the redacted file is one its reader refuses
)

// blocked is the plan's Blocked for a problem redacting the file at path.
func (p redactProblem) blocked(path string) string {
	switch p {
	case redactInvalidJSON:
		return fmt.Sprintf("redacting %s would leave it invalid JSON; narrow the pattern", path)
	case redactReforms:
		return fmt.Sprintf("redacting %s would leave the text in it, which the placeholder and what surrounds it form again", path)
	case redactStructure:
		return fmt.Sprintf("%s holds the text in the notebook's own structure (its field names, IDs, paths, revisions, dates or flags), which a purge cannot change; narrow the text or pattern, or purge only the notes that hold it", path)
	case redactIdentity:
		return fmt.Sprintf("the fmn-id of %s, the note's identity, holds the text, and a purge cannot change a note's identity; narrow the text or pattern", path)
	case redactUnreadable:
		return fmt.Sprintf("redacting %s would leave a file the notebook cannot read; narrow the text or pattern", path)
	}
	return ""
}

// redactFile returns a version of a file of kind with what m finds
// redacted, and what keeps that from running. The notebook's own JSON files
// keep their structure, which still holding the text blocks, and must stay
// readable; a note keeps its fmn-id. Any other file is redacted as bytes.
func (s *Store) redactFile(m purgeMatcher, kind purgeFileKind, content []byte) ([]byte, redactProblem) {
	if free := kind.freeText(); free != nil && jsonDocument(content) {
		redacted, reformed := m.redactJSONValues(content, free)
		switch {
		case reformed:
			return redacted, redactReforms
		case m.holds(redacted):
			return redacted, redactStructure
		case s.checkOwnFile(kind, content) == nil && s.checkOwnFile(kind, redacted) != nil:
			return redacted, redactUnreadable
		}
		return redacted, redactable
	}
	redacted := m.redact(content)
	switch {
	case json.Valid(content) && !json.Valid(redacted):
		return redacted, redactInvalidJSON
	case m.holds(redacted):
		return redacted, redactReforms
	}
	if kind == purgeNoteFile {
		if id, err := NoteID(content); err == nil {
			if again, err := NoteID(redacted); err != nil || again != id {
				return redacted, redactIdentity
			}
		}
	}
	return redacted, redactable
}

// checkOwnFile parses a version of one of the notebook's own JSON files as
// its reader does: loadTrash, trashArchived, loadLocks or Comments.
func (s *Store) checkOwnFile(kind purgeFileKind, content []byte) error {
	var index TrashIndex
	var err error
	switch kind {
	case purgeTrashIndex:
		index, err = parseTrashIndex(content)
	case purgeTrashArchive:
		index, err = parseTrashArchive(content)
	case purgeLocks:
		if len(content) > maxLockPolicyBytes {
			return errors.New("lock policy exceeds 64 KiB")
		}
		_, err = s.parseLockPolicy(content)
		return err
	case purgeComments:
		var discussion []Comment
		return json.Unmarshal(content, &discussion)
	default:
		return nil
	}
	if err != nil {
		return err
	}
	return s.validateTrash(index)
}

// redactJSONValues redacts what m finds in the string values of a JSON
// document that free selects by their keys, never in a key or any other
// value, writing each again in the escaping it had; reformed reports a value
// that still holds the text, which the placeholder and what surrounds it
// form again.
func (m purgeMatcher) redactJSONValues(doc []byte, free func(keys []string) bool) (redacted []byte, reformed bool) {
	var out []byte
	last := 0
	jsonValueStrings(doc, func(keys []string, start, end int) {
		literal := doc[start:end]
		var value string
		if !free(keys) || json.Unmarshal(literal, &value) != nil || m.count([]byte(value)) == 0 {
			return
		}
		lower := bytes.ToLower(literal)
		html := bytes.Contains(lower, []byte(`\u003c`)) || bytes.Contains(lower, []byte(`\u003e`)) || bytes.Contains(lower, []byte(`\u0026`))
		replaced := m.replace([]byte(value))
		reformed = reformed || m.count(replaced) > 0
		out = append(append(out, doc[last:start]...), jsonString(string(replaced), html)...)
		last = end
	})
	return append(out, doc[last:]...), reformed
}

// jsonValueStrings calls fn with each string value of a valid JSON document,
// never an object's key: the keys that lead to it, "*" for an array's
// element, and where its literal starts and ends, its quotes included.
func jsonValueStrings(doc []byte, fn func(keys []string, start, end int)) {
	type level struct {
		object bool
		key    string // in an object, the key of the value being read
		value  bool   // in an object, a key was read and its value is next
	}
	var stack []level
	path := func() []string {
		keys := make([]string, len(stack))
		for i, l := range stack {
			keys[i] = "*"
			if l.object {
				keys[i] = l.key
			}
		}
		return keys
	}
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.UseNumber()
	for {
		// Only spaces, commas and colons come between a token and the
		// opening quote of the string after it.
		before := int(dec.InputOffset())
		token, err := dec.Token()
		if err != nil {
			return
		}
		top := len(stack) - 1
		switch t := token.(type) {
		case json.Delim:
			if t == '{' || t == '[' {
				stack = append(stack, level{object: t == '{'})
				continue
			}
			stack = stack[:top]
		case string:
			if top >= 0 && stack[top].object && !stack[top].value {
				stack[top].key, stack[top].value = t, true
				continue
			}
			fn(path(), before+bytes.IndexByte(doc[before:], '"'), int(dec.InputOffset()))
		}
		// A value ended: its object reads a key next.
		if top := len(stack) - 1; top >= 0 && stack[top].object {
			stack[top].value = false
		}
	}
}

// trailerStart matches the start of a trailer line: its key and separator.
var trailerStart = regexp.MustCompile(`^[A-Za-z0-9-]+[ \t]*:`)

// gitnotesTrailer reports whether key is one of the trailers that record
// who made a revision and how, which the store reads back from every
// commit (Gitnotes-Source, Gitnotes-User, Gitnotes-Operation and the rest).
func gitnotesTrailer(key string) bool {
	return len(key) >= len("Gitnotes-") && strings.EqualFold(key[:len("Gitnotes-")], "Gitnotes-")
}

// collaborationTrailer is the trailer a live edits checkpoint writes for
// each edit it commits, whose value is that edit's receipt.
const collaborationTrailer = "Collaboration-Receipt"

// appTrailer reports whether key is a trailer this service writes: a
// Gitnotes trailer, or the receipt of a live edit. Any other line of a
// message that looks like a trailer, such as "Jane: please keep these",
// is what someone wrote.
func appTrailer(key string) bool {
	return gitnotesTrailer(key) || strings.EqualFold(key, collaborationTrailer)
}

// redactMessage replaces what m finds in a commit message: in its subject,
// its body and the values of the receipts of live edits, never in the key
// of a trailer this service writes or in a Gitnotes trailer. A line that
// only looks like a trailer is redacted as text, its key included.
func (m purgeMatcher) redactMessage(message string) string {
	lines := strings.SplitAfter(message, "\n")
	// Trailers are in the last paragraph, when it is not the first.
	end := len(lines)
	for end > 0 && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	block := end
	for i := end - 1; i > 0; i-- {
		if strings.TrimSpace(lines[i]) == "" {
			block = i + 1
			break
		}
	}
	var out, run strings.Builder
	flush := func() {
		out.Write(m.replace([]byte(run.String())))
		run.Reset()
	}
	kept := false // the line continues a Gitnotes trailer
	for i, line := range lines {
		if i >= block && i < end {
			if loc := trailerStart.FindStringIndex(line); loc != nil {
				if key := strings.TrimRight(line[:loc[1]-1], " \t"); appTrailer(key) {
					kept = gitnotesTrailer(key)
					flush()
					if kept {
						out.WriteString(line)
					} else {
						out.WriteString(line[:loc[1]])
						run.WriteString(line[loc[1]:])
					}
					continue
				}
			}
			if kept && (line[0] == ' ' || line[0] == '\t') {
				flush()
				out.WriteString(line)
				continue
			}
		}
		kept = false
		run.WriteString(line)
	}
	flush()
	return out.String()
}

// sameTrailers reports whether redacted, a redaction of message, keeps the
// trailers this service writes as a rewritten commit stores them: each key,
// in order, and the value of each Gitnotes trailer.
func sameTrailers(message, redacted string) bool {
	parse := func(m string) []gitobj.Trailer {
		all := gitobj.Trailers(gitobj.CleanMessage(m, "", []gitobj.Trailer{{Key: notestore.PurgeTrailer, Value: "purge"}}))
		return slices.DeleteFunc(all, func(t gitobj.Trailer) bool { return !appTrailer(t.Key) })
	}
	return slices.EqualFunc(parse(message), parse(redacted), func(a, b gitobj.Trailer) bool {
		return a.Key == b.Key && (a.Value == b.Value || !gitnotesTrailer(a.Key))
	})
}

// isText reports whether content is text a redaction can edit.
func isText(content []byte) bool {
	return utf8.Valid(content) && bytes.IndexByte(content, 0) < 0
}

var revisionToken = regexp.MustCompile(`\b[0-9a-f]{40}\b`)

// planPurge reads the notebook's whole history and plans scope on it.
func (s *Store) planPurge(ctx context.Context, scope PurgeScope, export *gitexport.Exporter) (purgeWork, error) {
	h, err := s.purgeHistory(ctx)
	if err != nil {
		return purgeWork{}, err
	}
	w := purgeWork{head: h.head}
	w.Scope, w.Revision = scope, h.head.Commit.String()
	var affected []purgeUse
	var target []string // the entries the plan lists as notes
	tokens := map[string]int{}
	readTokens := func(content []byte) {
		for _, t := range revisionToken.FindAll(content, -1) {
			tokens[string(t)]++
		}
	}
	switch scope.Kind {
	case PurgeNote, PurgeVersions:
		if systemFile(scope.Path) {
			return purgeWork{}, fmt.Errorf("%w: %s is kept by the notebook; purge text from it instead", ErrForbidden, scope.Path)
		}
		entry, err := h.entryAt(scope.Path)
		if err != nil {
			return purgeWork{}, err
		}
		target = []string{entry}
		if scope.Kind == PurgeNote {
			affected, err = s.planNote(ctx, &w, h, entry)
		} else {
			affected, err = s.planVersions(ctx, &w, h, entry, scope.Versions)
		}
		if err != nil {
			return purgeWork{}, err
		}
		if err := s.readHeadText(ctx, h, readTokens); err != nil {
			return purgeWork{}, err
		}
	case PurgeText:
		if affected, target, err = s.planText(ctx, &w, h, scope, readTokens); err != nil {
			return purgeWork{}, err
		}
	default:
		return purgeWork{}, fmt.Errorf("%w: unknown purge kind %q", ErrPurgeInvalid, scope.Kind)
	}

	// The notes and versions the plan lists, and where it starts.
	byEntry := map[string][]gitobj.ID{}
	listed := map[string]bool{}
	for _, entry := range target {
		listed[entry] = true
	}
	touched := map[string]bool{}
	seenVersion := map[[2]string]bool{}
	for _, u := range affected {
		if w.fromSeq == 0 || u.seq < w.fromSeq {
			w.fromSeq = u.seq
		}
		touched[u.entry] = true
		if key := [2]string{u.entry, u.commit.String()}; listed[u.entry] && !seenVersion[key] {
			seenVersion[key] = true
			byEntry[u.entry] = append(byEntry[u.entry], u.commit)
			w.Versions++
		}
	}
	for _, entry := range slices.Sorted(maps.Keys(byEntry)) {
		commits := byEntry[entry]
		slices.Reverse(commits)
		path, current := h.pathOf(entry)
		n := PurgeFile{Path: path, Current: current}
		for _, c := range commits[:min(len(commits), maxPlanVersions)] {
			n.Versions = append(n.Versions, c.String())
		}
		w.Notes = append(w.Notes, n)
	}
	// Sorted stably from entry order, so the same plan always lists the
	// same way and gets the same confirmation.
	slices.SortStableFunc(w.Notes, func(a, b PurgeFile) int { return strings.Compare(a.Path, b.Path) })
	if w.fromSeq > 0 {
		w.From = h.commitAt(w.fromSeq)
		w.RevisionsRewritten = int(h.head.Seq - w.fromSeq + 1)
		for token, count := range tokens {
			if id, err := gitobj.ParseID(token); err == nil && h.seqs[id] >= w.fromSeq {
				w.ReferencesToRemap += count
			}
		}
	}
	// The live documents the purge resets: those of every entry it changes,
	// and those planText found holding the text.
	live, err := s.notes.LiveEntries(ctx)
	if err != nil {
		return purgeWork{}, err
	}
	resetting := map[string]bool{}
	for _, entry := range w.reset {
		resetting[entry] = true
	}
	for _, entry := range live {
		if !touched[entry] && !resetting[entry] {
			continue
		}
		st, err := s.notes.LiveState(ctx, entry)
		if err != nil {
			return purgeWork{}, err
		}
		// Edits not saved as a revision are the updates since the snapshot
		// and, in a snapshot with no base revision, the snapshot's own.
		path, _ := h.pathOf(entry)
		w.Live = append(w.Live, PurgeLive{Path: path, Pending: len(st.Updates) > 0 || st.Snapshot != nil && st.Base.IsZero()})
	}
	slices.SortStableFunc(w.Live, func(a, b PurgeLive) int { return strings.Compare(a.Path, b.Path) })
	w.summary.Live = len(w.Live)
	for entry := range touched {
		w.paths = append(w.paths, h.paths[entry]...)
	}
	for _, entry := range w.reset {
		w.paths = append(w.paths, h.paths[entry]...)
	}
	// The export catches up with the notebook in the background, so by the
	// time the purge runs it may hold any revision the purge rewrites,
	// whatever its branch names now: a plan that read the branch would change,
	// and refuse its own confirmation, as the export caught up.
	w.ExportForcePush = export != nil && w.fromSeq > 0
	for _, list := range [][]PurgeOccurrence{w.Elsewhere, w.Unredactable, w.Shared} {
		slices.SortStableFunc(list, func(a, b PurgeOccurrence) int {
			if c := strings.Compare(a.Path, b.Path); c != 0 {
				return c
			}
			return strings.Compare(a.Kind, b.Kind)
		})
	}
	w.Confirmation = w.confirmation(s.notebook)
	return w, nil
}

func (h *purgeHistory) commitAt(seq int64) string {
	for id, s := range h.seqs {
		if s == seq {
			return id.String()
		}
	}
	return ""
}

// confirmation binds a plan to its notebook, head, scope and everything it
// reports, as Bound returns it.
func (w *purgeWork) confirmation(notebook string) string {
	raw, _ := json.Marshal(struct {
		Notebook string    `json:"notebook"`
		Plan     PurgePlan `json:"plan"`
	}{notebook, w.Bound()})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// Bound returns what a confirmation of p binds: all it reports but the
// confirmation itself and, for a purge that rewrites revisions, the live
// documents it resets that lose nothing. Such a purge (From) resets the
// live documents of the notes it changes or finds holding its text,
// including one someone opens before it runs, which loses nothing, as its
// result counts; so only the live documents holding edits not saved as a
// revision, which the reset discards, are bound, and a note in its scope
// someone types into meanwhile means the plan must be reviewed again. A
// purge that rewrites no revision changes the notebook only through the
// live documents it lists, or not at all, so they are all bound, with
// whether they hold edits not saved: a note it lists that someone types
// into meanwhile, whose edits the reset would discard, means the plan must
// be reviewed again too. What a purge leaves where it does not reach, such
// as a live document outside its scope that holds the text (Elsewhere),
// stays bound.
func (p PurgePlan) Bound() PurgePlan {
	p.Confirmation = ""
	if p.From == "" {
		return p
	}
	var pending []PurgeLive
	for _, l := range p.Live {
		if l.Pending {
			pending = append(pending, l)
		}
	}
	p.Live = pending
	return p
}

// occurrences groups uses by entry into the occurrences a plan reports.
func occurrences(h *purgeHistory, kind string, counts map[string]int) []PurgeOccurrence {
	var out []PurgeOccurrence
	for _, entry := range slices.Sorted(maps.Keys(counts)) {
		path, _ := h.pathOf(entry)
		out = append(out, PurgeOccurrence{Path: path, Kind: kind, Versions: counts[entry]})
	}
	return out
}

// sharedElsewhere adds to list the other notebooks that store any of blobs.
func (s *Store) sharedElsewhere(ctx context.Context, list []PurgeOccurrence, blobs map[gitobj.ID]bool) ([]PurgeOccurrence, error) {
	if len(blobs) == 0 {
		return list, nil
	}
	counts, err := s.notes.SharedElsewhere(ctx, slices.Collect(maps.Keys(blobs)))
	if err != nil {
		return nil, err
	}
	for _, notebook := range slices.Sorted(maps.Keys(counts)) {
		list = append(list, PurgeOccurrence{Path: notebook, Kind: "notebook", Versions: counts[notebook]})
	}
	return list, nil
}

// readHeadText calls fn with the content of every text file at the head.
func (s *Store) readHeadText(ctx context.Context, h *purgeHistory, fn func([]byte)) error {
	var blobs []gitobj.ID
	for _, f := range h.files {
		if strings.HasSuffix(f.Path, ".md") || strings.HasSuffix(f.Path, ".json") {
			blobs = append(blobs, f.Blob)
		}
	}
	return s.notes.ReadBlobs(ctx, blobs, func(_ int, content []byte) error {
		fn(content)
		return nil
	})
}

// planNote plans removing entry from every version, with its discussions,
// its Trash records, its locks and its path in commit messages.
func (s *Store) planNote(ctx context.Context, w *purgeWork, h *purgeHistory, entry string) ([]purgeUse, error) {
	drop := map[string]bool{entry: true}
	notePaths := map[string]bool{}
	for _, p := range h.paths[entry] {
		notePaths[p] = true
	}
	// The note's discussion is the file at its comment path while it is at
	// that path: a file there before or after is another note's, and one
	// that was both holds the two notes' comments, which only a text purge
	// can tell apart.
	at := h.pathsOver(entry)
	discussions := map[string]string{} // each of the note's paths' comment path
	for p := range notePaths {
		if strings.HasSuffix(p, ".md") {
			discussions[p] = commentPath(p)
		}
	}
	discussionAt := func(u purgeUse) bool {
		c, ok := discussions[at[u.seq]]
		return ok && u.path == c
	}
	for _, u := range h.uses {
		if u.entry != entry && discussionAt(u) {
			drop[u.entry] = true
		}
	}
	for _, u := range h.uses {
		if u.entry != entry && drop[u.entry] && !discussionAt(u) {
			w.Blocked = fmt.Sprintf("the discussion file %s also held another note's comments; purge the text of this note's comments from it instead", u.path)
			break
		}
	}
	blobs := map[gitobj.ID]bool{}
	var indexes []gitobj.ID
	for _, u := range h.uses {
		switch {
		case drop[u.entry]:
			blobs[u.blob] = true
		case trashFile(u.path) && !slices.Contains(indexes, u.blob):
			indexes = append(indexes, u.blob)
		}
	}
	// The Trash index and archive versions that record the note.
	recorded := map[gitobj.ID]bool{}
	err := s.notes.ReadBlobs(ctx, indexes, func(i int, content []byte) error {
		var index TrashIndex
		if json.Unmarshal(content, &index) == nil && slices.ContainsFunc(index.Records, func(r TrashRecord) bool { return notePaths[r.Path] }) {
			recorded[indexes[i]] = true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// The locks file versions that lock the note: each lists a path of the
	// note that every revision writing it had the note at, so the lock is the
	// note's. A version the head keeps after the note left that path may lock
	// another note there, and keeps the path.
	lockUses := map[gitobj.ID][]int64{}
	for _, u := range h.uses {
		if u.path == locksPath {
			lockUses[u.blob] = append(lockUses[u.blob], u.seq)
		}
	}
	headLocks := h.files[locksPath].Blob
	locks := map[gitobj.ID][]byte{}
	lockNamed := 0
	lockBlobs := slices.Collect(maps.Keys(lockUses))
	err = s.notes.ReadBlobs(ctx, lockBlobs, func(i int, content []byte) error {
		var policy lockPolicy
		if json.Unmarshal(content, &policy) != nil {
			return nil
		}
		blob := lockBlobs[i]
		kept := slices.DeleteFunc(slices.Clone(policy.Paths), func(p string) bool {
			if !notePaths[p] {
				return false
			}
			mine := !slices.ContainsFunc(lockUses[blob], func(seq int64) bool { return at[seq] != p }) && (blob != headLocks || at[h.head.Seq] == p)
			if !mine {
				lockNamed++
			}
			return mine
		})
		if len(kept) < len(policy.Paths) {
			raw, err := json.MarshalIndent(lockPolicy{policy.Version, kept}, "", "  ")
			if err != nil {
				return err
			}
			locks[blob] = append(raw, '\n')
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	var affected []purgeUse
	shared := map[string]int{}
	for _, u := range h.uses {
		switch {
		case drop[u.entry] || trashFile(u.path) && recorded[u.blob] || u.path == locksPath && locks[u.blob] != nil:
			affected = append(affected, u)
		case blobs[u.blob]:
			shared[u.entry]++
		}
	}
	w.Shared = occurrences(h, "file", shared)
	if w.Shared, err = s.sharedElsewhere(ctx, w.Shared, blobs); err != nil {
		return nil, err
	}
	// The other notes that may name the note in a link keep its name; the
	// plan lists them, for a text purge of the name.
	links, err := s.linkingVersions(ctx, h, h.paths[entry], drop)
	if err != nil {
		return nil, err
	}
	w.Elsewhere = occurrences(h, "link", links)
	if lockNamed > 0 {
		w.Shared = append(w.Shared, PurgeOccurrence{Path: locksPath, Kind: "name", Versions: lockNamed})
	}
	// The commit messages that name the note, or its discussion, by a path
	// it had then or had just before.
	names := func(seq int64) []string {
		var out []string
		for _, p := range []string{at[seq], at[h.parent[seq]]} {
			if p != "" && !slices.Contains(out, p) {
				out = append(out, p)
				if c, ok := discussions[p]; ok {
					out = append(out, c)
				}
			}
		}
		return out
	}
	w.message = func(seq int64, message string) (string, error) { return redactPaths(message, names(seq)), nil }
	for seq, message := range h.messages {
		if redactPaths(message, names(seq)) != message {
			w.Messages++
		}
	}
	w.summary = purgeSummary{Kind: PurgeNote, Entries: slices.Sorted(maps.Keys(drop)), Messages: w.Messages}
	w.fn = func(f notestore.RewriteFile, content func() ([]byte, error)) ([]byte, bool, error) {
		if drop[f.EntryID] {
			return nil, false, nil
		}
		if f.Path == locksPath && locks[f.Blob] != nil {
			return locks[f.Blob], true, nil
		}
		if !trashFile(f.Path) || !recorded[f.Blob] {
			return nil, true, nil
		}
		raw, err := content()
		if err != nil {
			return nil, false, err
		}
		var index TrashIndex
		if err := json.Unmarshal(raw, &index); err != nil {
			return nil, false, err
		}
		// Removing records only shrinks the index or archive, which keeps
		// its path and its other records.
		index.Records = slices.DeleteFunc(index.Records, func(r TrashRecord) bool { return notePaths[r.Path] })
		rewritten, err := encodeTrashIndex(index)
		if err != nil {
			return nil, false, err
		}
		return rewritten, true, nil
	}
	return affected, nil
}

// linkingVersions counts, by entry, the versions of the notes outside drop
// that may name a note that had paths in a link: a Markdown link or
// reference to one of its file names, as written or URL-escaped, which a
// relative link ends with too, or a wiki link to one of its names. A file
// name inside a longer one, as a.md is in data.md, is not one.
func (s *Store) linkingVersions(ctx context.Context, h *purgeHistory, paths []string, drop map[string]bool) (map[string]int, error) {
	var names, stems []string
	for _, p := range paths {
		base := path.Base(p)
		for _, name := range []string{base, url.PathEscape(base), (&url.URL{Path: base}).EscapedPath()} {
			if name = regexp.QuoteMeta(name); !slices.Contains(names, name) {
				names = append(names, name)
			}
		}
		if stem, ok := strings.CutSuffix(base, ".md"); ok && stem != "" {
			stems = append(stems, regexp.QuoteMeta(stem))
		}
	}
	if len(names) == 0 {
		return nil, nil
	}
	const nameChar = `\pL\pN._%-`
	pattern := `(?:^|[^` + nameChar + `])(?:` + strings.Join(names, "|") + `)(?:$|[^` + nameChar + `])`
	if len(stems) > 0 {
		pattern += `|\[\[(?:[^\]|#\n]*/)?(?:` + strings.Join(stems, "|") + `)(?:\.md)?(?:\]\]|[|#])`
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, err
	}
	notes := func(u purgeUse) bool {
		return !drop[u.entry] && u.mode != gitobj.ModeGitlink && strings.HasSuffix(u.path, ".md")
	}
	var blobs []gitobj.ID
	seen := map[gitobj.ID]bool{}
	for _, u := range h.uses {
		if notes(u) && !seen[u.blob] {
			seen[u.blob] = true
			blobs = append(blobs, u.blob)
		}
	}
	naming := map[gitobj.ID]bool{}
	for start := 0; start < len(blobs); start += 500 {
		chunk := blobs[start:min(start+500, len(blobs))]
		err := s.notes.ReadBlobs(ctx, chunk, func(i int, content []byte) error {
			naming[chunk[i]] = re.Match(content)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	counts := map[string]int{}
	for _, u := range h.uses {
		if notes(u) && naming[u.blob] {
			counts[u.entry]++
		}
	}
	return counts, nil
}

// redactPaths replaces each of paths that message names as a whole: quoted
// as %q quotes it, or bare between the start or end of the message, spaces,
// quotes, brackets and the punctuation that ends a clause. A path inside a
// longer one, as a.md is in data.md or Old/a.md, is not named.
func redactPaths(message string, paths []string) string {
	paths = slices.Clone(paths)
	slices.SortFunc(paths, func(a, b string) int { return len(b) - len(a) })
	for _, p := range paths {
		if p == "" {
			continue
		}
		message = strings.ReplaceAll(message, strconv.Quote(p), strconv.Quote(purgedText))
		var b strings.Builder
		last := 0
		for i := 0; i < len(message); {
			k := strings.Index(message[i:], p)
			if k < 0 {
				break
			}
			start, end := i+k, i+k+len(p)
			before := start == 0 || strings.IndexByte(" \t\n\"'`([{<", message[start-1]) >= 0
			after := end == len(message) || strings.IndexByte(" \t\n\"'`)]}>,;:!?", message[end]) >= 0 ||
				message[end] == '.' && (end+1 == len(message) || strings.IndexByte(" \t\n", message[end+1]) >= 0)
			if !before || !after {
				i = start + 1
				continue
			}
			b.WriteString(message[last:start])
			b.WriteString(purgedText)
			last, i = end, end
		}
		if last > 0 {
			b.WriteString(message[last:])
			message = b.String()
		}
	}
	return message
}

// planVersions plans replacing the given versions of entry, wherever its
// history has them, with a placeholder that keeps the note's identity.
func (s *Store) planVersions(ctx context.Context, w *purgeWork, h *purgeHistory, entry string, versions []string) ([]purgeUse, error) {
	if len(versions) == 0 {
		return nil, fmt.Errorf("%w: a versions purge needs the versions to remove", ErrPurgeInvalid)
	}
	blobs := map[gitobj.ID]bool{}
	for _, v := range versions {
		id, err := parseRevision(v)
		if err != nil || id.IsZero() {
			return nil, fmt.Errorf("%w: version %q", ErrPurgeInvalid, v)
		}
		i := slices.IndexFunc(h.uses, func(u purgeUse) bool { return u.entry == entry && u.commit == id })
		if i < 0 {
			return nil, fmt.Errorf("%w: %s is not a version of %s", fs.ErrNotExist, v, w.Scope.Path)
		}
		if h.uses[i].mode == gitobj.ModeGitlink {
			return nil, ErrForbidden
		}
		blobs[h.uses[i].blob] = true
	}
	var affected []purgeUse
	shared := map[string]int{}
	var removed []string
	for _, u := range h.uses {
		switch {
		case u.entry == entry && blobs[u.blob]:
			affected = append(affected, u)
			removed = append(removed, u.commit.String())
		case blobs[u.blob]:
			shared[u.entry]++
		}
	}
	var err error
	if w.Shared, err = s.sharedElsewhere(ctx, occurrences(h, "file", shared), blobs); err != nil {
		return nil, err
	}
	w.summary = purgeSummary{Kind: PurgeVersions, Entries: []string{entry}, Versions: removed}
	w.fn = func(f notestore.RewriteFile, content func() ([]byte, error)) ([]byte, bool, error) {
		if f.EntryID != entry || !blobs[f.Blob] {
			return nil, true, nil
		}
		placeholder := []byte(purgedText + "\n")
		if strings.HasSuffix(f.Path, ".md") {
			raw, err := content()
			if err != nil {
				return nil, false, err
			}
			if id := NoteEntryID(f.Path, raw); id != "" {
				placeholder = []byte("---\nfmn-id: " + id + "\n---\n\n" + purgedText + "\n")
			}
		}
		return placeholder, true, nil
	}
	return affected, nil
}

// blobMatch is what a text purge found in one version: whether it is text,
// and what keeps redacting it, as each kind of file the version is in
// scope as, from running.
type blobMatch struct {
	text     bool
	problems map[purgeFileKind]redactProblem
}

// planText plans redacting the scope's text from every version of the
// files in scope, and from every commit message. It reads every version the
// notebook ever had, so it can report where else the text appears.
func (s *Store) planText(ctx context.Context, w *purgeWork, h *purgeHistory, scope PurgeScope, readTokens func([]byte)) ([]purgeUse, []string, error) {
	m, err := newPurgeMatcher(scope)
	if err != nil {
		return nil, nil, err
	}
	var selected map[string]bool
	if len(scope.Paths) > 0 {
		selected = map[string]bool{}
		for _, p := range scope.Paths {
			entry, err := h.entryAt(p)
			if err != nil {
				return nil, nil, err
			}
			selected[entry] = true
		}
	}
	inScope := func(entry string) bool { return selected == nil || selected[entry] }
	atHead := map[gitobj.ID]bool{}
	for _, f := range h.files {
		atHead[f.Blob] = true
	}
	var blobs []gitobj.ID
	kinds := map[gitobj.ID]map[purgeFileKind]bool{} // each version's kinds in scope
	for _, u := range h.uses {
		if u.mode == gitobj.ModeGitlink {
			continue
		}
		if kinds[u.blob] == nil {
			kinds[u.blob] = map[purgeFileKind]bool{}
			blobs = append(blobs, u.blob)
		}
		if inScope(u.entry) {
			kinds[u.blob][purgeKindOf(u.path)] = true
		}
	}
	found := map[gitobj.ID]blobMatch{}
	for start := 0; start < len(blobs); start += 500 {
		chunk := blobs[start:min(start+500, len(blobs))]
		err := s.notes.ReadBlobs(ctx, chunk, func(i int, content []byte) error {
			text := isText(content)
			if atHead[chunk[i]] && text {
				readTokens(content)
			}
			if m.holds(content) {
				match := blobMatch{text: text, problems: map[purgeFileKind]redactProblem{}}
				if text {
					for kind := range kinds[chunk[i]] {
						_, match.problems[kind] = s.redactFile(m, kind, content)
					}
				}
				found[chunk[i]] = match
			}
			return nil
		})
		if err != nil {
			return nil, nil, err
		}
	}
	var affected []purgeUse
	redacted := map[gitobj.ID]bool{}
	elsewhere, unredactable := map[string]int{}, map[string]int{}
	for _, u := range h.uses {
		f, ok := found[u.blob]
		switch {
		case !ok:
		case !inScope(u.entry):
			elsewhere[u.entry]++
		case !f.text:
			unredactable[u.entry]++
		case f.problems[purgeKindOf(u.path)] != redactable:
			w.Blocked = f.problems[purgeKindOf(u.path)].blocked(u.path)
		default:
			affected = append(affected, u)
			redacted[u.blob] = true
		}
	}
	w.Elsewhere = occurrences(h, "file", elsewhere)
	w.Unredactable = occurrences(h, "file", unredactable)
	if w.Elsewhere, err = s.sharedElsewhere(ctx, w.Elsewhere, redacted); err != nil {
		return nil, nil, err
	}
	for _, entry := range slices.Sorted(maps.Keys(h.paths)) {
		for _, p := range h.paths[entry] {
			if m.count([]byte(p)) == 0 {
				continue
			}
			if inScope(entry) {
				w.Blocked = fmt.Sprintf("the file name %s holds the text, and a purge cannot rename history; purge that file whole first", p)
			} else {
				w.Elsewhere = append(w.Elsewhere, PurgeOccurrence{Path: p, Kind: "name"})
			}
		}
	}
	var firstMessage int64
	for _, seq := range h.order {
		message := h.messages[seq]
		if m.count([]byte(message)) == 0 {
			continue
		}
		w.Messages++
		if firstMessage == 0 {
			firstMessage = seq
		}
		// The trailers record who made the revision and how, which the
		// store reads back from the rewritten commit.
		switch redacted := m.redactMessage(message); {
		case m.count([]byte(redacted)) > 0:
			w.Blocked = "commit messages hold the text in the trailers ForgetMeNote writes (Gitnotes-Source, Gitnotes-User and the like), which record who made each revision and how and which a purge keeps; narrow the text or pattern"
		case !sameTrailers(message, redacted):
			w.Blocked = fmt.Sprintf("redacting the message of revision %s would change its trailers, which record who made it and how; narrow the pattern", h.commitAt(seq))
		}
	}
	if firstMessage > 0 {
		// A revision whose message alone holds the text starts the rewrite
		// as a file version would.
		affected = append(affected, purgeUse{seq: firstMessage})
	}
	// A rewrite keeps every commit's author and committer, so the plan
	// only lists the commits whose names or emails hold the text.
	authored := 0
	for _, seq := range h.order {
		if slices.ContainsFunc(h.people[seq], func(v string) bool { return m.count([]byte(v)) > 0 }) {
			authored++
		}
	}
	if authored > 0 {
		w.Elsewhere = append(w.Elsewhere, PurgeOccurrence{Kind: "author", Versions: authored})
	}
	// Live documents whose stored edits hold the text, committed or not.
	live, err := s.notes.LiveEntries(ctx)
	if err != nil {
		return nil, nil, err
	}
	for _, entry := range live {
		st, err := s.notes.LiveState(ctx, entry)
		if err != nil {
			return nil, nil, err
		}
		if !liveHolds(st, m) {
			continue
		}
		if inScope(entry) {
			w.reset = append(w.reset, entry)
			continue
		}
		// A note whose saved text holds it is listed by its versions
		// already: its live document, which starts from that text when
		// someone opens the note, tells nothing more.
		path, current := h.pathOf(entry)
		if _, saved := found[h.files[path].Blob]; current && saved {
			continue
		}
		w.Elsewhere = append(w.Elsewhere, PurgeOccurrence{Path: path, Kind: "live"})
	}
	var entries []string
	touched := map[string]bool{}
	for _, u := range affected {
		if u.entry != "" && !touched[u.entry] {
			touched[u.entry] = true
			entries = append(entries, u.entry)
		}
	}
	slices.Sort(entries)
	w.summary = purgeSummary{Kind: PurgeText, Pattern: scope.Pattern != "", Notebook: selected == nil, Entries: entries, Messages: w.Messages}
	w.fn = func(f notestore.RewriteFile, content func() ([]byte, error)) ([]byte, bool, error) {
		if !redacted[f.Blob] || !inScope(f.EntryID) {
			return nil, true, nil
		}
		raw, err := content()
		if err != nil {
			return nil, false, err
		}
		redacted, _ := s.redactFile(m, purgeKindOf(f.Path), raw)
		return redacted, true, nil
	}
	w.message = func(_ int64, message string) (string, error) {
		// Only the messages the plan counted: a redaction by parts could
		// match a pattern's anchors where the whole message does not.
		if m.count([]byte(message)) == 0 {
			return message, nil
		}
		return m.redactMessage(message), nil
	}
	return affected, entries, nil
}

// liveHolds reports whether a live document's stored bytes, or its text and
// history once decoded, hold what m finds. A document it cannot decode
// counts as holding it.
func liveHolds(st notestore.LiveState, m purgeMatcher) bool {
	if m.count(st.Snapshot) > 0 {
		return true
	}
	for _, u := range st.Updates {
		if m.count(u.Update) > 0 {
			return true
		}
	}
	if st.Snapshot == nil && len(st.Updates) == 0 {
		return false
	}
	updates := make([][]byte, len(st.Updates))
	for i, u := range st.Updates {
		updates[i] = u.Update
	}
	doc, err := yjsLive.Load(st.Snapshot, updates)
	if err != nil {
		return true
	}
	return m.count([]byte(doc.Text())) > 0 || m.count(doc.Snapshot()) > 0
}
