package workspace

import (
	"context"
	"errors"
	"math"
	"os"
	"sync"

	"forgetmenote/internal/gitexport"
	"forgetmenote/internal/notestore"
)

// A backup of a notebook kept in the notes store is a bundle of its Git
// export, the bare repository <data>/exports/<notebook>.git that
// internal/gitexport keeps. One process holds an export directory, so the
// process running the exporter registers it here for the notes store whose
// notebooks it exports.
var exporters struct {
	sync.Mutex
	stores map[*notestore.Store]*gitexport.Exporter
}

// errNoExport is a backup in a process that runs no export of the notebook.
var errNoExport = errors.New("notebook backups need the notebook's Git export, which this process does not run")

// UseExporter makes exporter, which exports notes' notebooks, the source of
// their backups until the returned function runs.
func UseExporter(notes *notestore.Store, exporter *gitexport.Exporter) func() {
	exporters.Lock()
	defer exporters.Unlock()
	if exporters.stores == nil {
		exporters.stores = map[*notestore.Store]*gitexport.Exporter{}
	}
	exporters.stores[notes] = exporter
	return func() {
		exporters.Lock()
		defer exporters.Unlock()
		if exporters.stores[notes] == exporter {
			delete(exporters.stores, notes)
		}
	}
}

// RemoveExport deletes the Git export of notebook, which notes no longer
// holds, when this process exports notes' notebooks (UseExporter). The
// exporter keeps a deleted notebook's export, and a notebook created or
// imported later under the same ID would diverge from it, failing every
// backup and the off-site push, so each path that deletes a notebook calls
// RemoveExport after the delete. It waits for an export of notebook in
// progress; one starting later finds no notebook and makes no export.
// Without a registered exporter it does nothing.
func RemoveExport(ctx context.Context, notes *notestore.Store, notebook string) error {
	exporters.Lock()
	exporter := exporters.stores[notes]
	exporters.Unlock()
	if exporter == nil {
		return nil
	}
	return exporter.Remove(ctx, notebook)
}

// Backup writes a bundle of the notebook's whole history to target, as git
// bundle create --all in its repository did: into Git's lock file
// target.lock, new with mode 0666 less the umask, renamed to target once
// complete. An existing lock file, another backup's, refuses the backup.
func (n *dbNotebook) Backup(ctx context.Context, target string) error {
	exporter, err := n.backupExport(ctx)
	if err != nil {
		return err
	}
	lock := target + ".lock"
	f, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0666)
	if err != nil {
		return err
	}
	err = exporter.Bundle(ctx, n.notebook, f, math.MaxInt64)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(lock, target)
	}
	if err != nil {
		os.Remove(lock)
	}
	return err
}

// BackupLimited writes the bundle into target as it is made and stops at
// limit bytes, failing with "MCP export exceeds 1 GiB; use operator backup",
// so temporary export storage stays bounded.
func (n *dbNotebook) BackupLimited(ctx context.Context, target string, limit int64) error {
	exporter, err := n.backupExport(ctx)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	err = exporter.Bundle(ctx, n.notebook, file, limit)
	return errors.Join(err, file.Close())
}

// backupExport refuses a backup while accepted live edits wait for their
// checkpoint, as the Git backend did, and returns the notebook's exporter.
// The bundle needs no lock: the exporter first brings the export up to the
// head, which holds every revision committed by now, and live edits accepted
// meanwhile stay pending, like those that waited for a Git backup to finish.
func (n *dbNotebook) backupExport(ctx context.Context) (*gitexport.Exporter, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.hasLivePending() {
		return nil, ErrLivePending
	}
	if err := n.clean(ctx); err != nil {
		return nil, err
	}
	exporters.Lock()
	defer exporters.Unlock()
	if exporter := exporters.stores[n.noteStore]; exporter != nil {
		return exporter, nil
	}
	return nil, errNoExport
}
