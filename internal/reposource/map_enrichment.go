package reposource

import (
	"context"
	"errors"
)

type MapInput struct {
	Path      string   `json:"path"`
	InputHash string   `json:"inputHash"`
	Component string   `json:"component"`
	Symbols   []string `json:"symbols,omitempty"`
	Text      string   `json:"text"`
}
type MapClassification struct {
	Path  string
	Label MapLabel
}
type MapClassifyFunc func(context.Context, State, []MapInput) ([]MapClassification, error)

func (m *Manager) SetMapClassifier(model func() string, classify MapClassifyFunc) {
	m.mu.Lock()
	m.mapModel, m.classify = model, classify
	m.mu.Unlock()
}

func (m *Manager) EnrichMapPending(ctx context.Context, project string) (bool, error) {
	ctx, done, err := m.startProcessing(ctx, project)
	if err != nil {
		return false, err
	}
	defer done()
	m.mu.Lock()
	modelFunc, classify := m.mapModel, m.classify
	m.mu.Unlock()
	if modelFunc == nil || classify == nil {
		return false, nil
	}
	model := modelFunc()
	if model == "" {
		return false, nil
	}
	n, err := m.book(project)
	if err != nil {
		return false, err
	}
	var state State
	var inputs []MapInput
	err = m.read(ctx, project, "", func(c *cache, s State) error {
		state = s
		if s.Paused || !s.IndexConfig.MapJudgments || !s.IndexConfig.ExternalProcessing || s.Index.MapError != "" {
			return nil
		}
		if s.MapActor == "" || m.check(project, s.MapActor) != nil {
			return ErrDenied
		}
		m.indexMu.Lock()
		defer m.indexMu.Unlock()
		idx, err := m.lexicalIndex(ctx, c, s)
		if err != nil {
			return err
		}
		if idx.Commit != s.Downloaded || idx.ConfigID != s.indexID() {
			return ErrConflict
		}
		selected := map[string]int{}
		for _, entry := range idx.Map {
			if entry.Role != "source" || entry.Label != nil && entry.Label.Model == model {
				continue
			}
			selected[entry.Path] = len(inputs)
			inputs = append(inputs, MapInput{Path: entry.Path, Component: entry.Component, Symbols: entry.Symbols})
			if len(inputs) == 8 {
				break
			}
		}
		for _, ch := range idx.Chunks {
			if i, ok := selected[ch.Path]; ok && len(inputs[i].Text) < 1400 {
				inputs[i].Text += clipUTF8(ch.Text, 1400-len(inputs[i].Text))
			}
		}
		for _, entry := range idx.Map {
			if i, ok := selected[entry.Path]; ok {
				inputs[i].InputHash = mapFingerprint(entry, inputs[i].Text)
			}
		}
		return nil
	})
	if err != nil || len(inputs) == 0 {
		return false, err
	}
	labels, providerErr := classify(ctx, state, inputs)
	err = m.read(ctx, project, state.ID, func(c *cache, s State) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if s.Generation != state.Generation || s.Downloaded != state.Downloaded || s.Paused || !s.IndexConfig.MapJudgments || m.check(project, state.MapActor) != nil {
			return ErrConflict
		}
		if providerErr != nil {
			if errors.Is(providerErr, ErrDenied) {
				return providerErr
			}
			s.Index.MapError = "Map classification unavailable; local navigation remains ready. Retrying after a source change or settings save."
			if err := n.save(s); err != nil {
				return err
			}
			return providerErr
		}
		if len(labels) == 0 {
			return nil
		}
		selected := map[string]string{}
		for _, in := range inputs {
			selected[in.Path] = in.InputHash
		}
		accepted := map[string]MapLabel{}
		for _, result := range labels {
			label := result.Label
			if selected[result.Path] == "" || selected[result.Path] != label.InputHash || label.Model != model || label.Topic != "" && mapTopics[label.Topic] == "" || label.Probability < 0 || label.Probability > 1 || label.Unavailable && (label.Topic != "" || label.Probability != 0) {
				return ErrUnavailable
			}
			if _, duplicate := accepted[result.Path]; duplicate {
				return ErrUnavailable
			}
			accepted[result.Path] = label
		}
		m.indexMu.Lock()
		defer m.indexMu.Unlock()
		idx, err := m.lexicalIndex(ctx, c, s)
		if err != nil {
			return err
		}
		prior := idx
		idx.Map = append([]MapEntry(nil), idx.Map...)
		for i := range idx.Map {
			if label, ok := accepted[idx.Map[i].Path]; ok {
				idx.Map[i].Label = &label
			}
		}
		replaced := s.Index.Generation
		if err = c.publishIndex(ctx, &idx, prior, nil, s.IndexConfig); err != nil {
			return err
		}
		s.Index = indexStatus(c, idx, s.Index, s.IndexConfig)
		return n.saveIndexed(ctx, c, s, replaced)
	})
	return err == nil && len(labels) > 0, err
}
