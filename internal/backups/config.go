package backups

import "forgetmenote/internal/githubapp"

type WorkerConfig struct {
	Owner     string `json:"owner"`
	TokenFile string `json:"tokenFile"`
}
type AppConfig struct {
	App         githubapp.Config        `json:"app"`
	AllowCreate bool                    `json:"allowCreate"`
	Workers     map[string]WorkerConfig `json:"workers"`
}
