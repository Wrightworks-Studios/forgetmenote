package workspace

import (
	"sort"
	"strings"
)

type HistoryRevision struct {
	Revision string `json:"revision"`
	Time     string `json:"time"`
}

func historyVisibilityID(exclusions []string) string {
	paths := append([]string(nil), exclusions...)
	sort.Strings(paths)
	return ContentHash([]byte(strings.Join(paths, "\x00")))
}

// HistoryNotes reads only changed regular Markdown postimages. Comparing merge
// commits with their first parent includes merge resolutions; HistoryRevisions
// separately visits side parents. Deletions have no postimage to cite.
type HistoryFile struct {
	SnapshotFile
	Blob string
}
type HistoryBatch struct {
	Visibility string
	Files      []HistoryFile
	Next       int
	Done       bool
}
