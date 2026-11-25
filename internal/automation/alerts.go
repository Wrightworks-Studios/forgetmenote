package automation

import (
	"encoding/json"
	"errors"
	"os"
	"time"
)

type AlertPolicy struct {
	ActiveAfter    int  `json:"activeAfter,omitempty"`
	ResolvedAfter  int  `json:"resolvedAfter,omitempty"`
	RepeatSeconds  int  `json:"repeatSeconds,omitempty"`
	NotifyResolved bool `json:"notifyResolved,omitempty"`
}
type Incident struct {
	Active   bool   `json:"active"`
	Severity string `json:"severity,omitempty"`
	Summary  string `json:"summary"`
	Key      string `json:"key,omitempty"`
}
type AlertDecision struct {
	Notify    bool     `json:"notify"`
	Recovered bool     `json:"recovered,omitempty"`
	Incident  Incident `json:"incident"`
}
type incidentState struct {
	Key      string        `json:"key,omitempty"`
	Version  int           `json:"version"`
	Run      string        `json:"run"`
	Active   int           `json:"active"`
	Resolved int           `json:"resolved"`
	Open     bool          `json:"open"`
	Severity string        `json:"severity"`
	Notified time.Time     `json:"notified"`
	Decision AlertDecision `json:"decision"`
}

func (p *AlertPolicy) validate() error {
	if p == nil {
		return nil
	}
	if p.ActiveAfter < 0 || p.ActiveAfter > 100 || p.ResolvedAfter < 0 || p.ResolvedAfter > 100 || p.RepeatSeconds < 0 || p.RepeatSeconds > 31*86400 {
		return errors.New("invalid incident policy")
	}
	return nil
}
func (q *Queue) Incident(spec Spec, run string, incident Incident, now time.Time, preview bool) (AlertDecision, error) {
	q.lock()
	defer q.unlock()
	name := Hash([]string{spec.ID, "incident"}) + ".state"
	state := incidentState{Version: 1}
	raw, version, err := q.records.read(name, 16<<10)
	if err == nil {
		if json.Unmarshal(raw, &state) != nil || state.Version != 1 {
			return AlertDecision{}, errors.New("invalid incident state")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return AlertDecision{}, err
	}
	if state.Run == run {
		return state.Decision, nil
	}
	if incident.Key != "" && incident.Key != state.Key {
		state = incidentState{Version: 1, Key: incident.Key}
	}
	p := AlertPolicy{}
	if spec.Alert != nil {
		p = *spec.Alert
	}
	decision := AlertDecision{Incident: incident}
	if incident.Active {
		state.Active++
		state.Resolved = 0
		if state.Active >= max(1, p.ActiveAfter) {
			decision.Notify = !state.Open || state.Severity != incident.Severity || p.RepeatSeconds > 0 && now.Sub(state.Notified) >= time.Duration(p.RepeatSeconds)*time.Second
			state.Open = true
			state.Severity = incident.Severity
		}
	} else {
		state.Resolved++
		state.Active = 0
		if state.Open && state.Resolved >= max(1, p.ResolvedAfter) {
			decision.Notify = p.NotifyResolved
			decision.Recovered = true
			state.Open = false
			state.Severity = ""
		}
	}
	if decision.Notify {
		state.Notified = now
	}
	state.Run = run
	state.Decision = decision
	if !preview {
		raw, _ := json.Marshal(state)
		if _, err := q.records.write(name, raw, version); err != nil {
			return AlertDecision{}, err
		}
	}
	return decision, nil
}
