package workspace

type listingQuery struct{ query, tag, status, pathPrefix string }

// A listing captures published immutable cache values and one commit. The maps
// are replaced, never modified; public entries clone nested metadata on return.
type listingSelection struct {
	revision, metadataRevision string
	generation                 uint64
	exclusions                 []string
	objects                    map[string]string
	metadata                   map[string]Entry
	paths                      []string
}
