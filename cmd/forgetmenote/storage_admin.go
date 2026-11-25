package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/gitexport"
	"forgetmenote/internal/notestore"
	"forgetmenote/internal/workspace"
)

// runStorageAdmin runs admin storage export on a stopped instance: it brings
// each notebook's Git export, DATA/exports/ID.git, up to its head, repacks
// and checks it, and verifies it against the notes store. An export that no
// longer matches its notebook fails; with -recreate it is deleted and
// exported again from scratch. It covers every notebook the catalog lists,
// or only the one -notebook names. Each notebook's outcome is one JSON line;
// any failure fails the command.
func runStorageAdmin(args []string, stdout io.Writer) error {
	usage := errors.New("usage: forgetmenote admin storage export [-data DIR] [-notebook ID] [-recreate] (stop the instance first)")
	if len(args) == 0 || args[0] != "export" {
		return usage
	}
	flags := flag.NewFlagSet("admin storage export", flag.ContinueOnError)
	data := flags.String("data", env("DATA_DIR", "/data"), "stopped instance data directory")
	only := flags.String("notebook", "", "only this notebook ID")
	recreate := flags.Bool("recreate", false, "delete each export and export it again from scratch, to repair one that diverged or fails its checks")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return usage
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	db, dir, config, err := stateDatabase(ctx, flags, *data)
	if err != nil {
		return err
	}
	defer db.Close()
	// A serving gateway's exporter writes the same repositories.
	if live, err := db.LiveGateways(ctx, db.Pool); err != nil || live > 0 {
		if err == nil {
			err = fmt.Errorf("%d gateways are serving; stop the instance first", live)
		}
		return err
	}
	ids := []string{*only}
	if *only == "" {
		if ids, err = catalog.StoredNotebooks(ctx, db); err != nil {
			return err
		}
	} else if !workspace.ValidNotebookID(*only) {
		return fmt.Errorf("invalid notebook ID %q", *only)
	}
	blobs, err := openBlobs(ctx, config.Server.Blobs, dir)
	if err != nil {
		return err
	}
	defer closeBlobs(blobs)
	store, err := notestore.Open(ctx, db, blobs, notestore.Options{ImportEntryID: workspace.ImportEntryID})
	if err != nil {
		return err
	}
	defer store.Close()
	exporter, err := gitexport.New(store, filepath.Join(dir, "exports"), gitexport.Options{})
	if err != nil {
		return err
	}
	defer exporter.Close()
	out := json.NewEncoder(stdout)
	failed := 0
	for _, id := range ids {
		result := map[string]any{"notebook": id}
		started := time.Now()
		err = exportNotebook(ctx, store, exporter, id, *recreate, result)
		result["seconds"] = time.Since(started).Round(time.Millisecond).Seconds()
		if err != nil {
			result["error"] = err.Error()
			failed++
		}
		if err := out.Encode(result); err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	if failed > 0 {
		return fmt.Errorf("storage export failed for %d of %d notebooks", failed, len(ids))
	}
	return nil
}

func exportNotebook(ctx context.Context, store *notestore.Store, exporter *gitexport.Exporter, id string, recreate bool, result map[string]any) error {
	export, status := exporter.Sync, "exported"
	if recreate {
		export, status = exporter.Recreate, "recreated"
	}
	head, err := export(ctx, id)
	if errors.Is(err, fs.ErrNotExist) {
		result["status"] = "skipped: not in the notebook store"
		return nil
	}
	if err != nil {
		return err
	}
	if err = exporter.Maintain(ctx, id); err != nil {
		return err
	}
	result["status"], result["repository"] = status, exporter.Path(id)
	rep, err := gitexport.Verify(ctx, store.Notebook(id), exporter.Path(id))
	if err != nil {
		return err
	}
	result["head"], result["commits"], result["trees"], result["blobs"] = rep.Head.String(), rep.Commits, rep.Trees, rep.Blobs
	if rep.Head != head.Commit {
		return fmt.Errorf("the export is at %s, the notebook at %s", rep.Head, head.Commit)
	}
	return nil
}
