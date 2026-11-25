package workspace

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"html"
	"path"
	"regexp"
	"slices"
	"strings"

	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"

	"github.com/yuin/goldmark/util"
)

// The links of notes in the Trash an organization move relocates a note
// for. An ordinary move rewrites them or refuses (checkTrashLinkRewrites);
// an organization move rewrites them on a best-effort basis, as the one
// exception to its selected scope, and leaves a note it may not change as
// it is. Its undo reverts them on the same basis. Results list these notes
// in TrashLinks, not in Files, so they count against neither the action's
// size limits nor its Undo's guards. The call that commits a move or an
// undo answers with what it planned under the notebook's lock, once its
// commit shows it rewrote exactly the planned notes; replays and recovered
// receipts, which have no plan, derive them again from the revisions: the
// notes a revision rewrote are its changes, and why it left the others is
// what the same checks find at its parent.

// Why an organization move or its undo left a note in the Trash as it was.
const (
	trashLinkUnsupported = "it refers to the note with wiki or HTML syntax, which a move cannot update"
	trashLinkUnsafe      = "its Markdown links to the note cannot be updated safely"
	trashLinkUnread      = "it could not be read to check its links"
	trashLinkUnchanged   = "the move left it unchanged"
	trashLinkChanged     = "it changed since the action"
	trashLinkRestored    = "it was restored from the Trash since"
	trashLinkEmptied     = "it was emptied from the Trash since"
	trashLinkGone        = "it is no longer in the Trash"
)

// reason is why a note in the Trash, trashed from from, was left as it
// was.
func (b trashLinkBlock) reason(from string) string {
	switch b {
	case trashLinkScoped:
		return "this action may not change notes in the Trash"
	case trashLinkReadOnly:
		return "the notebook is read-only"
	case trashLinkLocked:
		return "a lock rule covers it"
	case trashLinkOriginLocked:
		return "its original location " + from + " is locked"
	}
	return ""
}

// trashOrigins maps each note in the Trash in index to the location it was
// trashed from.
func trashOrigins(index TrashIndex) map[string]string {
	origins := map[string]string{}
	for _, r := range index.Records {
		if !r.Emptied && !r.Restored {
			origins[r.Path] = r.OriginalPath
		}
	}
	return origins
}

// trashRecords maps the path of each record in index to the record: the
// note's, while it is in the Trash, and a restored or emptied one's after.
func trashRecords(index TrashIndex) map[string]TrashRecord {
	records := map[string]TrashRecord{}
	for _, r := range index.Records {
		records[r.Path] = r
	}
	return records
}

// trashIndexAt is the Trash index at revision, empty where there is none.
// Its archives hold only records of notes no longer in the Trash, which a
// record missing from it stands for.
func (n *dbNotebook) trashIndexAt(ctx context.Context, revision string) (TrashIndex, error) {
	index := TrashIndex{Version: 1, Records: []TrashRecord{}}
	raw, _, err := n.policyFile(ctx, revision, trashIndexPath, trashFileBytes, "Trash index exceeds 8 MiB")
	if err != nil || raw == nil {
		return index, err
	}
	if json.Unmarshal(raw, &index) != nil || index.Version != 1 {
		return index, errors.New("invalid Trash index")
	}
	return index, nil
}

// locksAt is the lock rules at revision, as loadLocks reads them at the
// head.
func (n *dbNotebook) locksAt(ctx context.Context, revision string) ([]string, error) {
	if revision == n.locksRevision {
		return n.projectLocks, nil
	}
	raw, _, err := n.policyFile(ctx, revision, locksPath, 65536, "lock policy exceeds 64 KiB")
	if err != nil || raw == nil {
		return nil, err
	}
	var policy lockPolicy
	if json.Unmarshal(raw, &policy) != nil || policy.Version != 1 {
		return nil, errors.New("invalid project lock policy")
	}
	return policy.Paths, nil
}

// trashLinkRewrite reports whether f is a note in the Trash whose links a
// revision rewrote in place: an organization move's or its undo's.
func trashLinkRewrite(f OrganizationFile) bool {
	return IsTrash(f.Path) && strings.HasSuffix(f.Path, ".md") && f.Before != nil && f.After != nil
}

// splitTrashLinks takes the notes in the Trash whose links result's
// revision rewrote out of its files and returns their paths.
func splitTrashLinks(result *OrganizationResult) map[string]bool {
	rewritten := map[string]bool{}
	files := result.Files[:0]
	for _, f := range result.Files {
		if trashLinkRewrite(f) {
			rewritten[f.Path] = true
		} else {
			files = append(files, f)
		}
	}
	result.Files = files
	return rewritten
}

func sortTrashLinks(links []OrganizationTrashLink) {
	slices.SortFunc(links, func(a, b OrganizationTrashLink) int { return strings.Compare(a.Path, b.Path) })
}

// trashLinkCandidate reports whether content, a note in the Trash, may
// refer to the note at from, so that a move's plan must parse it. A link to
// that note spells its file name unless an escape spells part of it, and a
// wiki link names it; a note with neither cannot refer to it. Most notes in
// a Trash do not, and this spares parsing them for every move.
func trashLinkCandidate(content []byte, from string) bool {
	return bytes.Contains(content, []byte(path.Base(from))) || mayEscape(content) || wikiReference(content, from)
}

// mayEscape reports whether content has what could stand for a character
// of a link destination: a backslash before ASCII punctuation, a percent
// escape or a character reference.
func mayEscape(content []byte) bool {
	for rest := content; ; {
		i := bytes.IndexAny(rest, `\%&`)
		if i < 0 {
			return false
		}
		c, next := rest[i], rest[i+1:]
		switch {
		case c == '\\' && len(next) > 0 && util.IsPunct(next[0]):
			return true
		case c == '%' && len(next) > 1 && isHex(next[0]) && isHex(next[1]):
			return true
		case c == '&' && len(next) > 0 && next[0] == '#':
			return true
		case c == '&':
			// The longest named reference has 32 characters after its &.
			ref := string(rest[i:min(len(rest), i+40)])
			if html.UnescapeString(ref) != ref {
				return true
			}
		}
		rest = next
	}
}

func isHex(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

// wikiReference reports whether content has a wiki link naming the note at
// from by file name or title, as Obsidian matches them, in any case:
// [[a]], [[a.md]], [[folder/a|label]] or [[a#heading]]. A wiki link stays
// on one line.
func wikiReference(content []byte, from string) bool {
	name := path.Base(from)
	title := strings.TrimSuffix(name, ".md")
	for rest := content; ; {
		i := bytes.Index(rest, []byte("[["))
		if i < 0 {
			return false
		}
		rest = rest[i+2:]
		line := rest[:min(len(rest), 1024)]
		if end := bytes.IndexByte(line, '\n'); end >= 0 {
			line = line[:end]
		}
		end := bytes.Index(line, []byte("]]"))
		if end < 0 {
			continue
		}
		target, _, _ := strings.Cut(string(line[:end]), "|")
		target, _, _ = strings.Cut(target, "#")
		target = path.Base(strings.TrimSpace(target))
		if strings.EqualFold(target, name) || strings.EqualFold(target, title) {
			return true
		}
	}
}

var trashHTMLReference = regexp.MustCompile(`(?i)<(?:a|img)\s[^>]*>`)

// trashUnsupportedReference reports whether content refers to the note at
// from with syntax a move cannot update: a wiki link naming it, or an HTML
// link or image whose tag could name it. It is unsupportedReference made
// exact for notes in the Trash, which a move names rather than refuses: a
// note it names for nothing would be named in every move.
func trashUnsupportedReference(content []byte, from string) bool {
	body := MarkdownBody(content)
	if wikiReference(body, from) {
		return true
	}
	name := []byte(path.Base(from))
	for _, tag := range trashHTMLReference.FindAll(body, -1) {
		if namesFile(tag, name) || mayEscape(tag) {
			return true
		}
	}
	return false
}

// namesFile reports whether tag, an HTML tag, has name as a whole file
// name: not as the end of a longer one, such as a.md in data.md, nor the
// start of one, such as a.md in a.mdx.
func namesFile(tag, name []byte) bool {
	for at := 0; ; {
		i := bytes.Index(tag[at:], name)
		if i < 0 {
			return false
		}
		start, end := at+i, at+i+len(name)
		before := start == 0 || bytes.IndexByte([]byte("/\"'= \t\r\n"), tag[start-1]) >= 0
		after := end == len(tag) || bytes.IndexByte([]byte("\"'#?/ \t\r\n>"), tag[end]) >= 0
		if before && after {
			return true
		}
		at = start + 1
	}
}

// cannotName reports whether href, a destination the renderer cannot
// parse, cannot name the note at from: its file name is not in it even
// with its valid percent escapes decoded. Such a destination reaches the
// same place before and after a move, whatever that is, so verifying a
// note in the Trash leaves it out instead of leaving the note as it is.
func cannotName(from string) func(string) bool {
	name := path.Base(from)
	return func(href string) bool {
		var decoded strings.Builder
		for i := 0; i < len(href); i++ {
			if href[i] == '%' && i+2 < len(href) && isHex(href[i+1]) && isHex(href[i+2]) {
				decoded.WriteByte(unhex(href[i+1])<<4 | unhex(href[i+2]))
				i += 2
				continue
			}
			decoded.WriteByte(href[i])
		}
		return !strings.Contains(decoded.String(), name)
	}
}

func unhex(c byte) byte {
	switch {
	case c <= '9':
		return c - '0'
	case c >= 'a':
		return c - 'a' + 10
	}
	return c - 'A' + 10
}

// planTrashLinks plans what a move of from to to does to the notes in the
// Trash at revision: each note that links to from, whether the move
// updates it and why not, and the rewritten content of each it updates.
// origins maps the notes in the Trash to where they were trashed from;
// locks, readOnly and scoped are what checkTrashLinkRewrites checks. As
// with the backlinks in the notebook, a note is rewritten only when its
// rewritten links verifiably reach what they reached, and never in part;
// unlike them, a note the move may not rewrite is named, not refused. Only
// a note that may refer to from is parsed. A symbolic link or a submodule
// holds no links; a note no one can read, such as one too large, is named
// as unchecked.
func (n *dbNotebook) planTrashLinks(ctx context.Context, revision string, origins map[string]string, from, to string, locks []string, readOnly, scoped bool) ([]OrganizationTrashLink, map[string][]byte, error) {
	files, err := n.files(ctx, revision)
	if err != nil {
		return nil, nil, err
	}
	var links []OrganizationTrashLink
	var selected []notestore.File
	var limits []int
	for _, f := range files {
		if !IsTrash(f.Path) || !strings.HasSuffix(f.Path, ".md") || f.Mode == gitobj.ModeSymlink || f.Mode == gitobj.ModeGitlink {
			continue
		}
		limit := MaxReadBytes(f.Path)
		if _, err := n.resolve(f.Path, false); err != nil || f.Mode != gitobj.ModeFile || f.Size > int64(limit) {
			links = append(links, OrganizationTrashLink{Path: f.Path, OriginalPath: origins[f.Path], Reason: trashLinkUnread})
			continue
		}
		selected = append(selected, f)
		limits = append(limits, limit)
	}
	rewrites := map[string][]byte{}
	ignore := cannotName(from)
	err = n.readBlobsBounded(ctx, selected, limits, func(i int, content []byte) {
		p := selected[i].Path
		if !trashLinkCandidate(content, from) {
			return
		}
		after := rewriteLinks(content, p, p, from, to)
		// A link the rewriter does not recognise leaves the content as it
		// was; only the parsed destinations show it reaches from.
		unsupported := trashUnsupportedReference(content, from)
		unsafe := verifyLinkTargets(content, after, p, p, from, to, ignore) != nil
		if bytes.Equal(after, content) && !unsupported && !unsafe {
			return
		}
		link := OrganizationTrashLink{Path: p, OriginalPath: origins[p]}
		switch block := trashLinkCheck(p, link.OriginalPath, locks, readOnly, scoped); {
		case block != trashLinkFree:
			link.Reason = block.reason(link.OriginalPath)
		case unsupported:
			link.Reason = trashLinkUnsupported
		case unsafe:
			link.Reason = trashLinkUnsafe
		default:
			link.Updated = true
			rewrites[p] = bytes.Clone(after)
		}
		links = append(links, link)
	})
	if err != nil {
		return nil, nil, err
	}
	sortTrashLinks(links)
	return links, rewrites, nil
}

// moveTrashLinks names in result, the receipt of the organization move a
// at revision info, the notes in the Trash that link to the moved note:
// those it rewrote, which it takes out of result's files, and those it left
// as they were, for the reason the move's checks find at its parent.
func (n *dbNotebook) moveTrashLinks(ctx context.Context, result *OrganizationResult, info notestore.CommitInfo, a OrganizationAction) error {
	if a.Kind != "move" {
		return nil
	}
	rewritten := splitTrashLinks(result)
	if len(info.Commit.Parents) != 1 {
		return nil
	}
	parent := info.Commit.Parents[0].String()
	index, err := n.trashIndexAt(ctx, parent)
	if err != nil {
		return err
	}
	locks, err := n.locksAt(ctx, parent)
	if err != nil {
		return err
	}
	origins := trashOrigins(index)
	links, _, err := n.planTrashLinks(ctx, parent, origins, a.Path, a.Target, locks, false, false)
	if err != nil {
		return err
	}
	for i := range links {
		switch link := &links[i]; {
		case rewritten[link.Path]:
			link.Updated, link.Reason = true, ""
			delete(rewritten, link.Path)
		case link.Updated:
			// A move made before organization moves updated notes in the
			// Trash left them all as they were.
			link.Updated, link.Reason = false, trashLinkUnchanged
		}
	}
	for p := range rewritten {
		links = append(links, OrganizationTrashLink{Path: p, OriginalPath: origins[p], Updated: true})
	}
	sortTrashLinks(links)
	result.TrashLinks = links
	return nil
}

// plannedTrashLinks reports whether info, an organization move or undo
// committed on base, rewrote in the Trash exactly the notes rewrites
// plans, each to its planned content.
func plannedTrashLinks(info notestore.CommitInfo, base string, rewrites map[string][]byte) bool {
	if len(info.Commit.Parents) != 1 || info.Commit.Parents[0].String() != base {
		return false
	}
	rewritten := 0
	for _, c := range info.Changes {
		after, planned := rewrites[c.Path]
		switch {
		case planned && (c.Op != notestore.OpModify || c.Blob != gitobj.BlobID(after)):
			return false
		case planned:
			rewritten++
		case c.Op == notestore.OpModify && IsTrash(c.Path) && strings.HasSuffix(c.Path, ".md"):
			return false
		}
	}
	return rewritten == len(rewrites)
}

// plannedResult is the result of the organization move or undo at
// revision, committed by a call that planned rewrites on base, the notes
// in the Trash it rewrites, and named links. When the revision is that
// commit, the result names links; otherwise, as when another gateway
// committed the same action, receipt names the notes from the revisions.
func (n *dbNotebook) plannedResult(ctx context.Context, revision string, replayed bool, base string, rewrites map[string][]byte, links []OrganizationTrashLink, receipt func(*OrganizationResult, notestore.CommitInfo) error) (OrganizationResult, error) {
	result, info, err := n.organizationRevision(ctx, revision, replayed)
	if err != nil {
		return result, err
	}
	if replayed || !plannedTrashLinks(info, base, rewrites) {
		return result, receipt(&result, info)
	}
	splitTrashLinks(&result)
	result.TrashLinks = links
	return result, nil
}

// trashListing maps the path of each note in the Trash in files, a
// revision's listing, to its file.
func trashListing(files []notestore.File) map[string]notestore.File {
	listing := map[string]notestore.File{}
	for _, f := range files {
		if IsTrash(f.Path) && strings.HasSuffix(f.Path, ".md") {
			listing[f.Path] = f
		}
	}
	return listing
}

// undoTrashLinkSkip is why the undo of an organization move leaves file, a
// note in the Trash the move rewrote, as it is in listing, the notes in the
// Trash where the undo applies, or "" when its state there lets the undo
// revert it: the note is still in the Trash, holds what the move left and
// is not locked. records are the Trash records there by path. It compares
// blob IDs, so it reads no note.
func undoTrashLinkSkip(listing map[string]notestore.File, file OrganizationFile, records map[string]TrashRecord, locks []string, readOnly, scoped bool) string {
	current, found := listing[file.Path]
	record, recorded := records[file.Path]
	switch {
	case !found && recorded && record.Restored:
		return trashLinkRestored
	case !found && recorded && record.Emptied:
		return trashLinkEmptied
	case !found:
		return trashLinkGone
	}
	if block := trashLinkCheck(file.Path, record.OriginalPath, locks, readOnly, scoped); block != trashLinkFree {
		return block.reason(record.OriginalPath)
	}
	if current.Mode != gitobj.ModeFile || current.Blob != gitobj.BlobID([]byte(*file.After)) {
		return trashLinkChanged
	}
	return ""
}

// undoTrashLinks names in result, the receipt of the undo at revision info
// of the organization action input names, the notes in the Trash whose
// links the action rewrote: those the undo reverted, which it takes out of
// result's files, and those it left as they were, for the reason the
// undo's checks find at its parent. What they find nothing against there
// is a note that changed since the action and changed back. The action's
// revision is the one its operation finds, not input's: a purge gives it a
// new one, and may leave it no changes, and then the undo names the notes
// it reverted itself.
func (n *dbNotebook) undoTrashLinks(ctx context.Context, result *OrganizationResult, info notestore.CommitInfo, input OrganizationUndo) error {
	if input.Action.Kind != "move" {
		return nil
	}
	reverted := splitTrashLinks(result)
	if len(info.Commit.Parents) != 1 {
		return nil
	}
	parent := info.Commit.Parents[0].String()
	index, err := n.trashIndexAt(ctx, parent)
	if err != nil {
		return err
	}
	records := trashRecords(index)
	var links []OrganizationTrashLink
	original, err := n.operationReceipt(ctx, organizationOperation(input.Action, input.Actor))
	if err != nil {
		return err
	}
	if !original.Replayed {
		for p := range reverted {
			links = append(links, OrganizationTrashLink{Path: p, OriginalPath: records[p].OriginalPath, Updated: true})
		}
		sortTrashLinks(links)
		result.TrashLinks = links
		return nil
	}
	applied, _, err := n.organizationRevision(ctx, original.Revision, false)
	if err != nil {
		return err
	}
	locks, err := n.locksAt(ctx, parent)
	if err != nil {
		return err
	}
	files, err := n.files(ctx, parent)
	if err != nil {
		return err
	}
	listing := trashListing(files)
	for _, file := range applied.Files {
		if !trashLinkRewrite(file) {
			continue
		}
		link := OrganizationTrashLink{Path: file.Path, OriginalPath: records[file.Path].OriginalPath, Updated: reverted[file.Path]}
		if !link.Updated {
			if link.Reason = undoTrashLinkSkip(listing, file, records, locks, false, false); link.Reason == "" {
				link.Reason = trashLinkChanged
			}
		}
		links = append(links, link)
	}
	result.TrashLinks = links
	return nil
}
