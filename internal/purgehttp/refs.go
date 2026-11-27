package purgehttp

import (
	"bytes"
	"context"
	"encoding/json"
	"regexp"

	"forgetmenote/internal/workspace"
)

// A purge gives every revision from the first it rewrote a new ID, and keeps
// the mapping. References to the old IDs, such as citations, history pins,
// memory anchors and an agent's remembered revisions, keep working: a read
// pinned to an old revision reads the revision that stands for it now, and
// one pinned to a version the purge removed is refused as purged.

var pinnedRevision = regexp.MustCompile(`^[0-9a-f]{40}$`)

// PinnedRead reads with revision and, when that fails because a purge
// rewrote revision, with the revision standing for it; ErrPurgedVersion when
// the purge removed path's version there.
func PinnedRead[T any](ctx context.Context, store *workspace.Store, path, revision string, read func(revision string) (T, error)) (T, error) {
	value, err := read(revision)
	if err == nil || store == nil || !pinnedRevision.MatchString(revision) {
		return value, err
	}
	now, purged, resolveErr := store.ResolvePurgedRevision(ctx, path, revision)
	switch {
	case resolveErr != nil || now == revision:
		return value, err
	case purged:
		var zero T
		return zero, ErrPurgedVersion
	}
	return read(now)
}

// The arguments of read-only tools that pin a revision.
var pinnedArguments = []string{"revision", "since", "sourceRevision", "baseRevision"}

// WithPurgedRevisions retries a read-only action whose pinned revisions a
// purge rewrote, with the revisions standing for them, so agents holding old
// revisions do not break; a pin to a version the purge removed is refused as
// purged. Writes never retry: a revision a write expects must be current.
func WithPurgedRevisions[I, O any](open func(ctx context.Context, project, role string) (*workspace.Store, error), fn func(context.Context, I) (O, error)) func(context.Context, I) (O, error) {
	return func(ctx context.Context, in I) (O, error) {
		out, err := fn(ctx, in)
		if err == nil {
			return out, nil
		}
		raw, marshalErr := json.Marshal(in)
		if marshalErr != nil {
			return out, err
		}
		d := json.NewDecoder(bytes.NewReader(raw))
		d.UseNumber()
		var fields map[string]any
		if d.Decode(&fields) != nil {
			return out, err
		}
		project, _ := fields["project"].(string)
		if project == "" {
			project, _ = fields["notebook"].(string)
		}
		if project == "" {
			return out, err
		}
		path, _ := fields["path"].(string)
		var store *workspace.Store
		changed := false
		for _, key := range pinnedArguments {
			revision, _ := fields[key].(string)
			if !pinnedRevision.MatchString(revision) {
				continue
			}
			if store == nil {
				var accessErr error
				if store, accessErr = open(ctx, project, "read"); accessErr != nil {
					return out, err
				}
			}
			pinned := path
			if key != "revision" {
				pinned = ""
			}
			now, purged, resolveErr := store.ResolvePurgedRevision(ctx, pinned, revision)
			if resolveErr != nil || now == revision {
				continue
			}
			if purged {
				var zero O
				return zero, ErrPurgedVersion
			}
			fields[key], changed = now, true
		}
		if !changed {
			return out, err
		}
		remapped, marshalErr := json.Marshal(fields)
		var next I
		if marshalErr != nil || json.Unmarshal(remapped, &next) != nil {
			return out, err
		}
		return fn(ctx, next)
	}
}
