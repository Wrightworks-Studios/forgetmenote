package automation

import (
	"errors"
	"path"
	"strings"

	"forgetmenote/internal/messaging"
)

// Native tasks reuse existing instance owners; they do not accept executable code.
type NativeTask struct {
	Task            string  `json:"task"`
	Folder          string  `json:"folder,omitempty"`
	LookbackHours   int     `json:"lookbackHours,omitempty"`
	MaxAgeMinutes   int     `json:"maxAgeMinutes,omitempty"`
	WarningPercent  float64 `json:"warningPercent,omitempty"`
	CriticalPercent float64 `json:"criticalPercent,omitempty"`
	// Client is the messaging client whose members email-pairing pairs, or
	// task-reminder reminds.
	Client string `json:"client,omitempty"`
}

func (n *NativeTask) validate() error {
	if n == nil {
		return errors.New("native task configuration required")
	}
	if n.LookbackHours < 0 || n.LookbackHours > 24*31 || n.MaxAgeMinutes < 0 || n.MaxAgeMinutes > 31*24*60 || n.WarningPercent < 0 || n.CriticalPercent > 100 || n.WarningPercent > n.CriticalPercent {
		return errors.New("invalid native task limits")
	}
	if n.Folder != "" && (path.Clean(n.Folder) != n.Folder || strings.ContainsAny(n.Folder, "\\\x00") || strings.HasPrefix(n.Folder, "/") || n.Folder == ".." || strings.HasPrefix(n.Folder, "../")) {
		return errors.New("invalid task folder")
	}
	if (n.Task == "email-pairing" || n.Task == "task-reminder") != (n.Client != "") || n.Client != "" && !messaging.ValidID(n.Client) {
		return errors.New("email-pairing and task-reminder, and only they, name a messaging client")
	}
	switch n.Task {
	case "storage-health", "notebook-health", "automation-health", "activity-summary", "profile-health", "profile-compaction", "email-pairing", "task-reminder":
		return nil
	case "repository-refresh":
		return errors.New("native task repository-refresh was removed: repository notebooks refresh on their own, so remove this job")
	default:
		return errors.New("unsupported native task")
	}
}

func (s Spec) UsesAI() bool {
	return s.Repair != nil || s.AIConnection != "" || s.Kind == "assistant" || s.Kind == "personal" || s.Kind == "task" || s.Kind == "learning" || s.Script.UsesAI()
}
