package reposource

import (
	"context"
)

// EmbedFunc is the existing operator transport boundary. A background batch
// yields to foreground requests at the endpoint's shared concurrency gate.
type EmbedFunc func(context.Context, IndexConfig, []string, bool) ([][]float32, error)

func (m *Manager) SetEmbedder(embed EmbedFunc) { m.mu.Lock(); m.embed = embed; m.mu.Unlock() }

// Embedding and map enrichment share one per-notebook processing lifetime.
func (m *Manager) startProcessing(ctx context.Context, project string) (context.Context, func(), error) {
	select {
	case m.work <- struct{}{}:
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
	ctx, cancel := context.WithCancel(ctx)
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		cancel()
		<-m.work
		return nil, nil, ErrUnavailable
	}
	if _, ok := m.processingJobs[project]; ok {
		m.mu.Unlock()
		cancel()
		<-m.work
		return nil, nil, ErrBusy
	}
	m.processingJobs[project] = cancel
	m.wg.Add(1)
	m.mu.Unlock()
	return ctx, func() {
		cancel()
		m.mu.Lock()
		delete(m.processingJobs, project)
		m.mu.Unlock()
		m.wg.Done()
		<-m.work
	}, nil
}
func (m *Manager) EmbedPending(ctx context.Context, project string) (bool, error) {
	ctx, done, err := m.startProcessing(ctx, project)
	if err != nil {
		return false, err
	}
	defer done()
	n, err := m.book(project)
	if err != nil {
		return false, err
	}

	var state State
	var selected []SourceChunk
	var prior sourceIndex
	err = m.read(ctx, project, "", func(c *cache, s State) error {
		state = s
		if s.Paused || !s.IndexConfig.Embeddings || !s.IndexConfig.ExternalProcessing || s.Index.EmbeddingError != "" {
			return nil
		}
		var e error
		m.indexMu.Lock()
		prior, e = m.lexicalIndex(ctx, c, s)
		m.indexMu.Unlock()
		if e != nil {
			return e
		}
		if prior.Commit != s.Downloaded || prior.ConfigID != s.indexID() {
			return ErrConflict
		}
		vm, f, _ := vectors(prior, s.IndexConfig)
		if f != nil {
			f.Close()
		}
		seen := map[string]bool{}
		for _, ch := range prior.Chunks {
			if _, ok := vm.Rows[ch.EmbeddingHash]; !ok && !seen[ch.EmbeddingHash] {
				seen[ch.EmbeddingHash] = true
				if len(selected) < 32 {
					selected = append(selected, ch)
				}
			}
		}
		bytes, e := diskBytes(prior.dir)
		if e != nil {
			return e
		}
		// Reject an unaffordable final snapshot before buying its first batch.
		if bytes+int64(len(seen))*(int64(s.IndexConfig.Dimensions)*4+100) > int64(maxIndexBytes()) {
			s.Index.EmbeddingError = "Embedding vectors exceed FMN_LIMIT_INDEX_BYTES; raise the operator budget or reduce the included paths."
			if e := n.save(s); e != nil {
				return e
			}
			return ErrLimit
		}
		return nil
	})
	if err != nil || len(selected) == 0 {
		return false, err
	}
	m.mu.Lock()
	embed := m.embed
	m.mu.Unlock()
	if embed == nil {
		return false, nil
	}
	texts := make([]string, len(selected))
	bytes := 0
	for i, ch := range selected {
		texts[i] = ch.payload()
		bytes += len(texts[i])
	}
	result, providerErr := embed(ctx, state.IndexConfig, texts, true)
	err = m.read(ctx, project, state.ID, func(c *cache, s State) error {
		if s.Generation != state.Generation || s.Downloaded != state.Downloaded || s.Paused {
			return ErrConflict
		}
		s.Index.Requests++
		s.Index.InputBytes += int64(bytes)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		failed := func(err error) error {
			s.Index.EmbeddingError = "Embedding batch could not be saved; keyword search is available. Check provider/storage settings and save source settings to retry."
			if e := n.save(s); e != nil {
				return e
			}
			return err
		}
		if providerErr != nil {
			return failed(providerErr)
		}
		if len(result) != len(selected) {
			return failed(ErrUnavailable)
		}
		m.indexMu.Lock()
		defer m.indexMu.Unlock()
		current, e := m.lexicalIndex(ctx, c, s)
		if e != nil {
			return failed(e)
		}
		updates := map[string][]float32{}
		for i, ch := range selected {
			updates[ch.EmbeddingHash] = result[i]
		}
		replaced := s.Index.Generation
		if e = c.publishIndex(ctx, &current, current, updates, s.IndexConfig); e != nil {
			return failed(e)
		}
		s.Index = indexStatus(c, current, s.Index, s.IndexConfig)
		s.Index.EmbeddedInputs += int64(len(selected))
		return n.saveIndexed(ctx, c, s, replaced)
	})
	return err == nil, err
}
