package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"forgetmenote/internal/durable"
	"forgetmenote/internal/instanceconfig"
	"forgetmenote/internal/pg"
)

// openDatabase returns nil when the instance has no database configured;
// serving then refuses to start.
func openDatabase(ctx context.Context, config *instanceconfig.Database) (*pg.DB, error) {
	if config == nil {
		return nil, nil
	}
	if !filepath.IsAbs(config.URLFile) {
		return nil, errors.New("database urlFile requires an absolute private path")
	}
	url, err := durable.ReadSecret(config.URLFile, 4096)
	if err != nil {
		return nil, fmt.Errorf("database urlFile: %w", err)
	}
	return pg.Open(ctx, url, config.Org)
}

// requireDatabase opens and checks the instance database, which every command
// that reads the catalog or sign-in state needs. Only migrate changes its schema.
func requireDatabase(ctx context.Context, config *instanceconfig.Database) (*pg.DB, error) {
	db, err := openDatabase(ctx, config)
	if err != nil {
		return nil, err
	}
	if db == nil {
		return nil, errors.New("ForgetMeNote requires its Postgres database; set server.database.urlFile or DATABASE_URL_FILE (see docs/postgres.md)")
	}
	if err = db.Check(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// commandDatabase resolves the instance configuration when the command has not.
func commandDatabase(ctx context.Context, configured *instanceconfig.Resolved) (*pg.DB, error) {
	if configured == nil {
		resolved, err := instanceconfig.Resolve(os.Getenv)
		if err != nil {
			return nil, err
		}
		configured = &resolved
	}
	return requireDatabase(ctx, configured.Config.Server.Database)
}

// runMigrate applies the binary's schema migrations before a rollout. After
// every replica runs this binary, -finish-rollout raises the minimum reader so a
// rollback cannot serve rules an older binary does not enforce.
func runMigrate(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("forgetmenote migrate", flag.ContinueOnError)
	finish := flags.Bool("finish-rollout", false, "require this binary's reader version after every replica runs it")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: forgetmenote migrate [-finish-rollout]")
	}
	resolved, err := instanceconfig.Resolve(os.Getenv)
	if err != nil {
		return err
	}
	if resolved.Config.Server.Database == nil {
		return errors.New("no database is configured; set server.database.urlFile or DATABASE_URL_FILE")
	}
	ctx := context.Background()
	db, err := openDatabase(ctx, resolved.Config.Server.Database)
	if err != nil {
		return err
	}
	defer db.Close()
	applied, err := db.Migrate(ctx)
	if err != nil {
		return err
	}
	if err = db.Check(ctx); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "applied %d migrations\n", applied)
	if *finish {
		required, err := db.RequireReader(ctx)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "database requires reader version %d\n", required)
	}
	return nil
}
