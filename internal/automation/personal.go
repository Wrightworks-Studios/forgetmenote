package automation

import (
	"errors"
	"slices"
)

// Personal is a consent revision fence, not an administrator-supplied job kind.
// The web owner derives these jobs from current employee settings.
type Personal struct {
	Account  string `json:"account"`
	Settings uint64 `json:"settings"`
	Policy   uint64 `json:"policy"`
}

func (s Spec) validatePersonal() error {
	if !slices.Contains([]string{"", "none", "low", "medium", "high", "xhigh", "max"}, s.ReasoningEffort) {
		return errors.New("invalid workload reasoning effort")
	}
	if (s.Kind == "personal") != (s.Personal != nil) {
		return errors.New("personal task requires a consent revision")
	}
	if s.Personal == nil {
		return nil
	}
	if len(s.Personal.Account) != 32 || s.Personal.Settings == 0 || s.Personal.Policy == 0 || s.ID != "personal-"+s.Personal.Account || s.Project != "assistant-"+s.Personal.Account || s.Delivery != nil || s.Script != nil || s.Output != "" || s.Instruction != "" || s.NotifySlackUser != "" || s.NotifyChannel != "" {
		return errors.New("invalid personal task binding")
	}
	return nil
}
