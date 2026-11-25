package automation

import (
	"encoding/json"
	"errors"
	"os"
	"time"
)

type Trigger struct {
	Kind            string `json:"kind"`
	Project         string `json:"project,omitempty"`
	Channel         string `json:"channel,omitempty"`
	DebounceSeconds int    `json:"debounceSeconds"`
	MaxWaitSeconds  int    `json:"maxWaitSeconds"`
}
type Event struct {
	PreviousRevision string    `json:"previousRevision,omitempty"`
	ID               string    `json:"id"`
	Project          string    `json:"project,omitempty"`
	Revision         string    `json:"revision,omitempty"`
	Actor            string    `json:"actor,omitempty"`
	Epoch            uint64    `json:"epoch,omitempty"`
	Text             string    `json:"text,omitempty"`
	At               time.Time `json:"at"`
}
type EventBatch struct {
	ReadyAt time.Time `json:"readyAt"`
	Expires time.Time `json:"expires"`
	Events  []Event   `json:"events"`
}

func (t *Trigger) validate() error {
	if t == nil {
		return nil
	}
	if t.DebounceSeconds < 1 || t.MaxWaitSeconds < t.DebounceSeconds || t.MaxWaitSeconds > 86400 {
		return errors.New("invalid event debounce")
	}
	switch t.Kind {
	case "repository.updated":
		if t.Project == "" || t.Channel != "" {
			return errors.New("repository trigger requires a notebook")
		}
	case "slack.knowledge":
		if t.Channel == "" || t.Project != "" {
			return errors.New("Slack trigger requires a configured channel")
		}
	default:
		return errors.New("unsupported event trigger")
	}
	return nil
}

// A durable cursor prevents unchanged source snapshots from retriggering after
// run retention. Event batches freeze when claimed; arrivals get a successor.
func (q *Queue) ObserveEvent(spec Spec, event Event, now time.Time) (Run, error) {
	q.lock()
	defer q.unlock()
	if err := spec.Validate(); err != nil {
		return Run{}, err
	}
	if spec.Trigger == nil || event.ID == "" || len(event.ID) > 256 || len(event.Text) > 16000 {
		return Run{}, errors.New("invalid automation event")
	}
	var cursor struct {
		Revision string `json:"revision,omitempty"`
		Version  int    `json:"version"`
		Spec     string `json:"spec"`
		Event    string `json:"event"`
	}
	name := Hash([]string{spec.ID, "event-cursor"}) + ".state"
	raw, cursorVersion, err := q.records.read(name, 4096)
	if err == nil {
		if json.Unmarshal(raw, &cursor) != nil || cursor.Version != 1 {
			return Run{}, errors.New("invalid event cursor")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return Run{}, err
	}
	if spec.Trigger.Kind == "repository.updated" && cursor.Spec == Hash(spec) && cursor.Event == event.ID {
		return Run{}, nil
	}
	runs, err := q.scan(false)
	if err != nil {
		return Run{}, err
	}
	saveCursor := func() error {
		cursor.Version = 1
		cursor.Spec = Hash(spec)
		cursor.Event = event.ID
		cursor.Revision = event.Revision
		raw, _ := json.Marshal(cursor)
		_, err := q.records.write(name, raw, cursorVersion)
		return err
	}
	var pending *Run
	for _, summary := range runs {
		if summary.Spec.ID != spec.ID || summary.Batch == nil {
			continue
		}
		r, e := q.read(summary.ID)
		if e != nil {
			return Run{}, e
		}
		if r.DryRun || Hash(r.Spec) != Hash(spec) {
			continue
		}
		for _, old := range r.Batch.Events {
			if old.ID == event.ID {
				if err = saveCursor(); err != nil {
					return Run{}, err
				}
				return r, nil
			}
		}
		if !r.DryRun && r.State == "queued" && r.Attempts == 0 && Hash(r.Spec) == Hash(spec) && len(r.Batch.Events) < 64 && now.Before(r.Batch.ReadyAt) {
			pending = &r
		}
	}
	if pending == nil {
		if err := q.makeRoom(runs, now); err != nil {
			return Run{}, err
		}
		pending = &Run{ID: Hash([]string{Hash(spec), event.ID}), Spec: spec, State: "queued", Created: now, Batch: &EventBatch{Expires: now.Add(24 * time.Hour)}}
		// The run first queued for this event may remain after later events
		// replaced its batch, as when a repository returns to an earlier
		// snapshot. A finished one is queued again over its version; an
		// unfinished one ends first, and the event is observed again then.
		if old, err := q.read(pending.ID); err == nil {
			if !terminal(old.State) {
				return Run{}, nil
			}
			pending.stored = old.stored
		} else if !errors.Is(err, os.ErrNotExist) {
			return Run{}, err
		}
	}
	pending.Updated = now
	if spec.Trigger.Kind == "repository.updated" {
		// Preserve the comparison before the first coalesced update. A first
		// observation has no prior snapshot and remains a truthful baseline.
		event.PreviousRevision = cursor.Revision
		if len(pending.Batch.Events) > 0 {
			event.PreviousRevision = pending.Batch.Events[0].PreviousRevision
		}
		pending.Batch.Events = []Event{event}
	} else {
		pending.Batch.Events = append(pending.Batch.Events, event)
	}
	pending.Batch.ReadyAt = now.Add(time.Duration(spec.Trigger.DebounceSeconds) * time.Second)
	deadline := pending.Created.Add(time.Duration(spec.Trigger.MaxWaitSeconds) * time.Second)
	if deadline.Before(pending.Batch.ReadyAt) {
		pending.Batch.ReadyAt = deadline
	}
	if err = q.write(pending); err != nil {
		return Run{}, err
	}
	if err = saveCursor(); err != nil {
		return Run{}, err
	}
	return *pending, nil
}

func (q *Queue) PreviewEvidence(spec Spec, key, evidence string) (Run, error) {
	q.lock()
	defer q.unlock()
	if err := spec.Validate(); err != nil {
		return Run{}, err
	}
	old, err := q.read(evidence)
	if err != nil {
		return Run{}, err
	}
	if old.Spec.ID != spec.ID || old.Spec.Project != spec.Project {
		return Run{}, errors.New("select retained evidence from this job")
	}
	if spec.Kind == "learning" {
		if old.Learning == nil || !time.Now().Before(old.Learning.Expires) {
			return Run{}, errors.New("learning evidence expired")
		}
	} else if spec.Trigger == nil || old.Batch == nil || !time.Now().Before(old.Batch.Expires) {
		return Run{}, errors.New("event evidence expired")
	}

	id := Hash([]string{spec.ID, "preview", key})
	if existing, e := q.read(id); e == nil {
		return existing, nil
	}
	runs, err := q.scan(false)
	if err != nil {
		return Run{}, err
	}
	if err := q.makeRoom(runs, time.Now()); err != nil {
		return Run{}, err
	}
	r := Run{ID: id, Spec: spec, State: "queued", DryRun: true, Created: time.Now(), Updated: time.Now()}
	if spec.Kind == "learning" {
		r.Learning = &LearningBatch{ReadyAt: time.Now(), Expires: old.Learning.Expires, Events: old.Learning.Events}
	} else {
		r.Batch = &EventBatch{ReadyAt: time.Now(), Expires: old.Batch.Expires, Events: old.Batch.Events}
	}
	err = q.write(&r)
	return r, err
}
