package web

import (
	"errors"
	"io/fs"
	"path/filepath"

	"forgetmenote/internal/catalog"
)

// Integration configuration and credentials belong to the operator, outside
// every local notebook that users can edit or export.
func (s *Server) privateRuntimeFile(file string) error {
	if !filepath.IsAbs(file) {
		return errors.New("runtime files require absolute paths outside notebooks")
	}
	resolved, e := filepath.EvalSymlinks(file)
	if e != nil {
		return errors.New("runtime file unavailable")
	}
	for _, p := range s.catalog.List(catalog.Bootstrap) {
		store, e := s.catalog.Access(catalog.Bootstrap, p.ID, "read")
		if e != nil {
			return e
		}
		// A notebook kept in the notes store has no directory: its Root
		// need not exist, and no file lies inside one that does not. Where
		// it does, it is the previous release's working tree of the notebook.
		root, e := filepath.EvalSymlinks(store.Root)
		if errors.Is(e, fs.ErrNotExist) {
			continue
		}
		if e != nil {
			return e
		}
		rel, e := filepath.Rel(root, resolved)
		if e != nil || rel == "." || filepath.IsLocal(rel) {
			return errors.New("runtime credentials and configuration must stay outside notebook repositories")
		}
	}
	return nil
}
