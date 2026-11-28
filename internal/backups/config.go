package backups

import (
	"forgetmenote/internal/drivebackup"
	"forgetmenote/internal/githubapp"
)

type WorkerConfig struct {
	Owner     string `json:"owner"`
	TokenFile string `json:"tokenFile"`
}

// AppConfig is BACKUP_APP_CONFIG_FILE. The GitHub App and the drive providers
// are each optional; an instance configures at least one of them.
type AppConfig struct {
	App         githubapp.Config        `json:"app"`
	AllowCreate bool                    `json:"allowCreate"`
	Workers     map[string]WorkerConfig `json:"workers"`
	Drives      *drivebackup.Config     `json:"drives,omitempty"`
}

// GitHub reports whether the configuration names a GitHub App.
func (c AppConfig) GitHub() bool { return c.App != (githubapp.Config{}) }
