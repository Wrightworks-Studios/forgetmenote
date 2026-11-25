package knowledge

import (
	"context"
	"forgetmenote/internal/workspace"
	"strings"
)

func trashID(path string) string {
	if workspace.IsTrash(path) {
		return strings.TrimSuffix(strings.TrimPrefix(path, "Trash/"), ".md")
	}
	return ""
}

// Filter occurrences before ranking, snippets, facets and provenance. In
// particular, a new note reusing a deleted path does not inherit its tombstone.
func (e *Engine) searchableHistory(ctx context.Context, include bool, scope *workspace.ContentScope) (map[string]historyVersion, map[string]string, error) {
	policy, err := e.Store.TrashPolicy(ctx)
	if err != nil {
		return nil, nil, err
	}
	type state struct {
		id      string
		removed bool
	}
	occurrences := map[string]state{}
	paths := map[string]state{}
	for _, record := range policy.Records {
		v := state{id: record.ID, removed: record.Emptied || record.Restored || record.Hidden}
		paths[record.Path] = v
		if record.Restored {
			continue
		}
		for path, revisions := range record.History {
			for _, rev := range revisions {
				occurrences[path+"\x00"+rev] = v
			}
		}
	}
	versions := map[string]historyVersion{}
	ids := map[string]string{}
	for key, version := range e.history.Versions {
		if scope != nil && (!scope.AllowsPath(version.Path) || scope.Tier(e.history.Blobs[version.Blob].Metadata) < 0) {
			continue
		}
		kept := version
		kept.Revisions = nil
		for _, rev := range version.Revisions {
			v, trashed := paths[version.Path]
			if !trashed {
				v, trashed = occurrences[version.Path+"\x00"+rev.Revision]
			}
			if !trashed && workspace.IsTrash(version.Path) {
				continue
			}
			if trashed && (!include || v.removed) {
				continue
			}
			if len(kept.Revisions) == 0 && trashed {
				ids[key] = v.id
			}
			kept.Revisions = append(kept.Revisions, rev)
		}
		if len(kept.Revisions) > 0 {
			versions[key] = kept
		}
	}
	return versions, ids, nil
}
