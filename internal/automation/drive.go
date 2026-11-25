package automation

import (
	"encoding/json"
	"errors"
	"os"
	"path"
	"strings"
)

type DriveSync struct {
	Connection  string   `json:"connection"`
	Folders     []string `json:"folders"`
	Destination string   `json:"destination"`
	MaxFiles    int      `json:"maxFiles,omitempty"`
}
type DriveFolder struct {
	ID     string `json:"id"`
	Cursor string `json:"cursor,omitempty"`
}
type DriveFile struct {
	Path     string `json:"path"`
	Revision string `json:"revision"`
	Version  string `json:"version"`
	Seen     bool   `json:"seen"`
}
type DriveState struct {
	Version  int                  `json:"version"`
	Policy   string               `json:"policy"`
	Folders  []DriveFolder        `json:"folders"`
	Visited  map[string]bool      `json:"visited"`
	Files    map[string]DriveFile `json:"files"`
	Complete bool                 `json:"complete"`
}

func (d *DriveSync) validate() error {
	if d == nil || !identifier.MatchString(d.Connection) || len(d.Folders) == 0 || len(d.Folders) > 32 || d.MaxFiles < 0 || d.MaxFiles > 128 || d.Destination == "" || path.Clean(d.Destination) != d.Destination || strings.ContainsAny(d.Destination, "\\\x00") || strings.HasPrefix(d.Destination, "/") || d.Destination == ".." || strings.HasPrefix(d.Destination, "../") {
		return errors.New("Drive sync requires a connection, root folders and notebook destination")
	}
	for _, id := range d.Folders {
		if !identifier.MatchString(id) {
			return errors.New("invalid Drive folder ID")
		}
	}
	return nil
}

// ReadDrive only reads, so it takes no cross-gateway lock.
func (q *Queue) ReadDrive(id string) (DriveState, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	state := DriveState{Version: 1, Files: map[string]DriveFile{}, Visited: map[string]bool{}}
	raw, _, err := q.records.read(Hash([]string{id, "drive"})+".state", 4<<20)
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	if json.Unmarshal(raw, &state) != nil || state.Version != 1 || state.Files == nil || state.Visited == nil {
		return state, errors.New("invalid Drive checkpoint")
	}
	return state, nil
}
func (q *Queue) SaveDrive(id string, state DriveState) error {
	q.lock()
	defer q.unlock()
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if len(raw) > 4<<20 || len(state.Files) > 10000 || len(state.Visited) > 10000 {
		return errors.New("Drive sync exceeds 10000 items; narrow the selected folders")
	}
	return q.records.replace(Hash([]string{id, "drive"})+".state", raw)
}
