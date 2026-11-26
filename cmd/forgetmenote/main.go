// ForgetMeNote runs one locally versioned notes workspace behind an HTTP server.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	runtimeapp "forgetmenote/internal/app"
	"forgetmenote/internal/blob"
	"forgetmenote/internal/capacity"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/client"
	"forgetmenote/internal/instanceconfig"
	"forgetmenote/internal/integrations"
	// The hidden "forgetmenote sandbox-exec" helper that confines local document
	// extractors. Its init handles that subcommand before main runs.
	_ "forgetmenote/internal/sandbox"
	"forgetmenote/internal/web"
)

func env(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
func main() {
	if err := run(); err != nil {
		if client.IsCommand(os.Args[1:]) {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(client.ExitCode(err))
		}
		log.Fatal(err)
	}
}
func run() error { return runArguments(os.Args[1:]) }
func runArguments(args []string) error {
	if len(args) > 0 && args[0] == "healthz" {
		return runHealth(context.Background(), args[1:])
	}
	if len(args) > 0 && args[0] == "plugin" {
		return runPlugin(args[1:], os.Stdout)
	}
	if len(args) > 0 && args[0] == "integrations" {
		return runIntegrations(args[1:], os.Stdout)
	}
	if err := capacity.Validate(); err != nil {
		return err
	}
	if client.IsCommand(args) {
		return client.Run(context.Background(), args, os.Stdin, os.Stdout, os.Stderr)
	}
	if len(args) > 0 && args[0] == "admin" {
		return runAdmin(args[1:], os.Stdin, os.Stdout)
	}
	if len(args) > 0 && args[0] == "migrate" {
		return runMigrate(args[1:], os.Stdout)
	}
	if len(args) > 0 && args[0] == "import-files" {
		return runImportFiles(args[1:], os.Stdout)
	}
	if len(args) > 0 && args[0] == "export-files" {
		return runExportFiles(args[1:], os.Stdout)
	}
	if len(args) > 0 && args[0] == "serve" {
		args = args[1:]
	}
	if os.Getenv("MCP_ACCESS_CONFIG_FILE") != "" {
		return errors.New("MCP_ACCESS_CONFIG_FILE has been removed; remove this gateway setting and reconnect clients to PUBLIC_URL/mcp using ForgetMeNote OAuth (see docs/automations.md)")
	}
	flags := flag.NewFlagSet("forgetmenote", flag.ContinueOnError)
	headless := flags.Bool("headless", env("HEADLESS", "false") == "true", "disable the notebook UI; keep API, MCP and authentication")

	data := flags.String("data", env("DATA_DIR", "/data"), "persistent application directory")
	listen := flags.String("listen", env("LISTEN_ADDR", ":8080"), "HTTP listen address")
	public := flags.String("url", env("PUBLIC_URL", "http://localhost:8080"), "external origin, HTTPS behind SWAG")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.Arg(0) == "token" && os.Getenv("INSTANCE_CONFIG_FILE") != "" {
		explicitData := false
		flags.Visit(func(f *flag.Flag) {
			if f.Name == "data" {
				explicitData = true
			}
		})
		if !explicitData {
			core, err := instanceconfig.Load(os.Getenv("INSTANCE_CONFIG_FILE"))
			if err != nil {
				return errors.New("cannot determine the configured recovery directory; use an explicit -data DIR token to recover from invalid instance configuration")
			}
			if core.Server.Data != "" {
				*data = core.Server.Data
			}
		}
	}
	var configured *instanceconfig.Resolved
	if flags.NArg() == 0 || os.Getenv("INSTANCE_CONFIG_FILE") != "" && (flags.Arg(0) == "grant-admin" || flags.Arg(0) == "backup") {
		resolved, err := instanceconfig.Resolve(os.Getenv)
		if err != nil {
			return err
		}
		configured = &resolved
		explicit := map[string]bool{}
		flags.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
		core := resolved.Config.Server
		if !explicit["data"] && core.Data != "" {
			*data = core.Data
		}
		if !explicit["listen"] && core.Listen != "" {
			*listen = core.Listen
		}
		if !explicit["url"] && core.Origin != "" {
			*public = core.Origin
		}
		if !explicit["headless"] && core.Headless != nil {
			*headless = *core.Headless
		}
	}
	if flags.NArg() > 0 && flags.Arg(0) == "owner" {
		return errors.New("the owner command has been removed: every notebook lives in the instance database, which each gateway serves; stop running notebook owners and remove the owner map")
	}
	if err := os.MkdirAll(*data, 0700); err != nil {
		return err
	}
	tokenPath := filepath.Join(*data, "admin.token")
	raw, err := os.ReadFile(tokenPath)
	if errors.Is(err, os.ErrNotExist) {
		raw = []byte(web.RandomToken())
		f, e := os.OpenFile(tokenPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			return e
		}
		_, err = f.Write(raw)
		closeErr := f.Close()
		if err == nil {
			err = closeErr
		}
	}
	if err != nil {
		return err
	}
	if flags.NArg() > 0 && flags.Arg(0) == "token" {
		fmt.Println(strings.TrimSpace(string(raw)))
		return nil
	}
	browseExcluded := strings.Split(os.Getenv("BROWSE_EXCLUDE_PREFIXES"), ",")
	if configured != nil && configured.Config.Server.BrowseExcluded != nil {
		browseExcluded = configured.Config.Server.BrowseExcluded
	}
	if flags.NArg() > 0 {
		if configured == nil {
			resolved, err := instanceconfig.Resolve(os.Getenv)
			if err != nil {
				return err
			}
			configured = &resolved
		}
		ctx := context.Background()
		database, err := commandDatabase(ctx, configured)
		if err != nil {
			return err
		}
		defer database.Close()
		projects, closeCatalog, err := openCatalog(ctx, database, configured.Config.Server.Blobs, *data, browseExcluded)
		if err != nil {
			return err
		}
		defer closeCatalog()
		switch flags.Arg(0) {
		case "grant-admin":
			if flags.NArg() != 2 {
				return errors.New("usage: forgetmenote -data DIR grant-admin EXISTING_USER_ID (stop the service first)")
			}
			return projects.GrantAdmin(flags.Arg(1))
		case "backup":
			if flags.NArg() != 2 {
				return errors.New("usage: forgetmenote -data DIR backup DESTINATION.bundle")
			}
			s, err := projects.Access(catalog.Bootstrap, "default", "read")
			if err != nil {
				return err
			}
			return s.Backup(context.Background(), flags.Arg(1))
		default:
			return errors.New("unknown command")
		}
	}
	// Serve never migrates: a rollout runs forgetmenote migrate first.
	database, err := requireDatabase(context.Background(), configured.Config.Server.Database)
	if err != nil {
		return err
	}
	blobs, err := blob.Open(context.Background(), configured.Config.Server.Blobs, *data)
	if err != nil {
		database.Close()
		return err
	}
	runtime, err := runtimeapp.Open(runtimeapp.Options{Data: *data, Origin: strings.TrimSuffix(*public, "/"), Token: strings.TrimSpace(string(raw)), BrowseExcluded: browseExcluded, Database: database, Blobs: blobs})
	if err != nil {
		database.Close()
		if closer, ok := blobs.(io.Closer); ok {
			closer.Close()
		}
		return err
	}
	defer runtime.Close()
	app := runtime.Server
	if err = app.ConfigureHeadless(*headless); err != nil {
		return err
	}
	if err = app.ConfigureInstance(*configured); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	server := &http.Server{Addr: *listen, Handler: runtime, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 60 * time.Second, WriteTimeout: 120 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 32 << 10}
	log.Printf("ForgetMeNote instance configuration: %+v", app.InstanceSummary())
	log.Printf("ForgetMeNote modules: %s", app.Modules())
	if omitted := integrations.Omitted(); len(omitted) > 0 {
		names := []string{}
		for _, integration := range omitted {
			names = append(names, integration.Name)
		}
		log.Printf("ForgetMeNote build leaves out optional integrations: %s", strings.Join(names, " "))
	}
	if app.OutgoingPaused() {
		log.Print("ForgetMeNote outgoing actions are paused by server.outgoing (OUTGOING): no automations, Jev reviews, repository refresh, backup expiry, background embedding or Quicknotes routing run; Slack, data writes, personal connections, backup leases and setup, and hosted extraction refuse")
	}
	log.Printf("ForgetMeNote listening at %s; public origin %s; access key in %s", *listen, *public, tokenPath)
	return runtime.Serve(ctx, server, 15*time.Second)
}
