package catalog

import (
	"errors"
	"fmt"
	"strings"

	"forgetmenote/internal/workspace"
)

// ErrManagedAI tells every transport that notebook configuration cannot
// override the adopted instance policy.
var ErrManagedAI = errors.New("AI features are managed in Administration → AI features")

// AISettings is the instance policy. Notebook indexes retain only derived
// configuration and progress; credentials remain in operator-mounted files.
type AISettings struct {
	Revision          uint64 `json:"revision"`
	Embeddings        bool   `json:"embeddings"`
	Model             string `json:"model"`
	Dimensions        int    `json:"dimensions"`
	MapClassification bool   `json:"mapClassification"`
	Rerank            bool   `json:"rerank"`
	Citations         bool   `json:"citations"`
	Reviews           bool   `json:"reviews"`
	Writing           bool   `json:"writing"`
	// Policy an earlier release stored for hosted source OCR, descriptions
	// and transcription, which were removed, is ignored when it is read.
}

func (c *Catalog) AISettings() (AISettings, bool) {
	c.lock()
	defer c.mu.Unlock()
	if c.version < 0 {
		return AISettings{}, true
	}
	if c.state.AI == nil {
		return AISettings{Model: "text-embedding-3-small", Dimensions: 1536}, false
	}
	return *c.state.AI, true
}

// Ordinary notebooks are the shared ones that instance-wide background indexing
// and automatic reviews cover. A private Inbox, assistant, memory or task-queue
// notebook keeps its own consent boundary, and repository snapshots are indexed
// by their source manager rather than the editable-note engine.
func (p Project) Ordinary() bool { return !p.protected() && p.Kind != "repository" }

// OrdinaryNotebook reports whether project exists and is ordinary.
func (c *Catalog) OrdinaryNotebook(project string) bool {
	c.lock()
	defer c.mu.Unlock()
	p, ok := c.state.Projects[project]
	return ok && c.version >= 0 && p.Ordinary()
}

func (c *Catalog) BackgroundIndexAllowed(store *workspace.Store) bool {
	c.lock()
	defer c.mu.Unlock()
	for id, candidate := range c.stores {
		if candidate == store {
			return c.version >= 0 && c.state.Projects[id].Ordinary()
		}
	}
	return false
}

func (settings AISettings) validate() error {
	if len(settings.Model) > 200 || strings.TrimSpace(settings.Model) != settings.Model || settings.Embeddings && (settings.Model == "" || settings.Dimensions < 1 || settings.Dimensions > 4096) {
		return fmt.Errorf("%w: choose an embedding model and 1–4096 dimensions", ErrLocalInput)
	}
	return nil
}

func (c *Catalog) SetAISettings(actor string, settings AISettings) (AISettings, error) {
	c.lock()
	defer c.mu.Unlock()
	if !c.admin(actor) {
		return AISettings{}, workspace.ErrForbidden
	}
	var revision uint64
	if c.state.AI != nil {
		revision = c.state.AI.Revision
	}
	if revision != settings.Revision {
		return AISettings{}, workspace.ErrConflict
	}
	settings.Model = strings.TrimSpace(settings.Model)
	if err := settings.validate(); err != nil {
		return AISettings{}, err
	}
	settings.Revision++
	err := c.change(actor, "ai.settings", "instance", nil, func() error {
		c.state.AI = &settings
		return nil
	})
	return settings, err
}
