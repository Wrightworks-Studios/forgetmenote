package jev

import (
	"errors"
	"forgetmenote/internal/auth"
	"forgetmenote/internal/decision"
	"os"
)

// NotebookName is the record name of a notebook's Jev settings.
func NotebookName(project string) string { return "notebooks/" + auth.Digest(project) + ".json" }

func validJevNotebook(project string, settings jevNotebookSettings) bool {
	return settings.Version == 1 && settings.Notebook == project && !(settings.AutoChecks && (settings.EnabledBy == "" || settings.AccountID == "" || settings.Baseline == nil))
}

func (s *Service) loadJevNotebook(project string) (jevNotebookSettings, error) {
	s.jevState.mu.Lock()
	defer s.jevState.mu.Unlock()
	var settings jevNotebookSettings
	err := s.jevState.read(NotebookName(project), &settings)
	if errors.Is(err, os.ErrNotExist) {
		return jevNotebookSettings{Version: 1, Notebook: project}, nil
	}
	if err != nil || !validJevNotebook(project, settings) {
		return settings, decision.ErrStorage
	}
	return settings, nil
}

// updateNotebook changes a notebook's Jev settings while holding their record,
// starting from the defaults when it has none, so automatic checks and
// organization settings changed through another gateway are kept. Callers
// hold mu.
func (j *jevState) updateNotebook(project string, change func(settings *jevNotebookSettings, found bool) error) (jevNotebookSettings, error) {
	settings := jevNotebookSettings{Version: 1, Notebook: project}
	err := j.update(NotebookName(project), &settings, func(found bool) error {
		if found && !validJevNotebook(project, settings) {
			return decision.ErrStorage
		}
		return change(&settings, found)
	})
	return settings, err
}
