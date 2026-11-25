package automation

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"

	"forgetmenote/internal/capacity"
	"forgetmenote/internal/pg"
	"forgetmenote/internal/workspace"
)

// Definitions owns administrator configuration. Runtime checkpoints stay in Queue.
// File definitions are never copied into the writable configuration implicitly.
type Definitions struct {
	mu    sync.Mutex
	db    *pg.DB
	doc   *pg.Doc[definitionState]
	state definitionState
	files map[string]Spec
}
type definitionState struct {
	Version    int             `json:"version"`
	Generation uint64          `json:"generation"`
	Jobs       map[string]Spec `json:"jobs"`
}
type Definition struct {
	Spec      Spec   `json:"spec"`
	Revision  string `json:"revision"`
	ManagedBy string `json:"managedBy"`
}

// The configured jobs are one document, which jobs.json held.
const definitionsKind, definitionsID = "automation-definitions", "jobs"

// OpenDefinitions returns the configured jobs every gateway shares in db.
func OpenDefinitions(db *pg.DB) (*Definitions, error) {
	d := &Definitions{db: db, doc: pg.NewDoc[definitionState](db, definitionsKind, definitionsID, capacity.StateBytes.Limit()), state: emptyDefinitions(), files: map[string]Spec{}}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d, d.refresh()
}

func emptyDefinitions() definitionState {
	return definitionState{Version: 1, Jobs: map[string]Spec{}}
}

// refresh adopts the definitions another gateway saved. Caller holds mu. While
// they cannot be read, only file definitions exist, so no instance job runs.
func (d *Definitions) refresh() error {
	ctx, cancel := storageContext()
	defer cancel()
	next, changed, err := d.doc.Load(ctx)
	if err == nil && !changed {
		return nil
	}
	if err == nil {
		err = validDefinitions(&next)
	}
	if err != nil {
		d.doc.Forget()
		d.state = emptyDefinitions()
		return err
	}
	d.state = next
	return nil
}

func validDefinitions(state *definitionState) error {
	if state.Version == 0 && state.Jobs == nil {
		*state = emptyDefinitions()
		return nil
	}
	if state.Version != 1 || state.Jobs == nil {
		return errors.New("invalid automation definitions")
	}
	for id, s := range state.Jobs {
		if id != s.ID || s.Generation == 0 || s.Generation > state.Generation {
			return errors.New("invalid automation revision")
		}
		if err := s.ValidateConfigured(); err != nil {
			return err
		}
	}
	return nil
}

func cloneSpec(s Spec) Spec {
	b, _ := json.Marshal(s)
	var copy Spec
	_ = json.Unmarshal(b, &copy)
	return copy
}

func (d *Definitions) SetFiles(specs []Spec) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.refresh(); err != nil {
		return err
	}
	next := map[string]Spec{}
	for _, s := range specs {
		if _, ok := d.state.Jobs[s.ID]; ok {
			return fmt.Errorf("automation %s is both file-managed and instance-managed", s.ID)
		}
		if _, ok := next[s.ID]; ok {
			return errors.New("duplicate automation ID")
		}
		if err := s.ValidateConfigured(); err != nil {
			return err
		}
		next[s.ID] = cloneSpec(s)
	}
	d.files = next
	return nil
}

func (d *Definitions) get(id string) (Definition, bool) {
	s, ok := d.files[id]
	owner := "file"
	if !ok {
		s, ok = d.state.Jobs[id]
		owner = "instance"
	}
	if !ok {
		return Definition{}, false
	}
	return Definition{Spec: cloneSpec(s), Revision: Hash(s), ManagedBy: owner}, true
}
func (d *Definitions) Get(id string) (Definition, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	_ = d.refresh()
	return d.get(id)
}
func (d *Definitions) List() []Definition { return d.list(true) }

// list returns the jobs, first reading again those another gateway saved when
// refresh is set. Otherwise it returns the jobs this gateway last read or saved.
func (d *Definitions) list(refresh bool) []Definition {
	d.mu.Lock()
	defer d.mu.Unlock()
	if refresh {
		_ = d.refresh()
	}
	out := []Definition{}
	for id := range d.files {
		v, _ := d.get(id)
		out = append(out, v)
	}
	for id := range d.state.Jobs {
		v, _ := d.get(id)
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Spec.ID < out[j].Spec.ID })
	return out
}
func (d *Definitions) Specs() []Spec { return d.specs(true) }

// specs returns the jobs' specs as list does.
func (d *Definitions) specs(refresh bool) []Spec {
	out := []Spec{}
	for _, v := range d.list(refresh) {
		out = append(out, v.Spec)
	}
	return out
}

func (d *Definitions) Save(spec Spec, revision string) (Definition, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.refresh(); err != nil {
		return Definition{}, err
	}
	old, exists := d.get(spec.ID)
	if exists && old.ManagedBy == "file" {
		return Definition{}, errors.New("edit the mounted automation file or remove its definition before importing")
	}
	if exists && revision != old.Revision || !exists && revision != "" {
		return Definition{}, workspace.ErrConflict
	}
	next := definitionState{Version: 1, Generation: d.state.Generation + 1, Jobs: map[string]Spec{}}
	for k, v := range d.state.Jobs {
		next.Jobs[k] = v
	}
	spec.Generation = next.Generation
	if err := spec.ValidateConfigured(); err != nil {
		return Definition{}, err
	}
	next.Jobs[spec.ID] = cloneSpec(spec)
	if err := d.write(next); err != nil {
		return Definition{}, err
	}
	d.state = next
	v, _ := d.get(spec.ID)
	return v, nil
}
func (d *Definitions) Delete(id, revision string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.refresh(); err != nil {
		return err
	}
	old, ok := d.get(id)
	if !ok || old.Revision != revision {
		return workspace.ErrConflict
	}
	if old.ManagedBy == "file" {
		return errors.New("remove the automation from its mounted configuration")
	}
	next := definitionState{Version: 1, Generation: d.state.Generation + 1, Jobs: map[string]Spec{}}
	for k, v := range d.state.Jobs {
		if k != id {
			next.Jobs[k] = v
		}
	}
	if err := d.write(next); err != nil {
		return err
	}
	d.state = next
	return nil
}

// write stores state over the version this gateway last read; a save another
// gateway made first conflicts. A saved change is announced, so the gateway
// that schedules reads the jobs again.
func (d *Definitions) write(state definitionState) error {
	ctx, cancel := storageContext()
	defer cancel()
	switch err := d.doc.Save(ctx, state); {
	case errors.Is(err, pg.ErrTooLarge):
		return capacity.StateBytes.Error()
	case errors.Is(err, pg.ErrConflict):
		return workspace.ErrConflict
	case err != nil:
		return err
	}
	_ = d.db.Announce(ctx, QueueChannel, d.db.Holder)
	return nil
}
