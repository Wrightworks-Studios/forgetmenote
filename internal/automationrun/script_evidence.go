package automationrun

import (
	"encoding/json"
	"errors"
	"os"

	"forgetmenote/internal/automation"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/knowledge"
	"forgetmenote/internal/notebooks"
	"forgetmenote/internal/reposource"
	"forgetmenote/internal/workspace"
)

// Source snapshots have a separate lifetime from the installed package commit.
// They are frozen once per run, and checked again before every host operation.
func (s *Service) captureScriptSources(e *scriptExecution) error {
	if e.run.Script.Sources != nil {
		return nil
	}
	sources := map[string]automation.ScriptSource{}
	for alias, project := range e.run.Spec.Script.Notebooks {
		if s.hooks.Repositories() == nil {
			continue
		}
		state, err := s.hooks.Repositories().Status(project)
		if err != nil {
			return err
		}
		if !state.Connected {
			continue
		}
		if state.Paused {
			return reposource.ErrUnavailable
		}
		snapshot := automation.ScriptSource{Project: project, Source: state.ID, Revision: state.Downloaded, Policy: automation.Hash(state.IndexConfig)}
		if e.run.Batch != nil {
			for _, event := range e.run.Batch.Events {
				if event.Project == project {
					if event.Revision != state.Downloaded {
						return workspace.ErrConflict
					}
					snapshot.Revision, snapshot.PreviousRevision = event.Revision, event.PreviousRevision
				}
			}
		}
		sources[alias] = snapshot
	}
	e.run.Script.Sources = sources
	return e.save()
}

func (s *Service) checkScriptSources(e *scriptExecution) error {
	for alias, source := range e.run.Script.Sources {
		if e.run.Spec.Script.Notebooks[alias] != source.Project || s.hooks.Repositories() == nil {
			return workspace.ErrForbidden
		}
		state, err := s.hooks.Repositories().Status(source.Project)
		if err != nil {
			return err
		}
		if !state.Connected || state.Paused || state.ID != source.Source || state.Downloaded != source.Revision || automation.Hash(state.IndexConfig) != source.Policy {
			return workspace.ErrConflict
		}
		if e.run.Spec.UsesAI() && !state.IndexConfig.ExternalProcessing {
			return workspace.ErrForbidden
		}
	}
	return nil
}

func (s *Service) scriptReadDocument(e *scriptExecution, in scriptRequest) (any, error) {
	var v struct {
		Notebook, Path, Revision, Origin string
		Optional                         bool
		reposource.LineRange
	}
	if err := json.Unmarshal(in.Arguments, &v); err != nil {
		return nil, err
	}
	if v.Origin == "" {
		v.Origin = "notes"
	}
	ref := notebooks.DocumentRef{Project: e.run.Spec.Script.Notebooks[v.Notebook], Origin: v.Origin, Path: v.Path, Revision: v.Revision}
	if v.Optional && v.Origin == "notes" {
		store, err := s.hooks.ClientAccess(e.ctx, e.run.Spec.Actor, ref.Project, "read")
		if err != nil {
			return nil, err
		}
		if _, err = store.Read(e.ctx, v.Path, v.Revision); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return map[string]any{"missing": true, "path": v.Path, "revision": "", "hash": ""}, nil
			}
			return nil, err
		}
	}
	if v.Origin == "repository" {
		source, ok := e.run.Script.Sources[v.Notebook]
		if !ok || v.Revision != "" && v.Revision != source.Revision {
			return nil, workspace.ErrForbidden
		}
		ref.Source, ref.Revision = source.Source, source.Revision
	}
	doc, err := s.hooks.ReadDocument(e.ctx, ref, v.LineRange)
	if err != nil {
		if v.Optional && errors.Is(err, os.ErrNotExist) {
			return map[string]any{"missing": true, "path": v.Path, "revision": "", "hash": ""}, nil
		}
		return nil, err
	}
	raw, _ := json.Marshal(doc)
	citations, _ := s.chatrun.ToolEvidence(e.ctx, chat.RunAuth{User: e.run.Spec.Actor, Project: e.run.Spec.Project}, in.Step, "read_document", raw)
	for _, citation := range citations {
		citation.Text = ""
		e.run.Citations = append(e.run.Citations, citation)
	}
	doc.HTML = ""
	return doc, nil
}

func (s *Service) scriptRepositoryNavigation(e *scriptExecution, in scriptRequest) (any, error) {
	var v struct {
		Notebook string `json:"notebook"`
		knowledge.MapQuery
	}
	if err := json.Unmarshal(in.Arguments, &v); err != nil {
		return nil, err
	}
	source, ok := e.run.Script.Sources[v.Notebook]
	if !ok || v.Revision != "" && v.Revision != source.Revision {
		return nil, workspace.ErrForbidden
	}
	if _, err := s.chatrun.DocumentScope(e.ctx, source.Project, "repository", source.Source, source.Revision, ""); err != nil {
		return nil, err
	}
	if in.Operation == "repositories.changes" {
		return s.hooks.Repositories().IndexedChanges(e.ctx, source.Project, source.Source, source.PreviousRevision, source.Revision)
	}
	v.Revision = source.Revision
	return s.hooks.NotebookMap(e.ctx, source.Project, "repository", source.Source, v.MapQuery)
}
