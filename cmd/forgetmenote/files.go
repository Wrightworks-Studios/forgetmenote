package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"forgetmenote/internal/auth"
	"forgetmenote/internal/automation"
	"forgetmenote/internal/backups"
	"forgetmenote/internal/blob"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/connectors"
	"forgetmenote/internal/decision"
	"forgetmenote/internal/filestate"
	"forgetmenote/internal/instanceconfig"
	"forgetmenote/internal/knowledge"
	"forgetmenote/internal/pg"
	"forgetmenote/internal/quicknotes"
	"forgetmenote/internal/reposource"
	"forgetmenote/internal/sources"

	"github.com/jackc/pgx/v5"
)

// stateArea converts one owner package's state. Areas run in this order and
// report in it.
type stateArea struct {
	name string
	load func(context.Context, *pg.DB, pgx.Tx, *filestate.Source) (filestate.Sum, error)
	save func(context.Context, *pg.DB, pg.Querier, *filestate.Target) (filestate.Sum, error)
}

// stateAreas lists every area; personal connection grants open with the
// instance's configured key.
func stateAreas(config instanceconfig.Config) []stateArea {
	personal := connectors.PersonalFiles{Config: config.Integrations.Personal}
	return []stateArea{
		{"catalog", catalog.ImportFiles, catalog.ExportFiles},
		// Knowledge finds each notebook's directory in the catalog document.
		{"knowledge", knowledge.ImportFiles, knowledge.ExportFiles},
		{"sign-in", auth.ImportFiles, auth.ExportFiles},
		{"chat", chat.ImportFiles, chat.ExportFiles},
		{"automations", automation.ImportFiles, automation.ExportFiles},
		{"source jobs", sources.ImportFiles, sources.ExportFiles},
		{"repository sources", reposource.ImportFiles, reposource.ExportFiles},
		{"quicknotes", quicknotes.ImportFiles, quicknotes.ExportFiles},
		{"jev", decision.ImportFiles, decision.ExportFiles},
		{"personal connections", personal.ImportFiles, personal.ExportFiles},
		{"managed backups", backups.ImportFiles, backups.ExportFiles},
	}
}

// movedArea is one line of an import or export report.
type movedArea struct {
	Area string `json:"area"`
	filestate.Sum
}

// runImportFiles copies a stopped file-based instance's state into its empty
// database in one transaction, so a failed import leaves the database empty.
func runImportFiles(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("forgetmenote import-files", flag.ContinueOnError)
	data := flags.String("data", env("DATA_DIR", "/data"), "stopped file-based instance data directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: forgetmenote import-files [-data DIR] (stop every process of the instance first)")
	}
	ctx := context.Background()
	db, dir, config, err := stateDatabase(ctx, flags, *data)
	if err != nil {
		return err
	}
	defer db.Close()
	src, err := filestate.OpenSource(dir)
	if err != nil {
		return err
	}
	defer src.Close()
	// Blob files move only when blob storage is not this data directory.
	if c := config.Server.Blobs; c != nil && (c.S3 != nil || c.Directory != "" && filepath.Clean(c.Directory) != filepath.Clean(dir)) {
		store, err := openBlobs(ctx, c, dir)
		if err != nil {
			return err
		}
		defer closeBlobs(store)
		src.Blobs = store
	}
	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = db.RequireEmpty(ctx, tx); err != nil {
		return err
	}
	var areas []movedArea
	for _, area := range stateAreas(config) {
		sum, err := area.load(ctx, db, tx, src)
		if err != nil {
			return fmt.Errorf("import %s: %w", area.name, err)
		}
		areas = append(areas, movedArea{area.name, sum})
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(map[string]any{"imported": areas})
}

// runExportFiles writes the database's state in the file-based release's format
// into a new directory, from one consistent snapshot, while no gateway serves.
func runExportFiles(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("forgetmenote export-files", flag.ContinueOnError)
	data := flags.String("data", env("DATA_DIR", "/data"), "instance data directory")
	output := flags.String("output", "", "new directory for the file-based release's state")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *output == "" {
		return errors.New("usage: forgetmenote export-files -output NEW_DIRECTORY (stop every gateway first)")
	}
	ctx := context.Background()
	db, dir, config, err := stateDatabase(ctx, flags, *data)
	if err != nil {
		return err
	}
	defer db.Close()
	store, err := openBlobs(ctx, config.Server.Blobs, dir)
	if err != nil {
		return err
	}
	defer closeBlobs(store)
	tx, err := db.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if live, err := db.LiveGateways(ctx, tx); err != nil || live > 0 {
		if err == nil {
			err = fmt.Errorf("%d gateways are serving; stop them so no write follows the export", live)
		}
		return err
	}
	out, err := filestate.CreateTarget(*output)
	if err != nil {
		return err
	}
	defer out.Close()
	out.Blobs = store
	var areas []movedArea
	for _, area := range stateAreas(config) {
		sum, err := area.save(ctx, db, tx, out)
		if err != nil {
			return fmt.Errorf("export %s: %w", area.name, err)
		}
		areas = append(areas, movedArea{area.name, sum})
	}
	return json.NewEncoder(stdout).Encode(map[string]any{"exported": areas})
}

func openBlobs(ctx context.Context, c *blob.Config, data string) (blob.Store, error) {
	store, err := blob.Open(ctx, c, data)
	if err != nil {
		return nil, fmt.Errorf("blobs: %w", err)
	}
	return store, nil
}

func closeBlobs(store blob.Store) {
	if closer, ok := store.(io.Closer); ok {
		closer.Close()
	}
}

// stateDatabase opens the configured database and resolves the data directory
// as the other offline commands do.
func stateDatabase(ctx context.Context, flags *flag.FlagSet, data string) (*pg.DB, string, instanceconfig.Config, error) {
	resolved, err := instanceconfig.Resolve(os.Getenv)
	if err != nil {
		return nil, "", instanceconfig.Config{}, err
	}
	explicit := false
	flags.Visit(func(f *flag.Flag) { explicit = explicit || f.Name == "data" })
	if !explicit && resolved.Config.Server.Data != "" {
		data = resolved.Config.Server.Data
	}
	db, err := requireDatabase(ctx, resolved.Config.Server.Database)
	return db, data, resolved.Config, err
}
