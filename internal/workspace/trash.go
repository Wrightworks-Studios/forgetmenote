package workspace

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

const trashIndexPath = ".gitnotes-trash.json"

// A Trash index or archive file read holds at most trashFileRecords records
// in trashFileBytes bytes.
const trashFileRecords, trashFileBytes = 10000, 8 << 20

// trashLimits bound what the Trash keeps; tests lower them. A Trash index
// file written holds at most records records in bytes bytes, the bounds of
// a file read, and one record keeps a note's history of at most revisions
// revisions under at most names names, the bounds of the Git backend's walk
// of at most 100,000 revisions.
var trashLimits = struct{ records, bytes, revisions, names int }{trashFileRecords, trashFileBytes, 100000, 1000}

// A Trash archive holds tombstones, the records of notes restored or
// emptied, which the index no longer has room for, in the index's format.
// Archives are numbered from 1 at the notebook's root, written once each
// and changed only by a purge. The records stay for those who read them:
// history search hides the old versions of emptied notes by their records'
// histories (knowledge, through TrashPolicy), and the Trash walk continues
// a restored note's history from its record when the note is trashed
// again (trashHistoryAt).
var trashArchivePattern = regexp.MustCompile(`^\.gitnotes-trash-[1-9][0-9]*\.json$`)

// trashArchivePath is the path of Trash archive number k.
func trashArchivePath(k int) string { return ".gitnotes-trash-" + strconv.Itoa(k) + ".json" }

// trashFile reports whether p is the Trash index or one of its archives.
func trashFile(p string) bool { return p == trashIndexPath || trashArchivePattern.MatchString(p) }

// parseTrashIndex parses the Trash index as loadTrash reads it: its format
// within its bounds. Its records are validateTrash's to check.
func parseTrashIndex(raw []byte) (TrashIndex, error) {
	var index TrashIndex
	if len(raw) > trashFileBytes || json.Unmarshal(raw, &index) != nil || index.Version != 1 || index.Records == nil || len(index.Records) > trashFileRecords {
		return TrashIndex{}, errors.New("invalid Trash index")
	}
	return index, nil
}

// parseTrashArchive parses a Trash archive: the index's format within its
// bounds, holding tombstones only. Its records' paths are validateTrash's
// to check.
func parseTrashArchive(raw []byte) (TrashIndex, error) {
	var archive TrashIndex
	if len(raw) > trashFileBytes || json.Unmarshal(raw, &archive) != nil || archive.Version != 1 || archive.Records == nil || len(archive.Records) > trashFileRecords {
		return TrashIndex{}, invalidInput("invalid Trash archive")
	}
	for _, r := range archive.Records {
		if !r.Emptied && !r.Restored {
			return TrashIndex{}, invalidInput("invalid Trash archive")
		}
	}
	return archive, nil
}

// distinctTrashRecords refuses a record ID that repeats across the Trash
// index and its archives; a compaction moves each record to one archive in
// the commit that drops it from the index.
func distinctTrashRecords(files ...[]TrashRecord) error {
	seen := map[string]bool{}
	for _, records := range files {
		for _, r := range records {
			if seen[r.ID] {
				return invalidInput("invalid Trash archive")
			}
			seen[r.ID] = true
		}
	}
	return nil
}

func IsTrash(path string) bool { return path == "Trash" || strings.HasPrefix(path, "Trash/") }

type TrashEntry struct {
	ID           string `json:"id"`
	Path         string `json:"path"`
	OriginalPath string `json:"originalPath"`
	Title        string `json:"title"`
	DeletedAt    string `json:"deletedAt"`
	DeletedBy    string `json:"deletedBy"`
}
type TrashRecord struct {
	Hidden bool `json:"hidden,omitempty"`
	TrashEntry
	History  map[string][]string `json:"history"`
	Emptied  bool                `json:"emptied,omitempty"`
	Restored bool                `json:"restored,omitempty"`
}
type TrashIndex struct {
	Version int           `json:"version"`
	Records []TrashRecord `json:"records"`
	// archives is how many archives the notebook had where the index was
	// read, so a compaction numbers its own after them.
	archives int
}
type TrashListing struct {
	Revision     string       `json:"revision"`
	Entries      []TrashEntry `json:"entries"`
	Confirmation string       `json:"confirmation"`
}
type TrashResult struct {
	Revision string       `json:"revision"`
	Entries  []TrashEntry `json:"entries"`
}

func cloneTrashIndex(in TrashIndex) TrashIndex {
	in.Records = slices.Clone(in.Records)
	for i := range in.Records {
		in.Records[i].History = maps.Clone(in.Records[i].History)
		for path, revisions := range in.Records[i].History {
			in.Records[i].History[path] = slices.Clone(revisions)
		}
	}
	return in
}
func (s *Store) validateTrash(index TrashIndex) error {
	seen := map[string]bool{}
	for _, r := range index.Records {
		id, e := hex.DecodeString(r.ID)
		if e != nil || len(id) != 16 || seen[r.ID] || r.Path != "Trash/"+r.ID+".md" || IsTrash(r.OriginalPath) || r.OriginalPath == "AGENTS.md" || !strings.HasSuffix(r.OriginalPath, ".md") {
			return invalidInput("invalid Trash entry")
		}
		if _, e = s.resolve(r.OriginalPath, false); e != nil {
			return e
		}
		if _, e = s.resolve(r.Path, false); e != nil {
			return e
		}
		if r.History == nil {
			return invalidInput("invalid Trash history")
		}
		for path, revs := range r.History {
			if _, e = s.resolve(path, false); e != nil {
				return e
			}
			for _, rev := range revs {
				if !revisionPattern.MatchString(rev) {
					return invalidInput("invalid Trash revision")
				}
			}
		}
		seen[r.ID] = true
	}
	return nil
}

// stageTrashIndex stages index as the Trash index, for a mutation that
// appended its last added records: callers append the records they add
// after those they read (TrashNotes, ApplyGroup and Organize do), so a full
// index can tell a request too large for an empty Trash from a Trash full
// of notes. An index past trashLimits first moves its tombstones to new
// archives, so the records of restored and emptied notes never fill it:
// only notes in the Trash can, until it is emptied or they are restored.
func stageTrashIndex(index TrashIndex, added int, changes map[string][]byte) error {
	raw, err := encodeTrashIndex(index)
	if err != nil {
		return err
	}
	if fitsTrashLimits(raw, len(index.Records)) {
		changes[trashIndexPath] = raw
		return nil
	}
	live := TrashIndex{Version: index.Version, Records: []TrashRecord{}}
	var tombstones []TrashRecord
	for _, r := range index.Records {
		if r.Emptied || r.Restored {
			tombstones = append(tombstones, r)
		} else {
			live.Records = append(live.Records, r)
		}
	}
	if raw, err = encodeTrashIndex(live); err != nil {
		return err
	}
	if !fitsTrashLimits(raw, len(live.Records)) {
		fresh := TrashIndex{Version: index.Version, Records: live.Records[len(live.Records)-min(added, len(live.Records)):]}
		if alone, err := encodeTrashIndex(fresh); err != nil || !fitsTrashLimits(alone, len(fresh.Records)) {
			return invalidInput("Trash transaction exceeds the Trash index; move fewer notes at a time")
		}
		return capacityReached("Trash index capacity reached; empty the Trash or restore notes first")
	}
	// The tombstones go to archives oldest first, each taking all that are
	// left or, past trashLimits, half as many until they fit.
	k := index.archives
	for len(tombstones) > 0 {
		for n := len(tombstones); ; n /= 2 {
			archive, err := encodeTrashIndex(TrashIndex{Version: index.Version, Records: tombstones[:n]})
			if err != nil {
				return err
			}
			if fitsTrashLimits(archive, n) {
				k++
				changes[trashArchivePath(k)] = archive
				tombstones = tombstones[n:]
				break
			}
			if n == 1 {
				return limitExceeded("a Trash record exceeds the Trash index capacity")
			}
		}
	}
	changes[trashIndexPath] = raw
	return nil
}

// encodeTrashIndex is index as a Trash index or archive file.
func encodeTrashIndex(index TrashIndex) ([]byte, error) {
	raw, err := json.MarshalIndent(index, "", "  ")
	return append(raw, '\n'), err
}

// fitsTrashLimits reports whether a Trash index file of raw, with records
// records, is within trashLimits.
func fitsTrashLimits(raw []byte, records int) bool {
	return len(raw)-1 <= trashLimits.bytes && records <= trashLimits.records
}

type trashPreparation struct {
	contents  map[string][]byte
	histories map[string]map[string][]string
}
