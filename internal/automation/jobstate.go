package automation

import (
	"encoding/json"
	"errors"
	"os"
	"time"

	"forgetmenote/internal/capacity"
	"forgetmenote/internal/workspace"
)

// Job state is private operational state, independent of run-history pruning.
// Callers bind scope to an authorized job/actor/project; packages cannot name it.
type JobState struct {
	Revision string          `json:"revision"`
	Data     json.RawMessage `json:"data"`
	Updated  time.Time       `json:"updated,omitempty"`
}

func jobScope(spec Spec) string {
	return Hash([]string{"job-state", spec.ID, spec.Actor, spec.Project})
}

// readPrivate decodes a private record into value and returns the version it
// was read at, for the write that replaces it.
func (q *Queue) readPrivate(name string, value any) (int64, error) {
	raw, version, err := q.records.read(name, capacity.StateBytes.Limit())
	if err != nil {
		return 0, err
	}
	return version, json.Unmarshal(raw, value)
}

// JobState only reads, so it takes no cross-gateway lock.
func (q *Queue) JobState(spec Spec) (JobState, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	state, _, err := q.jobState(spec)
	return state, err
}
func (q *Queue) jobState(spec Spec) (JobState, int64, error) {
	state := JobState{Data: json.RawMessage(`{}`)}
	version, err := q.readPrivate(jobScope(spec)+".job-state", &state)
	if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	return state, version, err
}
func (q *Queue) SaveJobState(spec Spec, revision string, data json.RawMessage, preview bool) (JobState, error) {
	q.lock()
	defer q.unlock()
	state, version, err := q.jobState(spec)
	if err != nil {
		return state, err
	}
	next, err := NextJobState(state, revision, data)
	if err != nil || preview || next.Revision == state.Revision {
		return next, err
	}
	raw, _ := json.Marshal(next)
	_, err = q.records.write(jobScope(spec)+".job-state", raw, version)
	return next, err
}

func NextJobState(state JobState, revision string, data json.RawMessage) (JobState, error) {
	var value map[string]any
	if len(data) > capacity.StateBytes.Limit()/2 || json.Unmarshal(data, &value) != nil || value == nil {
		return state, errors.New("job state must be a bounded JSON object")
	}
	next := JobState{Revision: Hash([]any{revision, value}), Data: append(json.RawMessage(nil), data...), Updated: time.Now().UTC()}
	if state.Revision == next.Revision {
		return state, nil
	}
	if state.Revision != revision {
		return state, workspace.ErrConflict
	}
	return next, nil
}

// Effect records keep cross-run identities and unknown-outcome barriers. They
// contain hashes and compact acknowledgments, never request bodies or credentials.
type Effect struct {
	ID          string          `json:"id"`
	Scope       string          `json:"scope"`
	Action      string          `json:"action"`
	InputHash   string          `json:"inputHash"`
	Actor       string          `json:"actor"`
	Connection  string          `json:"connection"`
	Operation   string          `json:"operation"`
	State       string          `json:"state"`
	Result      json.RawMessage `json:"result,omitempty"`
	Updated     time.Time       `json:"updated"`
	RetainUntil time.Time       `json:"retainUntil,omitempty"`
	Resolution  string          `json:"resolution,omitempty"`
}

// The derived index keeps the per-action path constant-time after restart.
// Only this queue writes effect records; raw analysis results stay on disk.
func (q *Queue) indexEffects() error {
	if q.effectIndex != nil {
		return nil
	}
	index := map[string]map[string]string{}
	expiry := map[string]time.Time{}
	files, err := q.records.list(".effect")
	if err != nil {
		return err
	}
	for _, f := range files {
		var e Effect
		if _, err = q.readPrivate(f.name, &e); err != nil {
			return err
		}
		if index[e.Scope] == nil {
			index[e.Scope] = map[string]string{}
		}
		index[e.Scope][e.ID] = e.State
		if len(e.Result) > 0 && !e.RetainUntil.IsZero() {
			expiry[e.ID] = e.RetainUntil
		}
	}
	q.effectIndex = index
	q.effectExpiry = expiry
	return nil
}

// writeEffect stores e over the version it was read at, 0 for a new effect, so
// two gateways cannot both begin one action.
func (q *Queue) writeEffect(e Effect, version int64) error {
	raw, _ := json.Marshal(e)
	if _, err := q.records.write(e.ID+".effect", raw, version); err != nil {
		q.effectIndex = nil
		return err
	}
	if q.effectIndex != nil {
		if q.effectIndex[e.Scope] == nil {
			q.effectIndex[e.Scope] = map[string]string{}
		}
		q.effectIndex[e.Scope][e.ID] = e.State
		if len(e.Result) > 0 && !e.RetainUntil.IsZero() {
			q.effectExpiry[e.ID] = e.RetainUntil
		} else {
			delete(q.effectExpiry, e.ID)
		}
	}
	return nil
}
func (q *Queue) effects(scope string) ([]Effect, error) {
	if err := q.indexEffects(); err != nil {
		return nil, err
	}
	out := []Effect{}
	for id := range q.effectIndex[scope] {
		var e Effect
		if _, err := q.readPrivate(id+".effect", &e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}
func (q *Queue) Effects(scope string) ([]Effect, error) {
	q.lock()
	defer q.unlock()
	return q.effects(scope)
}
func (q *Queue) BeginEffect(e Effect) (Effect, bool, error) {
	q.lock()
	defer q.unlock()
	if e.Scope == "" || e.Action == "" || e.InputHash == "" || len(e.Action) > 512 {
		return e, false, errors.New("effect requires scope, stable action identity and input hash")
	}
	e.ID = Hash([]string{e.Scope, e.Action})
	if err := q.indexEffects(); err != nil {
		return e, false, err
	}
	if _, ok := q.effectIndex[e.Scope][e.ID]; ok {
		var old Effect
		if _, err := q.readPrivate(e.ID+".effect", &old); err != nil {
			return e, false, err
		}
		// A retained outcome belongs to the actor that caused it; another
		// writer sharing the scope never receives it, even with equal input.
		if old.Actor != e.Actor {
			return e, false, errors.New("action identity is already in use; choose a new action ID")
		}
		if old.InputHash != e.InputHash {
			return old, false, errors.New("action identity reused with different input")
		}
		if old.State == "accepted" && len(old.Result) > 0 && (old.RetainUntil.IsZero() || time.Now().Before(old.RetainUntil)) {
			return old, false, nil
		}
		return old, false, errors.New("action already attempted; inspect its retained outcome")
	}
	for id, state := range q.effectIndex[e.Scope] {
		if state == "started" || state == "unknown" {
			return e, false, errors.New("unresolved external outcome " + id + " blocks this scope, including new action IDs")
		}
	}
	e.State = "started"
	e.Updated = time.Now().UTC()
	if err := q.writeEffect(e, 0); err != nil {
		return e, false, err
	}
	return e, true, nil
}
func (q *Queue) FinishEffect(id, state string, result any) (Effect, error) {
	q.lock()
	defer q.unlock()
	var e Effect
	if !digestPattern.MatchString(id) {
		return e, errors.New("invalid effect ID")
	}
	version, err := q.readPrivate(id+".effect", &e)
	if err != nil {
		return e, err
	}
	if e.State != "started" || state != "accepted" && state != "rejected" && state != "unknown" {
		return e, errors.New("invalid effect transition")
	}
	e.State = state
	e.Updated = time.Now().UTC()
	if result != nil {
		e.Result, err = json.Marshal(result)
		if err != nil {
			return e, err
		}
		if len(e.Result) > capacity.StateBytes.Limit()/4 {
			return e, errors.New("effect result exceeds private state limit")
		}
	}
	return e, q.writeEffect(e, version)
}

// An administrator supplies a concrete reconciliation note; negative search
// results alone are not proof of rejection. No automatic re-execution follows.
func (q *Queue) ResolveEffect(id, state, note string, result json.RawMessage) (Effect, error) {
	q.lock()
	defer q.unlock()
	var e Effect
	if !digestPattern.MatchString(id) || len(note) < 10 || len(note) > 2000 || state != "accepted" && state != "rejected" {
		return e, errors.New("explicit accepted/rejected reconciliation and evidence note required")
	}
	version, err := q.readPrivate(id+".effect", &e)
	if err != nil {
		return e, err
	}
	if e.State != "unknown" && e.State != "started" {
		return e, errors.New("effect does not need reconciliation")
	}
	if len(result) > 16384 || len(result) > 0 && !json.Valid(result) {
		return e, errors.New("invalid reconciliation receipt")
	}
	e.State = state
	e.Resolution = note
	e.Result = result
	e.Updated = time.Now().UTC()
	return e, q.writeEffect(e, version)
}

func (q *Queue) pruneEffectResults(now time.Time) error {
	if err := q.indexEffects(); err != nil {
		return err
	}
	for id, expires := range q.effectExpiry {
		if now.Before(expires) {
			continue
		}
		var e Effect
		version, err := q.readPrivate(id+".effect", &e)
		if err != nil {
			return err
		}
		e.Result = nil
		if err := q.writeEffect(e, version); err != nil {
			return err
		}
	}
	return nil
}

// ChangeJobState atomically appends or acknowledges small host-owned inboxes.
// The callback must not perform I/O or call the queue; network work happens
// outside this lock and uses persisted intent/checkpoints.
func (q *Queue) ChangeJobState(spec Spec, change func(json.RawMessage) (json.RawMessage, error)) (JobState, error) {
	q.lock()
	defer q.unlock()
	state, version, err := q.jobState(spec)
	if err != nil {
		return state, err
	}
	raw, err := change(state.Data)
	if err != nil {
		return state, err
	}
	if string(raw) == string(state.Data) {
		return state, nil
	}
	next, err := NextJobState(state, state.Revision, raw)
	if err != nil {
		return state, err
	}
	encoded, _ := json.Marshal(next)
	_, err = q.records.write(jobScope(spec)+".job-state", encoded, version)
	return next, err
}
