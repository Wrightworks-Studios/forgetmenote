package knowledge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"forgetmenote/internal/outgoing"
	"forgetmenote/internal/retrieval"
	"forgetmenote/internal/workspace"
)

// Endpoint is operator-configured; its URL and secret never enter project data.
type Endpoint struct {
	// Authorize is installed before Start and rechecks instance policy around
	// each external request, including work queued behind another request.
	Authorize func(string, int) error
	// Outgoing is installed before Start. While paused, document embedding,
	// which is paid background work, is refused; search queries continue.
	Outgoing    *outgoing.Mode
	url, secret string
	openAI      bool
	client      *http.Client
	gate        chan struct{}
	foreground  atomic.Int64
}

type queryCacheKey struct{}
type queryEmbeddingKey struct {
	Endpoint   *Endpoint
	Model      string
	Dimensions int
	Text       string
}
type queryEmbeddingCache struct {
	mu     sync.Mutex
	values map[queryEmbeddingKey][][]float32
}

// Query reuse lasts for one already-authorized retrieval, never across users.
func WithQueryCache(ctx context.Context) context.Context {
	if ctx.Value(queryCacheKey{}) != nil {
		return ctx
	}
	return context.WithValue(ctx, queryCacheKey{}, &queryEmbeddingCache{values: map[queryEmbeddingKey][][]float32{}})
}
func cloneVectors(v [][]float32) [][]float32 {
	out := make([][]float32, len(v))
	for i := range v {
		out[i] = append([]float32(nil), v[i]...)
	}
	return out
}

func NewEndpoint(address, secret string) (*Endpoint, error) {
	u, err := url.Parse(address)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (!strings.HasSuffix(u.Path, "/api/embed") && !strings.HasSuffix(u.Path, "/v1/embeddings")) || strings.ContainsAny(secret, "\r\n") {
		return nil, errors.New("EMBEDDING_URL must be an HTTP(S) /api/embed or /v1/embeddings endpoint without URL credentials or query parameters")
	}
	return &Endpoint{url: address, secret: secret, openAI: strings.HasSuffix(u.Path, "/v1/embeddings"), gate: make(chan struct{}, 1), client: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return errors.New("embedding endpoint redirects are not allowed")
	}}}, nil
}
func (e *Endpoint) Embed(ctx context.Context, config Config, texts []string) ([][]float32, error) {
	return e.EmbedModel(ctx, config.Model, config.Dimensions, texts, false)
}

// Background requests embed documents; foreground requests embed search queries.
func (e *Endpoint) EmbedModel(ctx context.Context, model string, dimensions int, texts []string, background bool) (vectors [][]float32, resultErr error) {
	if background {
		if err := e.Outgoing.Check(); err != nil {
			return nil, err
		}
	}
	if e.Authorize != nil {
		if err := e.Authorize(model, dimensions); err != nil {
			return nil, err
		}
		defer func() {
			if err := e.Authorize(model, dimensions); err != nil {
				vectors, resultErr = nil, err
			}
		}()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if cache, ok := ctx.Value(queryCacheKey{}).(*queryEmbeddingCache); ok && !background && len(texts) == 1 {
		key := queryEmbeddingKey{e, model, dimensions, texts[0]}
		cache.mu.Lock()
		defer cache.mu.Unlock()
		if value, exists := cache.values[key]; exists {
			return cloneVectors(value), nil
		}
		defer func() {
			if resultErr == nil && len(cache.values) < 10 {
				cache.values[key] = cloneVectors(vectors)
			}
		}()
	}

	if len(texts) < 1 || len(texts) > 64 {
		return nil, errors.New("embedding batch must contain 1–64 inputs")
	}
	size := 0
	inputs := make([]string, len(texts))
	for i, text := range texts {
		inputs[i] = retrieval.EmbeddingInput(model, text, background)
		size += len(inputs[i])
	}
	if size > 1<<20 {
		return nil, errors.New("embedding input batch exceeds 1 MiB")
	}
	if !background {
		e.foreground.Add(1)
		defer e.foreground.Add(-1)
	}
	for background && e.foreground.Load() > 0 {
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}

	select {
	case e.gate <- struct{}{}:
		defer func() { <-e.gate }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	payload := map[string]any{"model": model, "input": inputs}
	if e.Authorize != nil {
		if err := e.Authorize(model, dimensions); err != nil {
			return nil, err
		}
	}
	if e.openAI {
		payload["encoding_format"] = "float"
		if strings.HasPrefix(model, "text-embedding-3-") {
			payload["dimensions"] = dimensions
		}
	} else {
		payload["truncate"] = false
	}
	raw, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.url, bytes.NewReader(raw))
	if err != nil {
		return nil, errors.New("embedding request configuration is invalid")
	}
	req.Header.Set("Content-Type", "application/json")
	if e.secret != "" {
		req.Header.Set("Authorization", "Bearer "+e.secret)
	}
	response, err := e.client.Do(req)
	if err != nil {
		return nil, errors.New("embedding service is unreachable; ask the operator to check connectivity and the configured model")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return nil, fmt.Errorf("embedding service returned HTTP %d; ask the operator to check the model and credentials", response.StatusCode)
	}
	raw, err = io.ReadAll(io.LimitReader(response.Body, (4<<20)+1))
	if err != nil || len(raw) > 4<<20 {
		return nil, errors.New("embedding response is unreadable or exceeds 4 MiB")
	}
	var result struct {
		Model      string      `json:"model"`
		Embeddings [][]float32 `json:"embeddings"`
		Data       []struct {
			Index     *int      `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &result) != nil || result.Model != "" && result.Model != model || e.openAI && result.Model == "" {
		return nil, errors.New("embedding service returned a partial batch or incompatible model")
	}
	vectors = result.Embeddings
	if e.openAI {
		if len(result.Data) != len(texts) {
			return nil, errors.New("embedding service returned a partial batch")
		}
		vectors = make([][]float32, len(texts))
		seen := make([]bool, len(texts))
		// OpenAI identifies each input by index; response order is not provenance.
		for _, item := range result.Data {
			if item.Index == nil || *item.Index < 0 || *item.Index >= len(texts) || seen[*item.Index] {
				return nil, errors.New("embedding service returned invalid input indices")
			}
			seen[*item.Index] = true
			vectors[*item.Index] = item.Embedding
		}
	}
	if len(vectors) != len(texts) {
		return nil, errors.New("embedding service returned a partial batch")
	}
	for i, v := range vectors {
		normalized, err := normalize(v, dimensions)
		if err != nil {
			return nil, err
		}
		vectors[i] = normalized
	}
	return vectors, nil
}
func (e *Engine) Start(provider *Endpoint) error {
	e.lifecycle.Lock()
	defer e.lifecycle.Unlock()
	if e.closed {
		return errors.New("knowledge engine is closed")
	}
	// Starting or updating the worker never waits on its owner's network calls.
	// Cached history is loaded only by syncHistory after visibility validation.
	e.provider.Store(provider)
	if e.started {
		e.signal()
		return nil
	}
	hints, cancel, err := e.Store.Watch()
	if err != nil {
		return err
	}
	e.started = true
	workerCtx, workerCancel := context.WithCancel(context.Background())
	go func() {
		select {
		case <-e.stop:
			workerCancel()
		case <-workerCtx.Done():
		}
	}()
	go func() {
		defer close(e.done)
		defer workerCancel()
		defer cancel()
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-e.stop:
				return
			case _, ok := <-hints:
				if !ok {
					return
				}
			case <-e.wake:
			case <-ticker.C:
			}
			reset := true
			for e.ProcessBatch(workerCtx, reset) {
				reset = false
			}
		}
	}()
	e.signal()
	return nil
}
func (e *Engine) Close() {
	e.lifecycle.Lock()
	started := e.started
	if !e.closed {
		e.closed = true
		close(e.stop)
	}
	e.lifecycle.Unlock()
	if started {
		<-e.done
	}
}

// SetEndpoint changes only the configured boundary; it starts no goroutines.
func (e *Engine) SetEndpoint(provider *Endpoint) error {
	e.lifecycle.Lock()
	defer e.lifecycle.Unlock()
	if e.closed {
		return errors.New("knowledge engine is closed")
	}
	e.provider.Store(provider)
	return nil
}

// ProcessBatch performs one bounded work quantum. Standalone engines and the
// notebook registry share this logic; a registry can yield fairly between
// notebooks without retaining a worker and corpus for every idle notebook.
func (e *Engine) ProcessBatch(workerCtx context.Context, reset bool) bool {
	e.batchWork.Lock()
	defer e.batchWork.Unlock()
	if workerCtx.Err() != nil {
		return false
	}
	e.mu.Lock()
	if reset {
		e.embeddingFailed = false
	}
	if e.batchLimit == 0 {
		e.batchLimit = 32
	}
	batchLimit, embeddingFailed, deadline := e.batchLimit, e.embeddingFailed, e.batchDeadline
	c, p, history := e.state.Config, e.provider.Load(), e.historyLoaded || e.historyResume
	recovery := c.Enabled && e.currentVectorPersistFailure != ""
	e.mu.Unlock()
	available := c.Enabled && c.Endpoint && p != nil && !embeddingFailed
	if !available && !history && !recovery {
		return false
	}
	ctx, done := context.WithTimeout(workerCtx, deadline)
	// A gateway embedding this notebook's batch finishes it alone; this one
	// adopts those vectors on a later pass instead of paying for them again.
	release, ok, err := e.db.TryExclusive(ctx, "knowledge-embedding "+e.notebook)
	if err != nil || !ok {
		done()
		return false
	}
	defer release()
	pending, err := e.Pending(ctx, batchLimit)
	syncFailed := err != nil
	scope := ""
	if err == nil && history && (!available || len(pending.Passages) == 0) {
		scope = "history"
		pending, err = e.PendingScope(ctx, batchLimit, scope)
		syncFailed = err != nil
	}
	c = pending.Status.Config
	available = c.Enabled && c.Endpoint && p != nil && !embeddingFailed
	if err == nil && (!available || len(pending.Passages) == 0) {
		done()
		if scope == "history" && pending.Status.History != nil && pending.Status.History.State == "scanning" {
			return true
		}
		return false
	}
	providerFailed, submissionFailed := false, false
	if err == nil {
		texts := []string{}
		for _, passage := range pending.Passages {
			texts = append(texts, passage.Text)
		}
		var vectors [][]float32
		vectors, err = p.EmbedModel(ctx, c.Model, c.Dimensions, texts, true)
		providerFailed = err != nil
		if err != nil && len(texts) > 1 && workerCtx.Err() == nil {
			// Slow models may exceed the fixed request deadline for a large
			// batch. Keep the learned size and leave every vector pending
			// until a complete, validated smaller batch is submitted.
			e.mu.Lock()
			e.batchLimit = max(1, len(texts)/2)
			e.mu.Unlock()
			done()
			return true
		}
		if err == nil {
			sub := Submission{Scope: scope, Revision: pending.Status.Revision, ConfigID: pending.Status.ConfigID}
			for i, passage := range pending.Passages {
				sub.Vectors = append(sub.Vectors, VectorSubmission{ID: passage.ID, Hash: passage.Hash, NoteHash: passage.NoteHash, Vector: vectors[i]})
			}
			_, err = e.Submit(ctx, sub)
			submissionFailed = err != nil
		}
	}
	done()
	if err != nil {
		e.mu.Lock()
		var vectorWrite *vectorPersistenceError
		// A worker deadline during owner validation is transport state,
		// not a permanent catalogue or embedding failure. A provider's
		// own timeout must retain its separate recovery requirement.
		ownerFailed := !providerFailed && ownerSyncError(err)
		staleSubmission := errors.Is(err, errStaleEmbeddingConfiguration) || submissionFailed && errors.Is(err, workspace.ErrConflict)
		if errors.As(err, &vectorWrite) {
			// persistVectors already owns this failure and its recovery.
		} else if scope == "history" {
			if providerFailed {
				e.historyEmbeddingFailure = err.Error()
			} else if ownerFailed {
				e.historyOwnerFailure = err.Error()
			} else if staleSubmission {
				e.historySubmissionFailure = err.Error()
			} else if syncFailed && errors.Is(err, workspace.ErrConflict) {
				e.historySyncFailure = err.Error()
			} else {
				e.historyFailure = err.Error()
			}
		} else if syncFailed || ownerFailed || staleSubmission || errors.Is(err, workspace.ErrConflict) {
			// Successful owner/index synchronization supersedes this
			// failure even if an agent still owes compatible vectors.
			e.syncFailure = err.Error()
		} else {
			e.failure = err.Error()
		}
		e.mu.Unlock()
		if history && !embeddingFailed {
			e.mu.Lock()
			e.embeddingFailed = true
			e.mu.Unlock()
			return true
		}
		return false
	}
	select {
	case <-workerCtx.Done():
		return false
	default:
	}
	return workerCtx.Err() == nil
}
