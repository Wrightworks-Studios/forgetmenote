package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/instanceconfig"
	"forgetmenote/internal/web"
)

func runAdmin(args []string, input io.Reader, output io.Writer) error {
	if len(args) == 0 {
		return errors.New("use admin bootstrap, admin credential, admin rollback-semantic-storage, admin storage export (stop the instance first) or admin purge (on the running instance)")
	}
	switch args[0] {
	case "storage":
		return runStorageAdmin(args[1:], output)
	case "purge":
		return runPurgeAdmin(args[1:], input, output)
	case "move-notebook":
		return errors.New("admin move-notebook has been removed: every notebook lives in the instance database, which each gateway serves, so none moves between notebook owners")
	case "rollback-live-storage":
		return errors.New("live edits live in the instance database; a release that keeps them in files cannot read them, and live storage rollback is retired")
	}
	flags := flag.NewFlagSet("admin "+args[0], flag.ContinueOnError)
	data := flags.String("data", env("DATA_DIR", "/data"), "stopped instance data directory")
	username := flags.String("username", "", "initial administrator username")
	passwordStdin := flags.Bool("password-stdin", false, "read the initial password from stdin")
	user := flags.String("user", "", "existing account ID")
	name := flags.String("name", "CLI", "credential name")
	projects := flags.String("projects", "", "comma-separated notebook IDs")
	all := flags.Bool("all-projects", false, "allow all notebooks authorized for this account")
	scopes := flags.String("scopes", "notes:read", "comma-separated scopes")
	audiences := flags.String("audiences", "api", "api and/or mcp")
	days := flags.Int("days", 30, "credential lifetime, 1–365 days")
	destination := flags.String("output", "", "new private credential file")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 || args[0] != "bootstrap" && args[0] != "credential" && args[0] != "rollback-storage" && args[0] != "rollback-semantic-storage" {
		if flags.NArg() == 0 && args[0] == "migrate-instance-config" && *destination != "" {
			if err := instanceconfig.Migrate(os.Getenv, *data, *destination); err != nil {
				return err
			}
			return json.NewEncoder(output).Encode(map[string]int{"instanceConfigVersion": 1})
		}
		return errors.New("invalid administrator command")
	}
	var sectioned *instanceconfig.Config
	if os.Getenv("INSTANCE_CONFIG_FILE") != "" {
		resolved, err := instanceconfig.Resolve(os.Getenv)
		if err != nil {
			return err
		}
		sectioned = &resolved.Config
		explicitData := false
		flags.Visit(func(f *flag.Flag) {
			if f.Name == "data" {
				explicitData = true
			}
		})
		if !explicitData && sectioned.Server.Data != "" {
			*data = sectioned.Server.Data
		}
	}
	if args[0] == "rollback-storage" {
		return errors.New("the catalog and sign-in state live in Postgres; a file-based release cannot read them, and storage rollback is retired (see docs/postgres.md)")
	}
	if args[0] == "rollback-semantic-storage" {
		return errors.New("semantic vectors live in Postgres; a file-based release cannot read them, and semantic storage rollback is retired (see docs/postgres.md)")
	}
	if args[0] == "bootstrap" && (!*passwordStdin || *username == "" || os.Getenv("OIDC_ISSUER_URL") != "" || os.Getenv("PROXY_SECRET_FILE") != "" || sectioned != nil && (sectioned.Identity.OIDC != nil || sectioned.Identity.Proxy != nil)) {
		return errors.New("local bootstrap needs --username and --password-stdin with external authentication disabled")
	}
	if args[0] == "credential" && (*user == "" || *destination == "") {
		return errors.New("credential issuance needs --user and --output")
	}
	browseExcluded := strings.Split(os.Getenv("BROWSE_EXCLUDE_PREFIXES"), ",")
	if sectioned != nil && sectioned.Server.BrowseExcluded != nil {
		browseExcluded = sectioned.Server.BrowseExcluded
	}
	resolved, err := instanceconfig.Resolve(os.Getenv)
	if err != nil {
		return err
	}
	ctx := context.Background()
	db, err := commandDatabase(ctx, &resolved)
	if err != nil {
		return err
	}
	defer db.Close()
	c, closeCatalog, err := openCatalog(ctx, db, resolved.Config.Server.Blobs, *data, browseExcluded)
	if err != nil {
		return err
	}
	defer closeCatalog()
	if args[0] == "bootstrap" {
		raw, e := io.ReadAll(io.LimitReader(input, 4097))
		if e != nil {
			return e
		}
		id, e := c.CreateInitialAdmin(catalog.Bootstrap, *username, strings.TrimRight(string(raw), "\r\n"))
		if e != nil {
			return e
		}
		return json.NewEncoder(output).Encode(map[string]string{"user": id})
	}
	users, _, err := c.Administration(catalog.Bootstrap)
	if err != nil {
		return err
	}
	found := false
	for _, account := range users {
		found = found || account.ID == *user && !account.Disabled && account.ID != catalog.Bootstrap
	}
	if !found {
		return errors.New("unknown account")
	}
	root, err := filepath.EvalSymlinks(filepath.Join(*data, "projects"))
	if err != nil {
		return err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return err
	}
	target, err := filepath.Abs(*destination)
	if err != nil {
		return err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(target))
	if err != nil {
		return err
	}
	target = filepath.Join(parent, filepath.Base(target))
	if target == root || strings.HasPrefix(target, root+string(os.PathSeparator)) {
		return errors.New("credentials must be stored outside notebook repositories")
	}
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	done := false
	defer func() {
		f.Close()
		if !done {
			os.Remove(target)
		}
	}()
	selected := []string{}
	if *projects != "" {
		selected = strings.Split(*projects, ",")
	}
	issued, err := web.IssueCredential(c, db, *user, web.CredentialSpec{Name: *name, Projects: selected, AllProjects: *all, Scopes: strings.Split(*scopes, ","), Audiences: strings.Split(*audiences, ","), Days: *days})
	if err != nil {
		return err
	}
	if _, err = f.WriteString(issued.Token + "\n"); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	done = true
	return json.NewEncoder(output).Encode(map[string]any{"id": issued.ID, "expires": issued.Expires, "file": target})
}
