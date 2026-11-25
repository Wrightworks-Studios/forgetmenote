package workspace

import (
	"forgetmenote/internal/capacity"
	"forgetmenote/internal/gitexport"
	"strings"
)

func MaxBundleBytes() int64 { return int64(capacity.BundleBytes.Limit()) }

// The import refusals are internal/gitexport's, which imports bundles into
// notebooks kept in the notes store, so callers test one value for both.
var ErrBundle = gitexport.ErrBundle
var ErrBundleTooLarge = gitexport.ErrBundleTooLarge

// notebookFile is the file allow-list shared by handoff validation and restore:
// notes, assets, discussion files and the root lock and Trash indexes, with
// the Trash's archives. Those indexes are the only hidden components;
// .gitattributes, hidden directories and other repository configuration never
// enter a notebook. Content checks stay with each caller, since a restore
// admits legacy notes that a handoff rejects.
func notebookFile(p string) bool {
	switch {
	case p == locksPath || trashFile(p):
		return true
	case strings.HasPrefix(p, "comments/"):
		name := strings.TrimSuffix(strings.TrimPrefix(p, "comments/"), ".json")
		return len(name) == 64 && strings.HasSuffix(p, ".json") && strings.Trim(name, "0123456789abcdef") == ""
	case strings.HasPrefix(p, "assets/"), strings.HasSuffix(p, ".md"):
		for _, part := range strings.Split(p, "/") {
			if strings.HasPrefix(part, ".") {
				return false
			}
		}
		return true
	}
	return false
}
