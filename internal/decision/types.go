package decision

import "encoding/json"

const (
	Boolean = "boolean"
	Choice  = "choice"
	Score   = "score"
)

type Question struct {
	Type         string `json:"type"`
	Instructions any    `json:"instructions"`
	Criteria     any    `json:"criteria,omitempty"`
}
type Request struct {
	State     any                 `json:"state"`
	Questions map[string]Question `json:"questions"`
}
type Answer struct {
	Type string `json:"type"`
	// Keep the legacy JSON name for existing organization checkpoints.
	Probability   *float64                   `json:"noul,omitempty"`
	Choice        string                     `json:"choice,omitempty"`
	Score         *float64                   `json:"score,omitempty"`
	Confidence    *float64                   `json:"confidence,omitempty"`
	Probabilities map[string]float64         `json:"probabilities,omitempty"`
	Legend        map[string]json.RawMessage `json:"legend,omitempty"`
}
type Usage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}
type Result struct {
	Model   string            `json:"model"`
	Answers map[string]Answer `json:"answers"`
	Usage   *Usage            `json:"usage"`
}

// Usage requires both explicit counters wherever it is decoded, including
// restored ledger records. JSON null must never become a known zero-token bill.
func (u *Usage) UnmarshalJSON(raw []byte) error {
	var wire struct {
		Input  *int64 `json:"input_tokens"`
		Output *int64 `json:"output_tokens"`
	}
	if !strictJSON(raw, &wire) || wire.Input == nil || wire.Output == nil || *wire.Input < 0 || *wire.Output < 0 || *wire.Input > 1000000000 || *wire.Output > 1000000000 {
		return ErrInvalid
	}
	*u = Usage{InputTokens: *wire.Input, OutputTokens: *wire.Output}
	return nil
}

// Existing checkpoints store TypeSafe's original answer encoding. Keep that
// format stable while consumers use provider-neutral boolean probabilities.
// The TypeSafe adapter also decodes this format before validating its answers.
type answerRecord Answer

func (a *answerRecord) UnmarshalJSON(raw []byte) error {
	type plain answerRecord
	var decoded plain
	if !strictJSON(raw, &decoded) {
		return ErrInvalid
	}
	var wire struct {
		Probabilities map[string]*float64 `json:"probabilities"`
	}
	if json.Unmarshal(raw, &wire) != nil {
		return ErrInvalid
	}
	for _, value := range wire.Probabilities {
		if value == nil {
			return ErrInvalid
		}
	}
	*a = answerRecord(decoded)
	return nil
}

func (a answerRecord) normalized() Answer {
	if a.Type == "noul" {
		a.Type = Boolean
	}
	return Answer(a)
}

func (a *Answer) UnmarshalJSON(raw []byte) error {
	var record answerRecord
	if err := record.UnmarshalJSON(raw); err != nil {
		return err
	}
	*a = record.normalized()
	return nil
}

func (a Answer) MarshalJSON() ([]byte, error) {
	record := answerRecord(a)
	if record.Type == Boolean {
		record.Type = "noul"
	}
	return json.Marshal(record)
}
