package instanceconfig

import (
	"encoding/json"
	"errors"
	"forgetmenote/internal/durable"
	"os"
	"path/filepath"
	"strings"
)

// Migrate writes a new private operator file. Legacy files and environment are
// unchanged, so rollback is restoring the previous deployment configuration.
func Migrate(getenv func(string) string, data, destination string) error {
	if getenv("INSTANCE_CONFIG_FILE") != "" {
		return errors.New("migration requires legacy configuration without INSTANCE_CONFIG_FILE")
	}
	r, err := FromLegacy(getenv)
	if err != nil {
		return err
	}
	if !filepath.IsAbs(destination) {
		return errors.New("migration output requires an absolute private path")
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(destination))
	if err != nil {
		return errors.New("migration output directory unavailable")
	}
	target := filepath.Join(parent, filepath.Base(destination))
	content, err := filepath.Abs(filepath.Join(data, "projects"))
	if err != nil {
		return err
	}
	if actual, e := filepath.EvalSymlinks(content); e == nil {
		content = actual
	}
	if target == content || strings.HasPrefix(target, content+string(os.PathSeparator)) {
		return errors.New("instance configuration must stay outside notebook repositories")
	}
	raw, err := json.MarshalIndent(r.Config, "", "  ")
	// The durable file includes its final newline, just like the loader's limit.
	if err != nil || len(raw) >= MaximumBytes {
		return errors.New("instance configuration exceeds its byte budget")
	}
	file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errors.New("migration output already exists or is unavailable")
	}
	complete := false
	defer func() {
		file.Close()
		if !complete {
			os.Remove(target)
		}
	}()
	if _, err = file.Write(append(raw, '\n')); err != nil {
		return err
	}
	if err = file.Sync(); err != nil {
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	if err = durable.SyncDir(parent); err != nil {
		return err
	}
	complete = true
	return nil
}
