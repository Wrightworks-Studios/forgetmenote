package automation

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"forgetmenote/internal/pg"
)

// Learning uses the existing queue and publication journal. Inputs are private,
// short-lived replay evidence, never a second notebook or a conversation archive.
type LearningPolicy struct {
	CaptureInputs   bool   `json:"captureInputs"`
	Folder          string `json:"folder"`
	DebounceSeconds int    `json:"debounceSeconds"`
	MaxWaitSeconds  int    `json:"maxWaitSeconds"`
	MinExamples     int    `json:"minExamples"`
	RetentionHours  int    `json:"retentionHours"`
	Jev             bool   `json:"jev,omitempty"`
}

func (p LearningPolicy) validate() error {
	if !p.CaptureInputs || p.Folder == "" || p.Folder == "." || path.Clean(p.Folder) != p.Folder || strings.HasPrefix(p.Folder, "/") || p.Folder == ".." || strings.HasPrefix(p.Folder, "../") || strings.ContainsAny(p.Folder, "\\\x00\r\n") || p.DebounceSeconds < 1 || p.MaxWaitSeconds < p.DebounceSeconds || p.MaxWaitSeconds > 86400 || p.MinExamples < 2 || p.MinExamples > 16 || p.RetentionHours < 1 || p.RetentionHours > 168 || p.MaxWaitSeconds >= p.RetentionHours*3600 {
		return errors.New("learning requires input-capture consent, a local folder, debounce/max wait, 2–16 examples and 1–168 hour retention")
	}
	return nil
}

// Context is optional client-supplied task context, not authority. With no task
// ID the server records an activity window, never claims it saw a full chat.
type LearningContext struct {
	Conversation string `json:"conversation,omitempty"`
	RequestID    string `json:"requestId,omitempty"`
	Intent       string `json:"intent,omitempty"`
}
type LearningEvent struct {
	ID           string    `json:"id"`
	Actor        string    `json:"actor"`
	Account      string    `json:"account"`
	Epoch        uint64    `json:"epoch"`
	ProjectEpoch uint64    `json:"projectEpoch"`
	Credential   string    `json:"credential,omitempty"`
	Client       string    `json:"client,omitempty"`
	Conversation string    `json:"conversation"`
	Intent       string    `json:"intent,omitempty"`
	At           time.Time `json:"at"`
	Call         Call      `json:"call"`
	Shape        string    `json:"shape"`
	Outcome      string    `json:"outcome"`
	DurationMS   int64     `json:"durationMs"`
	Receipt      string    `json:"receipt,omitempty"`
	Complete     bool      `json:"complete"`
}
type LearningBatch struct {
	ReadyAt           time.Time                `json:"readyAt"`
	Expires           time.Time                `json:"expires"`
	Events            []LearningEvent          `json:"events"`
	Decisions         map[string]string        `json:"decisions,omitempty"`
	JudgmentAttempted bool                     `json:"judgmentAttempted,omitempty"`
	Processed         map[string]bool          `json:"processed,omitempty"`
	Published         []string                 `json:"published,omitempty"`
	Prepared          bool                     `json:"prepared,omitempty"`
	Fresh             []string                 `json:"fresh,omitempty"`
	Candidates        map[string]LearningDraft `json:"candidates,omitempty"`
	Sources           []string                 `json:"sources,omitempty"`
	Deferred          int                      `json:"deferred,omitempty"`
	Summary           string                   `json:"summary,omitempty"`
}

// A draft pins its original base before purchasing generation. An empty content
// records an interrupted attempt, avoiding a second paid call after a crash.
type LearningDraft struct {
	Revision string          `json:"revision"`
	Hash     string          `json:"hash"`
	Content  json.RawMessage `json:"content,omitempty"`
}

// ObserveLearning appends to one not-yet-started batch. A running batch is frozen;
// arrivals during model/validation work go into its successor. now is injectable
// so debounce and crash recovery can be tested without wall-clock sleeps.
func (q *Queue) ObserveLearning(spec Spec, event LearningEvent, now time.Time) (Run, error) {
	q.lock()
	defer q.unlock()
	if err := spec.Validate(); err != nil {
		return Run{}, err
	}
	if spec.Kind != "learning" || event.ID == "" {
		return Run{}, errors.New("learning observation requires a learning job and identity")
	}
	runs, err := q.pruneLearning(now)
	if err != nil {
		return Run{}, err
	}
	var pending *Run
	for i := range runs {
		r := &runs[i]
		if r.Learning == nil || r.Spec.ID != spec.ID {
			continue
		}
		*r, err = q.read(r.ID)
		if err != nil {
			return Run{}, err
		}
		if r.DryRun || Hash(r.Spec) != Hash(spec) {
			continue
		}
		for _, old := range r.Learning.Events {
			if old.ID == event.ID {
				if Hash([]any{old.Call, old.Intent, old.Actor, old.Conversation}) != Hash([]any{event.Call, event.Intent, event.Actor, event.Conversation}) {
					return Run{}, errors.New("learning request ID reused for different input")
				}
				return *r, nil
			}
		}
		if r.State == "queued" && r.Attempts == 0 && len(r.Learning.Events) < 128 && now.Before(r.Learning.ReadyAt) {
			pending = r
		}
	}
	if pending == nil {
		if err := q.makeRoom(runs, now); err != nil {
			return Run{}, err
		}
		r := Run{ID: Hash([]string{Hash(spec), event.ID}), Spec: spec, State: "queued", Created: now, Updated: now, Learning: &LearningBatch{Expires: now.Add(time.Duration(spec.Learning.RetentionHours) * time.Hour)}}
		pending = &r
	}
	pending.Learning.Events = append(pending.Learning.Events, event)
	pending.Updated = now
	pending.Learning.ReadyAt = now.Add(time.Duration(spec.Learning.DebounceSeconds) * time.Second)
	if deadline := pending.Created.Add(time.Duration(spec.Learning.MaxWaitSeconds) * time.Second); deadline.Before(pending.Learning.ReadyAt) {
		pending.Learning.ReadyAt = deadline
	}
	err = q.write(pending)
	return *pending, err
}

// Caller holds mu. Expiry runs even when no more calls arrive.
func (q *Queue) pruneLearning(now time.Time) ([]Run, error) {
	runs, err := q.scan(false)
	if err != nil {
		return nil, err
	}
	retained := runs[:0]
	for _, r := range runs {
		if r.Spec.Script != nil && r.Spec.Script.RetentionHours > 0 && r.Script != nil && !r.Script.Expired && now.Sub(r.Created) >= time.Duration(r.Spec.Script.RetentionHours)*time.Hour && !q.claimed(r) {
			full, err := q.read(r.ID)
			if err != nil {
				return nil, err
			}
			full.Script.Expired = true
			full.Script.State = nil
			full.Script.PreviewJobState = nil
			for _, step := range full.Script.Steps {
				step.Input = nil
				step.Result = nil
				step.Error = ""
			}
			full.Result = ""
			full.Answer = ""
			full.Error = "Private run journal expired; start a new run. Unresolved effects still require reconciliation."
			full.Citations = nil
			full.ChatContext = nil
			full.ChatDocuments = nil
			full.ChatResults = nil
			full.Publication = nil
			if full.State == "queued" || full.State == "running" {
				full.State = "action-required"
			}
			if err = q.write(&full); err != nil {
				return nil, err
			}
			r = full
		}
		if (r.Learning != nil && !now.Before(r.Learning.Expires) || r.Batch != nil && !now.Before(r.Batch.Expires)) && !q.claimed(r) {
			if err := q.records.remove(r.ID+".json", r.stored); err != nil && !errors.Is(err, pg.ErrConflict) {
				return nil, err
			} else if err != nil {
				// Another gateway changed the run first; it is kept.
				retained = append(retained, r)
			}
		} else {
			retained = append(retained, r)
		}
	}
	return retained, nil
}

func (q *Queue) PruneLearning(now time.Time) error {
	q.lock()
	defer q.unlock()
	_, err := q.pruneLearning(now)
	if err == nil {
		err = q.pruneEffectResults(now)
	}
	return err
}

func (q *Queue) LearningEvidence(spec Spec, through, now time.Time) ([]LearningEvent, error) {
	q.lock()
	defer q.unlock()
	runs, err := q.scan(false)
	if err != nil {
		return nil, err
	}
	var events []LearningEvent
	seen := map[string]bool{}
	for i := len(runs) - 1; i >= 0; i-- {
		r := runs[i]
		if r.Learning == nil || !now.Before(r.Learning.Expires) || r.Spec.ID != spec.ID {
			continue
		}
		r, err = q.read(r.ID)
		if err != nil {
			return nil, err
		}
		if Hash(r.Spec) != Hash(spec) {
			continue
		}
		for j := len(r.Learning.Events) - 1; j >= 0; j-- {
			e := r.Learning.Events[j]
			if !seen[e.ID] && !e.At.After(through) && now.Sub(e.At) < time.Duration(spec.Learning.RetentionHours)*time.Hour {
				events = append(events, e)
				seen[e.ID] = true
			}
			if len(events) >= 128 {
				return events, nil
			}
		}
	}
	return events, nil
}

func (r *Run) LearningReady(now time.Time) bool {
	return r.Learning == nil || !now.Before(r.Learning.ReadyAt)
}

func (r *Run) LearningPath(shape string) string {
	return fmt.Sprintf("%s/pattern-%s.md", r.Spec.Learning.Folder, Hash(shape)[:20])
}
