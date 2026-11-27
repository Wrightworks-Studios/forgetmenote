package judgments

import (
	"errors"
	"forgetmenote/internal/auth"
	"forgetmenote/internal/decision"
	"os"
)

// NotebookName is the record name of a notebook's judgment settings.
func NotebookName(project string) string { return "notebooks/" + auth.Digest(project) + ".json" }

func validJudgmentNotebook(project string, settings judgmentNotebookSettings) bool {
	return settings.Version == 1 && settings.Notebook == project && !(settings.AutoChecks && (settings.EnabledBy == "" || settings.AccountID == "" || settings.Baseline == nil))
}

func (s *Service) loadJudgmentNotebook(project string) (judgmentNotebookSettings, error) {
	s.judgmentState.mu.Lock()
	defer s.judgmentState.mu.Unlock()
	var settings judgmentNotebookSettings
	err := s.judgmentState.read(NotebookName(project), &settings)
	if errors.Is(err, os.ErrNotExist) {
		return judgmentNotebookSettings{Version: 1, Notebook: project}, nil
	}
	if err != nil || !validJudgmentNotebook(project, settings) {
		return settings, decision.ErrStorage
	}
	return settings, nil
}

// updateNotebook changes a notebook's judgment settings while holding their record,
// starting from the defaults when it has none, so automatic checks and
// organization settings changed through another gateway are kept. Callers
// hold mu.
func (j *judgmentState) updateNotebook(project string, change func(settings *judgmentNotebookSettings, found bool) error) (judgmentNotebookSettings, error) {
	settings := judgmentNotebookSettings{Version: 1, Notebook: project}
	err := j.update(NotebookName(project), &settings, func(found bool) error {
		if found && !validJudgmentNotebook(project, settings) {
			return decision.ErrStorage
		}
		return change(&settings, found)
	})
	return settings, err
}
