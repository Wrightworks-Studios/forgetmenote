package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
)

// Group A: listings, search, browsing, collections, snapshots, changes and
// knowledge reads of notebooks kept in the notes store. Each answers what its
// Git original answers. A read captures the head, policy and authority under
// mu, reads immutable content without it, and checks under mu again that
// authority and visibility still hold before it discloses anything.

func (n *dbNotebook) List(ctx context.Context) ([]Entry, error) {
	l, err := n.listing(ctx, nil)
	return l.entries, err
}

// ArchivedSources is the archived source notes of the head's listing and the
// revision it lists.
func (n *dbNotebook) ArchivedSources(ctx context.Context) (string, []Entry, error) {
	l, err := n.listing(ctx, nil)
	if err != nil {
		return "", nil, err
	}
	var out []Entry
	for _, entry := range l.entries {
		if strings.HasSuffix(entry.Path, ".md") && entry.Metadata != nil && entry.Metadata.Source != nil {
			out = append(out, entry)
		}
	}
	return l.revision, out, nil
}

func (n *dbNotebook) Find(ctx context.Context, query, tag, status, pathPrefix string) ([]Entry, error) {
	l, err := n.listing(ctx, &listingQuery{query: query, tag: tag, status: status, pathPrefix: pathPrefix})
	return l.entries, err
}

func (n *dbNotebook) ListCollection(ctx context.Context, collection string) ([]Entry, error) {
	entries, _, err := n.collection(ctx, collection)
	return entries, err
}

func (n *dbNotebook) Collections(ctx context.Context) (Collections, error) {
	l, err := n.listing(ctx, nil)
	if err != nil {
		return Collections{}, err
	}
	result := Collections{Version: 1, Revision: l.revision, Visibility: l.visibility}
	for _, entry := range l.entries {
		if !strings.HasSuffix(entry.Path, ".md") {
			continue
		}
		switch {
		case entry.Metadata != nil && entry.Metadata.ContainerError != "":
			result.Invalid++
			result.Notes++
		case InCollection(entry.Metadata, "drafts"):
			result.Drafts++
		case InCollection(entry.Metadata, "archives"):
			result.Archives++
		default:
			result.Notes++
		}
	}
	return result, nil
}

func (n *dbNotebook) BrowseCollection(ctx context.Context, folder, collection string) (FolderBrowse, error) {
	folder = strings.TrimSuffix(folder, "/")
	if folder != "" {
		if err := n.resolveAtHead(ctx, folder); err != nil || path.Clean(folder) != folder {
			return FolderBrowse{}, ErrForbidden
		}
	}
	entries, l, err := n.collection(ctx, collection)
	if err != nil {
		return FolderBrowse{}, err
	}
	result := FolderBrowse{Collection: collection, Visibility: l.visibility, Path: folder, Revision: l.revision, Items: []BrowseItem{}}
	return browseEntries(result, entries), nil
}

// collection is the entries of the head's listing in collection.
func (n *dbNotebook) collection(ctx context.Context, collection string) ([]Entry, dbListing, error) {
	if !ValidCollection(collection) {
		return nil, dbListing{}, invalidInput("unknown note collection")
	}
	l, err := n.listing(ctx, nil)
	if err != nil {
		return nil, l, err
	}
	filtered := []Entry{}
	for _, entry := range l.entries {
		if collection != "" && collection != "all" && !strings.HasSuffix(entry.Path, ".md") {
			continue
		}
		if InCollection(entry.Metadata, collection) {
			filtered = append(filtered, entry)
		}
	}
	return filtered, l, nil
}

// A dbListing is the listing of one revision under one visibility.
type dbListing struct {
	revision, visibility string
	entries              []Entry
}

// listing is the files of the head and their metadata, which follows blob
// identity, so a new revision rereads only the notes it changed; and with a
// query, the notes Git's grep would find, then those whose path contains
// the query.
func (n *dbNotebook) listing(ctx context.Context, query *listingQuery) (dbListing, error) {
	n.mu.Lock()
	if err := n.loadLocks(ctx); err != nil {
		n.mu.Unlock()
		return dbListing{}, err
	}
	selection := listingSelection{revision: n.locksRevision, generation: n.authorityGeneration.Load(), exclusions: slices.Clone(n.browseExcluded), metadataRevision: n.metadataHead, objects: n.metadataObjects, metadata: n.metadataCache, paths: n.metadataPaths}
	n.mu.Unlock()
	if query != nil && len(query.query) > 200 {
		return dbListing{}, invalidInput("search is limited to 200 characters")
	}
	if selection.metadataRevision != selection.revision {
		metadata, objects, paths, err := n.readMetadataRevision(ctx, selection.revision, selection.exclusions, selection.objects, selection.metadata)
		if err != nil {
			return dbListing{}, err
		}
		selection.metadata, selection.objects, selection.paths = metadata, objects, paths
	}
	paths := selection.paths
	if query != nil {
		matched, err := n.grepNotes(ctx, selection, query.query)
		if err != nil {
			return dbListing{}, err
		}
		found, lower := []string{}, strings.ToLower(query.query)
		for _, p := range paths {
			if matched[p] {
				found = append(found, p)
			}
		}
		for _, p := range paths {
			if strings.HasSuffix(p, ".md") && !matched[p] && strings.Contains(strings.ToLower(p), lower) {
				found = append(found, p)
			}
		}
		paths = found
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	visibility := historyVisibilityID(selection.exclusions)
	if err := n.stillVisible(selection.generation, visibility); err != nil {
		return dbListing{}, err
	}
	// Avoid replacing a newer cache populated by a reader or intervening writer.
	if n.observedHead == selection.revision {
		n.metadataHead, n.metadataObjects, n.metadataCache, n.metadataPaths = selection.revision, selection.objects, selection.metadata, selection.paths
	}
	entries := n.listedEntries(paths, selection.metadata, selection.exclusions)
	if query != nil {
		entries = filterFoundEntries(entries, query.tag, query.status, query.pathPrefix)
	}
	return dbListing{revision: selection.revision, visibility: visibility, entries: entries}, nil
}

// stillVisible fails a read whose authority or visibility changed while it
// read without mu. Callers hold mu.
func (n *dbNotebook) stillVisible(generation uint64, visibility string) error {
	if n.authorityGeneration.Load() != generation || historyVisibilityID(n.browseExcluded) != visibility {
		return ErrConflict
	}
	return nil
}

// stillVisibleNow is stillVisible for callers that do not hold mu.
func (n *dbNotebook) stillVisibleNow(generation uint64, visibility string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.stillVisible(generation, visibility)
}

// resolveAtHead is resolve with the check the Git backend makes of its
// working tree, which holds the head: it refuses a path that is a symbolic
// link at the head, or lies under a link or a file there. Only a path of an
// older revision can fail it, and reads of one refuse it as Git's did.
func (n *dbNotebook) resolveAtHead(ctx context.Context, p string) error {
	if _, err := n.resolve(p, false); err != nil {
		return err
	}
	for q, self := p, true; ; self = false {
		f, err := n.notes.Stat(ctx, gitobj.ID{}, q)
		if err == nil && (f.Mode == gitobj.ModeSymlink || !self && f.Mode != gitobj.ModeGitlink) {
			return ErrForbidden
		}
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		i := strings.LastIndexByte(q, '/')
		if i < 0 {
			return nil
		}
		q = q[:i]
	}
}

// visibleAtHead is visibleNote with resolveAtHead; it fails only when the
// store cannot answer. Callers hold mu.
func (n *dbNotebook) visibleAtHead(ctx context.Context, p string) (bool, error) {
	if !n.visibleNote(p) {
		return false, nil
	}
	err := n.resolveAtHead(ctx, p)
	if errors.Is(err, ErrForbidden) {
		return false, nil
	}
	return err == nil, err
}

// listedEntries are the browseable paths with their cached metadata, each a
// private copy. Callers hold mu.
func (n *dbNotebook) listedEntries(paths []string, metadata map[string]Entry, exclusions []string) []Entry {
	entries := []Entry{}
	for _, p := range paths {
		if IsTrash(p) || matchesPrefix(p, exclusions) {
			continue
		}
		if _, err := n.resolve(p, false); err != nil {
			continue
		}
		entry, ok := metadata[p]
		if !ok {
			entry = Entry{Path: p}
		}
		entry.Editable = n.editable(p)
		entries = append(entries, cloneEntry(entry))
	}
	return entries
}

// list is the committed listing at the head for callers that hold mu, such
// as a source import looking for an earlier capture.
func (n *dbNotebook) list(ctx context.Context) ([]Entry, error) {
	if err := n.loadLocks(ctx); err != nil {
		return nil, err
	}
	if err := n.refreshMetadata(ctx); err != nil {
		return nil, err
	}
	return n.listedEntries(n.metadataPaths, n.metadataCache, n.browseExcluded), nil
}

// refreshMetadata brings the metadata cache to the head. Callers hold mu.
func (n *dbNotebook) refreshMetadata(ctx context.Context) error {
	head, err := n.head(ctx)
	if err != nil {
		return err
	}
	if head == n.metadataHead {
		return nil
	}
	next, objects, paths, err := n.readMetadataRevision(ctx, head, n.browseExcluded, n.metadataObjects, n.metadataCache)
	if err != nil {
		return err
	}
	n.metadataHead, n.metadataCache, n.metadataObjects, n.metadataPaths = head, next, objects, paths
	return nil
}

// readMetadataRevision lists the regular files of revision and the metadata
// of its visible notes, reusing what previousCache holds for the same blob
// wherever that note now lies. It reads only immutable content and publishes
// nothing, so callers need not hold mu.
func (n *dbNotebook) readMetadataRevision(ctx context.Context, revision string, exclusions []string, previousObjects map[string]string, previousCache map[string]Entry) (map[string]Entry, map[string]string, []string, error) {
	files, err := n.files(ctx, revision)
	if err != nil {
		return nil, nil, nil, err
	}
	parsed := make(map[string]Entry, len(previousCache))
	for p, blob := range previousObjects {
		if entry, ok := previousCache[p]; ok {
			parsed[blob] = entry
		}
	}
	next := map[string]Entry{}
	paths := []string{}
	objects := map[string]string{}
	var reads []notestore.File
	for _, f := range files {
		if f.Mode != gitobj.ModeFile && f.Mode != gitobj.ModeExecutable {
			continue
		}
		// Tree membership is immutable; access is rechecked for every
		// returned listing.
		paths = append(paths, f.Path)
		if !strings.HasSuffix(f.Path, ".md") || matchesPrefix(f.Path, exclusions) {
			continue
		}
		blob := f.Blob.String()
		objects[f.Path] = blob
		if entry, ok := parsed[blob]; ok {
			entry.Path = f.Path
			next[f.Path] = entry
			continue
		}
		if f.Size < 0 || f.Size > int64(MaxFileBytes()) {
			next[f.Path] = Entry{Path: f.Path, MetadataError: "note exceeds file limit"}
			continue
		}
		reads = append(reads, f)
	}
	// Parsing dominates a first listing, so notes are read and parsed by a
	// few workers at once.
	read := make([]Entry, len(reads))
	err = inParallel(ctx, len(reads), func(ctx context.Context, _, start, end int) error {
		return n.readBlobs(ctx, reads[start:end], func(i int, raw []byte) {
			entry := Entry{Path: reads[start+i].Path}
			m, e := ParseMetadata(raw)
			entry.Metadata = &m
			if e != nil {
				entry.MetadataError = e.Error()
			} else {
				entry.Title, entry.Excerpt = NoteSummary(raw)
			}
			read[start+i] = entry
		})
	})
	if err != nil {
		return nil, nil, nil, err
	}
	for _, entry := range read {
		next[entry.Path] = entry
	}
	return next, objects, paths, nil
}

// readWorkers bounds the workers one read uses.
const readWorkers = 4

// inParallel runs work over count items in up to readWorkers contiguous
// ranges at once, telling each its worker number, and returns the first
// error, after which the other workers' context is cancelled.
func inParallel(ctx context.Context, count int, work func(ctx context.Context, worker, start, end int) error) error {
	workers := min(readWorkers, runtime.GOMAXPROCS(0), count)
	if workers <= 1 {
		return work(ctx, 0, 0, count)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var mu sync.Mutex
	var first error
	var wg sync.WaitGroup
	for w := range workers {
		wg.Go(func() {
			if err := work(ctx, w, count*w/workers, count*(w+1)/workers); err != nil {
				mu.Lock()
				if first == nil {
					first = err
					cancel()
				}
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	return first
}

// grepNotes returns the notes of a listing whose text contains query as
// "git grep -i -I -F -e query" finds it in production. It reads only the
// notes the listing can return, each distinct content once.
func (n *dbNotebook) grepNotes(ctx context.Context, selection listingSelection, query string) (map[string]bool, error) {
	q, err := newGrepQuery(query)
	if err != nil {
		return nil, err
	}
	var blobs []gitobj.ID
	index := map[gitobj.ID]int{}
	for _, p := range selection.paths {
		raw, ok := selection.objects[p]
		if !ok || IsTrash(p) {
			continue
		}
		if _, err := n.resolve(p, false); err != nil {
			continue
		}
		blob, err := gitobj.ParseID(raw)
		if err != nil {
			return nil, err
		}
		if _, ok := index[blob]; !ok {
			index[blob] = len(blobs)
			blobs = append(blobs, blob)
		}
	}
	hits := make([]bool, len(blobs))
	scratch := make([][]byte, readWorkers)
	err = inParallel(ctx, len(blobs), func(ctx context.Context, worker, start, end int) error {
		return n.notes.ReadBlobs(ctx, blobs[start:end], func(i int, content []byte) error {
			hits[start+i] = q.matches(content, &scratch[worker])
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	matched := map[string]bool{}
	for p, raw := range selection.objects {
		blob, err := gitobj.ParseID(raw)
		if err != nil {
			return nil, err
		}
		if i, ok := index[blob]; ok && hits[i] {
			matched[p] = true
		}
	}
	return matched, nil
}

// A grepQuery matches text as "git grep -i -I -F" does under production's
// LANG=C.UTF-8, where PCRE2 folds case by Unicode simple case folding. Each
// line of the query is a pattern of its own that may match within one line;
// an empty pattern matches every line; a binary file, with a NUL in its first
// 8000 bytes, never matches; and invalid UTF-8 matches nothing.
type grepQuery struct {
	patterns [][]byte // folded
	anyLine  bool
}

func newGrepQuery(query string) (grepQuery, error) {
	// Git refuses such a pattern rather than search with it.
	if !utf8.ValidString(query) || strings.ContainsRune(query, 0) {
		return grepQuery{}, invalidInput("search text must be UTF-8 without NUL characters")
	}
	var q grepQuery
	for _, p := range strings.Split(query, "\n") {
		if p == "" {
			q.anyLine = true
		} else {
			q.patterns = append(q.patterns, foldCase(nil, []byte(p)))
		}
	}
	return q, nil
}

// matches reports whether a line of content matches the query, folding
// content into scratch.
func (q grepQuery) matches(content []byte, scratch *[]byte) bool {
	if len(content) == 0 || bytes.IndexByte(content[:min(len(content), 8000)], 0) >= 0 {
		return false
	}
	if q.anyLine {
		return true
	}
	*scratch = foldCase((*scratch)[:0], content)
	for _, p := range q.patterns {
		if bytes.Contains(*scratch, p) {
			return true
		}
	}
	return false
}

// asciiFold maps each ASCII byte to the least rune of its case-folding
// orbit: a letter to its capital, which 'k', 'K' and the Kelvin sign share.
var asciiFold = func() (fold [utf8.RuneSelf]byte) {
	for i := range fold {
		fold[i] = byte(i)
		if 'a' <= i && i <= 'z' {
			fold[i] = byte(i - 'a' + 'A')
		}
	}
	return fold
}()

// foldCase appends src to dst with each rune replaced by the least rune of
// its simple case-folding orbit, so two texts are equal under folding exactly
// when their folds are equal, and each invalid byte by 0xFF, which no valid
// pattern holds.
func foldCase(dst, src []byte) []byte {
	dst = slices.Grow(dst, len(src))
	for i := 0; i < len(src); {
		if c := src[i]; c < utf8.RuneSelf {
			dst = append(dst, asciiFold[c])
			i++
			continue
		}
		r, size := utf8.DecodeRune(src[i:])
		if r == utf8.RuneError && size == 1 {
			dst = append(dst, 0xff)
		} else {
			dst = utf8.AppendRune(dst, foldRune(r))
		}
		i += size
	}
	return dst
}

func foldRune(r rune) rune {
	least := r
	for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
		least = min(least, f)
	}
	return least
}

func (n *dbNotebook) Snapshot(ctx context.Context, revision string) (Snapshot, error) {
	return n.snapshotCommitted(ctx, revision, nil)
}

func (n *dbNotebook) SnapshotScoped(ctx context.Context, revision string, scope ContentScope) (Snapshot, error) {
	if err := scope.Validate(); err != nil {
		return Snapshot{}, err
	}
	return n.snapshotCommitted(ctx, revision, &scope)
}

// snapshotCommitted captures authority and visibility under mu, then reads
// only immutable content, so saves proceed while it reads.
func (n *dbNotebook) snapshotCommitted(ctx context.Context, revision string, scope *ContentScope) (Snapshot, error) {
	n.mu.Lock()
	selection, err := n.snapshotSelection(ctx, revision)
	n.mu.Unlock()
	if err != nil {
		return Snapshot{}, err
	}
	result, err := n.readSnapshot(ctx, selection, scope)
	if err != nil {
		return Snapshot{}, err
	}
	// Only an older revision can hold a note at a path the head refuses.
	files := result.Files[:0]
	for _, f := range result.Files {
		if err := n.resolveAtHead(ctx, f.Path); err == nil {
			files = append(files, f)
		} else if !errors.Is(err, ErrForbidden) {
			return Snapshot{}, err
		}
	}
	result.Files = files
	if err := n.stillVisibleNow(selection.generation, selection.visibility); err != nil {
		return Snapshot{}, err
	}
	return result, nil
}

func (n *dbNotebook) ListScoped(ctx context.Context, scope ContentScope) (ScopedEntries, error) {
	return n.FindScoped(ctx, "", "", "", "", false, 0, scope)
}

// FindScoped filters committed input before the literal result limit. limit=0
// requests the complete bounded list, used by scoped browse/review consumers.
func (n *dbNotebook) FindScoped(ctx context.Context, query, tag, status, path string, includeTrash bool, limit int, scope ContentScope) (ScopedEntries, error) {
	if err := scope.Validate(); err != nil {
		return ScopedEntries{}, err
	}
	if len(query) > 200 || len(tag) > 200 || len(status) > 200 || len(path) > 512 || limit < 0 || limit > 100 {
		return ScopedEntries{}, invalidInput("search filters or result limit exceed bounds")
	}
	n.mu.Lock()
	selection, err := n.snapshotSelection(ctx, "")
	var trash TrashIndex
	if err == nil {
		trash, err = n.loadTrash(ctx)
	}
	n.mu.Unlock()
	if err != nil {
		return ScopedEntries{}, err
	}
	snapshot, err := n.readSnapshot(ctx, selection, &scope)
	if err != nil {
		return ScopedEntries{}, err
	}
	if err := n.stillVisibleNow(selection.generation, selection.visibility); err != nil {
		return ScopedEntries{}, err
	}
	result := ScopedEntries{Revision: snapshot.Revision, Visibility: snapshot.Visibility, ScopeID: snapshot.ScopeID, Entries: []Entry{}}
	originals := map[string]string{}
	for _, record := range trash.Records {
		originals[record.Path] = record.OriginalPath + "\n" + record.Title
	}
	for _, file := range snapshot.Files {
		if IsTrash(file.Path) && !includeTrash || !strings.HasPrefix(file.Path, path) || !strings.Contains(strings.ToLower(file.Path+"\n"+originals[file.Path]+"\n"+SearchableText(file.Content)), strings.ToLower(query)) {
			continue
		}
		if status != "" && !strings.EqualFold(status, file.Metadata.Status) {
			continue
		}
		found := tag == ""
		for _, v := range file.Metadata.Tags {
			found = found || strings.EqualFold(tag, v)
		}
		if !found {
			continue
		}
		title, excerpt := NoteSummary([]byte(file.Content))
		metadata := file.Metadata
		entry := Entry{Path: file.Path, Title: title, Excerpt: excerpt, Metadata: &metadata}
		if IsTrash(file.Path) {
			entry.TrashID = strings.TrimSuffix(strings.TrimPrefix(file.Path, "Trash/"), ".md")
		}
		result.Entries = append(result.Entries, entry)
	}
	sort.SliceStable(result.Entries, func(i, j int) bool {
		a, b := result.Entries[i], result.Entries[j]
		ta, tb := scope.Tier(*a.Metadata), scope.Tier(*b.Metadata)
		if ta != tb {
			return ta < tb
		}
		return a.Path < b.Path
	})
	if limit > 0 && len(result.Entries) > limit {
		result.Entries = result.Entries[:limit]
	}
	return result, nil
}

// BrowseScoped is used only by explicit retrieval consumers. Ordinary collection
// browsing remains independent of search preferences, including repairable notes.
func (n *dbNotebook) BrowseScoped(ctx context.Context, folder string, scope ContentScope) (FolderBrowse, error) {
	if err := scope.Validate(); err != nil {
		return FolderBrowse{}, err
	}
	folder = strings.TrimSuffix(folder, "/")
	if folder != "" {
		if err := n.resolveAtHead(ctx, folder); err != nil || path.Clean(folder) != folder {
			return FolderBrowse{}, ErrForbidden
		}
	}
	entries, err := n.ListScoped(ctx, scope)
	if err != nil {
		return FolderBrowse{}, err
	}
	return browseEntries(FolderBrowse{Path: folder, Collection: scope.Collection, Revision: entries.Revision, Visibility: entries.Visibility, ScopeID: entries.ScopeID, Items: []BrowseItem{}}, entries.Entries), nil
}

// changeSetPaths is how many files one change set lists; tests lower it.
var changeSetPaths = 10000

// ChangesSince lists what changed in the visible files from a checkpoint to
// revision, as Git's diff --name-status --find-renames lists it, with the
// newest commits that touched them; or every visible file for a fresh
// review when the checkpoint is no ancestor of revision. A file the store
// kept as one entry is renamed where Git's similarity would find a rename.
func (n *dbNotebook) ChangesSince(ctx context.Context, from, revision string) (ChangeSet, error) {
	n.mu.Lock()
	revision, err := n.snapshotRevision(ctx, revision)
	exclusions := slices.Clone(n.browseExcluded)
	generation := n.authorityGeneration.Load()
	n.mu.Unlock()
	if err != nil {
		return ChangeSet{}, err
	}
	result, err := n.changesSince(ctx, from, revision, exclusions)
	if err != nil {
		return result, err
	}
	if err := n.stillVisibleNow(generation, historyVisibilityID(exclusions)); err != nil {
		return ChangeSet{}, err
	}
	return result, nil
}

func (n *dbNotebook) changesSince(ctx context.Context, from, revision string, exclusions []string) (ChangeSet, error) {
	result := ChangeSet{From: from, Revision: revision, Files: []FileChange{}, Commits: []Commit{}}
	var unanswered error
	visible := func(p string) bool {
		if IsTrash(p) || matchesPrefix(p, exclusions) || strings.HasPrefix(p, "comments/") {
			return false
		}
		err := n.resolveAtHead(ctx, p)
		if err != nil && !errors.Is(err, ErrForbidden) {
			unanswered = errors.Join(unanswered, err)
		}
		return err == nil
	}
	if from == "" || !revisionPattern.MatchString(from) {
		result.FreshReview = true
		result.Reason = "No valid checkpoint; review the current snapshot."
	} else if ancestor, err := n.isAncestor(ctx, from, revision); err != nil {
		return result, err
	} else if !ancestor {
		result.FreshReview = true
		result.Reason = "Checkpoint is missing or is not an ancestor; a fresh review is required."
	}
	if result.FreshReview {
		files, err := n.files(ctx, revision)
		if err != nil {
			return result, err
		}
		for _, f := range files {
			if f.Mode == gitobj.ModeFile && visible(f.Path) {
				result.Files = append(result.Files, FileChange{Kind: "added", Path: f.Path})
			}
		}
		if unanswered != nil {
			return result, unanswered
		}
		if len(result.Files) > changeSetPaths {
			// The notebook itself has more files than a change set lists,
			// so a fresh review cannot be listed at any revision.
			return result, limitExceeded(fmt.Sprintf("change set exceeds %d paths", changeSetPaths))
		}
		return result, nil
	}
	older, err := parseRevision(from)
	if err != nil {
		return result, err
	}
	newer, err := parseRevision(revision)
	if err != nil {
		return result, err
	}
	changes, err := n.notes.Changes(ctx, older, newer)
	if err != nil {
		return result, err
	}
	for _, c := range gitDiff(changes) {
		if c.Kind == "renamed" {
			oldVisible, newVisible := visible(c.From), visible(c.Path)
			if oldVisible && newVisible {
				result.Files = append(result.Files, c)
			} else if oldVisible {
				result.Files = append(result.Files, FileChange{Kind: "deleted", Path: c.From})
			} else if newVisible {
				result.Files = append(result.Files, FileChange{Kind: "added", Path: c.Path})
			}
		} else if visible(c.Path) {
			result.Files = append(result.Files, c)
		}
	}
	if unanswered != nil {
		return result, unanswered
	}
	if len(result.Files) > changeSetPaths {
		return result, invalidInput(fmt.Sprintf("change set exceeds %d paths; use a fresh snapshot", changeSetPaths))
	}
	if from != revision && len(result.Files) > 0 {
		var paths []string
		for _, f := range result.Files {
			paths = append(paths, f.Path)
			if f.From != "" {
				paths = append(paths, f.From)
			}
		}
		if result.Commits, err = n.commitsTouching(ctx, older, newer, paths, 257); err != nil {
			return result, err
		}
	}
	if len(result.Commits) > 256 {
		result.Commits = result.Commits[:256]
		result.CommitsTruncated = true
	}
	return result, nil
}

// gitDiff restates the store's changes, one per entry, as Git's diff of the
// two trees reports them, one per path in path order: a path in both trees is
// modified, whichever entries it held; a path in one tree only is added or
// deleted, unless it is the destination of a rename from a path in only the
// older tree, which is reported in its place.
func gitDiff(changes []notestore.Change) []FileChange {
	older, newer := map[string]bool{}, map[string]bool{}
	renamed := map[string]string{} // destination to source
	for _, c := range changes {
		switch c.Op {
		case notestore.OpAdd:
			newer[c.Path] = true
		case notestore.OpDelete:
			older[c.Path] = true
		case notestore.OpModify:
			older[c.Path], newer[c.Path] = true, true
		case notestore.OpRename:
			older[c.OldPath], newer[c.Path] = true, true
			renamed[c.Path] = c.OldPath
		}
	}
	consumed := map[string]bool{}
	for to, from := range renamed {
		if older[to] || newer[from] {
			delete(renamed, to)
		} else {
			consumed[from] = true
		}
	}
	var paths []string
	for p := range older {
		paths = append(paths, p)
	}
	for p := range newer {
		if !older[p] {
			paths = append(paths, p)
		}
	}
	slices.Sort(paths)
	var out []FileChange
	for _, p := range paths {
		switch {
		case older[p] && newer[p]:
			out = append(out, FileChange{Kind: "modified", Path: p})
		case newer[p] && renamed[p] != "":
			out = append(out, FileChange{Kind: "renamed", From: renamed[p], Path: p})
		case newer[p]:
			out = append(out, FileChange{Kind: "added", Path: p})
		case !consumed[p]:
			out = append(out, FileChange{Kind: "deleted", Path: p})
		}
	}
	return out
}

// commitsTouching returns, newest first, up to limit revisions after older
// up to newer that changed a file at or under one of paths, as
// "git log -limit older..newer -- paths" lists them.
func (n *dbNotebook) commitsTouching(ctx context.Context, older, newer gitobj.ID, paths []string, limit int) ([]Commit, error) {
	low, err := n.revisionSeq(ctx, older)
	if err != nil {
		return nil, err
	}
	high, err := n.revisionSeq(ctx, newer)
	if err != nil {
		return nil, err
	}
	named := map[string]bool{}
	for _, p := range paths {
		named[p] = true
	}
	touches := func(p string) bool {
		for p != "" {
			if named[p] {
				return true
			}
			i := strings.LastIndexByte(p, '/')
			if i < 0 {
				break
			}
			p = p[:i]
		}
		return false
	}
	commits := []Commit{}
	for before := high + 1; before > low+1 && len(commits) < limit; {
		page, err := n.notes.History(ctx, notestore.HistoryQuery{Before: before, Limit: int(min(before-1-low, 512))})
		if err != nil {
			return nil, err
		}
		if len(page) == 0 {
			break
		}
		for _, info := range page {
			before = info.Seq
			if info.Seq <= low || len(commits) == limit {
				break
			}
			if slices.ContainsFunc(info.Changes, func(c notestore.Change) bool { return touches(c.Path) || c.OldPath != "" && touches(c.OldPath) }) {
				c := commitRecord(info)
				c.Message = "" // the change set's log format reads no subject
				commits = append(commits, c)
			}
		}
	}
	return commits, nil
}

// revisionSeq is the store's sequence number of one of the notebook's
// revisions.
func (n *dbNotebook) revisionSeq(ctx context.Context, rev gitobj.ID) (int64, error) {
	if h, err := n.notes.Head(ctx); err != nil {
		return 0, err
	} else if h.Commit == rev {
		return h.Seq, nil
	}
	info, err := n.notes.Commit(ctx, rev)
	return info.Seq, err
}

// AssertSnapshot checks ancestry without mistaking an unavailable commit for success.
func (n *dbNotebook) AssertSnapshot(ctx context.Context, revision string) error {
	n.mu.Lock()
	head, err := n.head(ctx)
	n.mu.Unlock()
	if err != nil {
		return err
	}
	if !revisionPattern.MatchString(revision) {
		return ErrConflict
	}
	ancestor, err := n.isAncestor(ctx, revision, head)
	if err != nil {
		return err
	}
	if !ancestor {
		return fmt.Errorf("snapshot is no longer in current history: %w", ErrConflict)
	}
	return nil
}

// KnowledgeRead reads a visible note at revision, or at the head, for
// derived knowledge.
func (n *dbNotebook) KnowledgeRead(ctx context.Context, path, revision string) (Document, error) {
	n.mu.Lock()
	if visible, err := n.visibleAtHead(ctx, path); err != nil || !visible {
		n.mu.Unlock()
		if err == nil {
			err = ErrForbidden
		}
		return Document{}, err
	}
	generation, visibility := n.authorityGeneration.Load(), historyVisibilityID(n.browseExcluded)
	if revision == "" {
		var err error
		if revision, err = n.head(ctx); err != nil {
			n.mu.Unlock()
			return Document{}, err
		}
	}
	n.mu.Unlock()
	doc, err := n.readRevision(ctx, path, revision)
	if err != nil {
		return Document{}, err
	}
	if err := n.stillVisibleNow(generation, visibility); err != nil {
		return Document{}, err
	}
	return doc, nil
}

func (n *dbNotebook) HistoryVisibilityID(ctx context.Context) (string, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return historyVisibilityID(n.browseExcluded), nil
}

// VisibleHistoryPaths reapplies present exclusions and path safety to
// derived caches, including after the operator changes exclusions on restart.
func (n *dbNotebook) VisibleHistoryPaths(ctx context.Context, paths []string) (map[string]bool, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	allowed := map[string]bool{}
	for _, path := range paths {
		visible, err := n.visibleAtHead(ctx, path)
		if err != nil {
			return nil, err
		}
		if visible {
			allowed[path] = true
		}
	}
	return allowed, nil
}
