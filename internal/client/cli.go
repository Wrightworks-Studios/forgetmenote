package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
)

func IsCommand(args []string) bool {
	return len(args) > 0 && slices.Contains([]string{"auth", "notebook", "document", "note", "search", "backup", "action", "edit", "organization", "data", "automation", "recipe", "help"}, args[0])
}

type options map[string][]string

func (o options) get(k string) string {
	if len(o[k]) > 0 {
		return o[k][len(o[k])-1]
	}
	return ""
}
func (o options) yes(k string) bool { return o.get(k) == "true" }
func parse(args []string) ([]string, options, error) {
	pos := []string{}
	opt := options{}
	booleans := []string{"json", "create", "token-stdin", "no-browser", "manage", "help", "rebase", "operator-writers-stopped"}
	values := []string{"path", "evidence", "data-access", "server", "file", "expected-revision", "source-revision", "origin", "revision", "source", "start-line", "end-line", "limit", "after", "mode", "notebook", "authority", "name", "id", "output", "editor", "to", "repository", "preview", "generation", "kind", "query", "profile", "schema-hash"}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(a, "--") {
			pos = append(pos, a)
			continue
		}
		key, value, equal := strings.Cut(strings.TrimPrefix(a, "--"), "=")
		if slices.Contains(booleans, key) {
			if equal && value != "true" && value != "false" {
				return nil, nil, fmt.Errorf("--%s expects true or false", key)
			}
			if !equal {
				value = "true"
			}
		} else if slices.Contains(values, key) {
			if !equal {
				i++
				if i == len(args) {
					return nil, nil, fmt.Errorf("--%s requires a value", key)
				}
				value = args[i]
			}
		} else {
			return nil, nil, fmt.Errorf("unknown option --%s", key)
		}
		if key != "notebook" && len(opt[key]) > 0 {
			return nil, nil, fmt.Errorf("--%s specified more than once", key)
		}
		opt[key] = append(opt[key], value)
	}
	return pos, opt, nil
}

const usage = `ForgetMeNote client

  auth login --server URL [--token-stdin | --no-browser] [--data-access read|write] [--manage]
  auth logout [--server URL]
  notebook list | create ID | status ID | map ID | refresh ID
  notebook connect-github OWNER/REPO --id ID
  notebook export ID --output FILE.bundle
  notebook authority preview ID --to forgetmenote|github [--repository OWNER/REPO]
  notebook authority apply ID --preview ID
  document list NOTEBOOK | read NOTEBOOK PATH
  note edit NOTEBOOK PATH [--editor 'code --wait']
  note write NOTEBOOK PATH --file FILE|- (--create | --expected-revision REV)
  note history NOTEBOOK [PATH]
  note restore NOTEBOOK PATH --source-revision REV --expected-revision REV
  search QUERY --notebook ID [--notebook ID] [--mode text|meaning|automatic]
  backup status NOTEBOOK | run|pause|resume|disconnect NOTEBOOK --generation N [--kind github|gdrive|dropbox]
  edit list | resume ID [--rebase]
  organization status | offboarding USER | disable USER | enable USER
  organization group | invite | service --file FILE|-
  data list | operations CONNECTION | call CONNECTION OPERATION --file FILE|-
    [--notebook ID --profile PATH --revision REV] [--schema-hash HASH]
  automation status | save|validate|install|prepare --file FILE [--revision REVISION]
  automation discover NOTEBOOK [--path PACKAGE] [--revision COMMIT]
  automation run|test JOB --id REQUEST_ID [--evidence RUN]
  automation inspect|cancel RUN | delete JOB --revision REVISION
  recipe find NOTEBOOK [--query TEXT]
  action list | NAME --file FILE|-

All client commands accept --server URL and --json. Credentials can be provided
through FORGETMENOTE_TOKEN with FORGETMENOTE_SERVER. No client command opens /data.
`

func Run(ctx context.Context, args []string, in io.Reader, out, diagnostics io.Writer) error {
	pos, opt, err := parse(args)
	if err != nil {
		return err
	}
	if len(pos) == 0 || pos[0] == "help" || opt.yes("help") {
		fmt.Fprint(out, usage)
		return nil
	}
	dir, err := configDirectory()
	if err != nil {
		return err
	}
	if pos[0] == "auth" {
		return runAuth(ctx, pos, opt, dir, in, out, diagnostics)
	}
	if len(pos) >= 2 && pos[0] == "edit" && pos[1] == "list" {
		items, e := ListEdits(filepath.Join(dir, "edits"))
		if e != nil {
			return e
		}
		return printJSON(out, items)
	}
	c, err := configuredClient(ctx, dir, opt.get("server"))
	if err != nil {
		return err
	}
	editor := editorCommand(ctx, opt, in, out, diagnostics)
	if len(pos) == 3 && pos[0] == "edit" && pos[1] == "resume" {
		if opt.yes("rebase") {
			if err = RebaseEdit(filepath.Join(dir, "edits"), pos[2]); err != nil {
				return err
			}
		}
		r, e := c.ResumeEdit(ctx, filepath.Join(dir, "edits"), pos[2], editor)
		if e != nil {
			return e
		}
		return printJSON(out, r)
	}
	if len(pos) == 4 && pos[0] == "note" && pos[1] == "edit" {
		r, e := c.Edit(ctx, pos[2], pos[3], filepath.Join(dir, "edits"), editor)
		if e != nil {
			return e
		}
		return printJSON(out, r)
	}
	if len(pos) == 3 && pos[0] == "notebook" && pos[1] == "export" {
		if opt.get("output") == "" {
			return errors.New("--output is required")
		}
		return c.Export(ctx, pos[2], opt.get("output"))
	}
	if len(pos) == 3 && pos[0] == "notebook" && pos[1] == "connect-github" {
		return c.ConnectGitHub(ctx, pos[2], opt.get("id"), opt.get("name"), opt.yes("no-browser"), diagnostics, out)
	}
	if len(pos) == 2 && pos[0] == "action" && pos[1] == "list" {
		var value any
		if err = c.Get(ctx, "/api/v1/actions", &value); err != nil {
			return err
		}
		return printJSON(out, value)
	}
	name, params, err := commandCall(pos, opt, in)
	if err != nil {
		return err
	}
	if name == "" {
		return errors.New("unknown command; run forgetmenote help")
	}
	if err = addFlagParams(name, params, opt); err != nil {
		return err
	}
	var result json.RawMessage
	if err = c.Call(ctx, name, params, &result); err != nil {
		return err
	}
	return printResult(out, name, result, opt.yes("json"))
}

// editorCommand opens a path in the user's editor. Only the editor setting is
// shell syntax; file paths are positional arguments, so neither note names nor
// server content can execute commands.
func editorCommand(ctx context.Context, opt options, in io.Reader, out, diagnostics io.Writer) func(string) error {
	return func(path string) error {
		command := opt.get("editor")
		if command == "" {
			command = os.Getenv("VISUAL")
		}
		if command == "" {
			command = os.Getenv("EDITOR")
		}
		if command == "" {
			command = "vi"
		}
		cmd := exec.CommandContext(ctx, "sh", "-c", "exec "+command+` "$@"`, "forgetmenote-editor", path)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = in, out, diagnostics
		return cmd.Run()
	}
}

// commandCall maps a server command to its tool name and parameters. An empty
// name with no error is an unknown command.
func commandCall(pos []string, opt options, in io.Reader) (string, map[string]any, error) {
	switch pos[0] {
	case "data":
		return dataCall(pos, opt, in)
	case "automation":
		return automationCall(pos, opt, in)
	case "recipe":
		if len(pos) != 3 || pos[1] != "find" {
			return "", nil, errors.New("use recipe find NOTEBOOK [--query TEXT]")
		}
		return "find_recipes", map[string]any{"project": pos[2], "query": opt.get("query")}, nil
	case "organization":
		return organizationCall(pos, opt, in)
	case "action":
		if len(pos) != 2 {
			return "", nil, errors.New("use action list or action NAME --file FILE|-")
		}
		params := map[string]any{}
		if opt.get("file") != "" {
			raw, e := inputFile(opt.get("file"), in)
			if e != nil {
				return "", nil, e
			}
			if json.Unmarshal(raw, &params) != nil || params == nil {
				return "", nil, errors.New("action input must be a JSON object")
			}
		}
		return pos[1], params, nil
	case "notebook":
		return notebookCall(pos, opt)
	case "document", "note":
		return documentCall(pos, opt, in)
	case "search":
		if len(pos) != 2 || len(opt["notebook"]) == 0 {
			return "", nil, errors.New("search requires a query and --notebook ID")
		}
		return "search_notebooks", map[string]any{"query": pos[1], "projects": opt["notebook"], "corpora": []string{"notes", "repository"}}, nil
	case "backup":
		return backupCall(pos, opt)
	}
	return "", map[string]any{}, nil
}

func dataCall(pos []string, opt options, in io.Reader) (string, map[string]any, error) {
	params := map[string]any{}
	switch {
	case len(pos) == 2 && pos[1] == "list":
		return "list_data_connections", params, nil
	case len(pos) == 3 && pos[1] == "operations":
		params["connection"] = pos[2]
		return "list_data_operations", params, nil
	case len(pos) == 4 && pos[1] == "call" && opt.get("file") != "":
		params["connection"], params["operation"] = pos[2], pos[3]
		raw, e := inputFile(opt.get("file"), in)
		if e != nil {
			return "", nil, e
		}
		var args map[string]any
		if json.Unmarshal(raw, &args) != nil || args == nil {
			return "", nil, errors.New("operation input must be a JSON object")
		}
		params["arguments"] = args
		if hash := opt.get("schema-hash"); hash != "" {
			params["schemaHash"] = hash
		}
		if opt.get("profile") != "" || opt.get("revision") != "" || opt.get("notebook") != "" {
			if opt.get("profile") == "" || opt.get("revision") == "" || len(opt["notebook"]) != 1 {
				return "", nil, errors.New("recipe provenance requires one --notebook, --profile and --revision")
			}
			params["project"], params["profilePath"], params["profileRevision"] = opt.get("notebook"), opt.get("profile"), opt.get("revision")
		}
		return "call_data_operation", params, nil
	}
	return "", nil, errors.New("use data list, operations CONNECTION, or call CONNECTION OPERATION --file FILE|-")
}

func automationCall(pos []string, opt options, in io.Reader) (string, map[string]any, error) {
	params := map[string]any{}
	switch {
	case len(pos) == 3 && pos[1] == "discover":
		params["project"] = pos[2]
		params["path"] = opt.get("path")
		params["revision"] = opt.get("revision")
		return "discover_automation_packages", params, nil
	case len(pos) == 2 && pos[1] == "status":
		return "automation_status", params, nil
	case len(pos) == 2 && (pos[1] == "save" || pos[1] == "validate" || pos[1] == "install" || pos[1] == "prepare") && opt.get("file") != "":
		raw, e := inputFile(opt.get("file"), in)
		if e != nil {
			return "", nil, e
		}
		var spec map[string]any
		if json.Unmarshal(raw, &spec) != nil || spec == nil {
			return "", nil, errors.New("job definition must be a JSON object")
		}
		name := "validate_automation"
		params["spec"] = spec
		if pos[1] == "prepare" {
			name = "prepare_automation_package"
		}
		if pos[1] == "save" || pos[1] == "install" {
			name = "save_automation"
			if pos[1] == "install" {
				name = "install_automation_package"
			}
			params["revision"] = opt.get("revision")
		}
		return name, params, nil
	case len(pos) == 3 && pos[1] == "delete" && opt.get("revision") != "":
		params["id"] = pos[2]
		params["revision"] = opt.get("revision")
		return "delete_automation", params, nil
	case len(pos) == 3 && pos[1] == "inspect":
		params["id"] = pos[2]
		return "automation_run", params, nil
	case len(pos) == 3 && (pos[1] == "run" || pos[1] == "test") && opt.get("id") != "":
		name := "run_automation"
		if pos[1] == "test" {
			name = "test_automation"
			if opt.get("evidence") != "" {
				params["evidenceRun"] = opt.get("evidence")
			}
		}
		params["id"] = pos[2]
		params["requestId"] = opt.get("id")
		return name, params, nil
	case len(pos) == 3 && pos[1] == "cancel":
		params["id"] = pos[2]
		return "cancel_automation", params, nil
	}
	return "", nil, errors.New("use automation status, discover NOTEBOOK, save|validate|install|prepare --file FILE, run|test JOB --id REQUEST_ID, inspect|cancel RUN, or delete JOB --revision REVISION")
}

func organizationCall(pos []string, opt options, in io.Reader) (string, map[string]any, error) {
	if len(pos) < 2 {
		return "", nil, errors.New("choose an organization command")
	}
	params := map[string]any{}
	switch pos[1] {
	case "status":
		if len(pos) != 2 {
			return "", nil, errors.New("use organization status")
		}
		return "organization_status", params, nil
	case "offboarding", "disable", "enable":
		if len(pos) != 3 {
			return "", nil, errors.New("provide an account ID")
		}
		if pos[1] == "offboarding" {
			params["user"] = pos[2]
			return "offboarding_status", params, nil
		}
		params["id"] = pos[2]
		params["disabled"] = pos[1] == "disable"
		return "set_user_access", params, nil
	case "group", "invite", "service":
		if len(pos) != 2 || opt.get("file") == "" {
			return "", nil, errors.New("provide --file FILE or --file -")
		}
		name := map[string]string{"group": "set_group", "invite": "create_invitation", "service": "create_service_account"}[pos[1]]
		raw, e := inputFile(opt.get("file"), in)
		if e != nil {
			return "", nil, e
		}
		if e = json.Unmarshal(raw, &params); e != nil || params == nil {
			return "", nil, errors.New("provide a JSON object")
		}
		return name, params, nil
	}
	return "", params, nil
}

func notebookCall(pos []string, opt options) (string, map[string]any, error) {
	if len(pos) < 2 {
		return "", nil, errors.New("choose a notebook command")
	}
	params := map[string]any{}
	if pos[1] == "list" && len(pos) == 2 {
		return "list_projects", params, nil
	}
	if pos[1] == "authority" {
		if len(pos) != 4 {
			return "", nil, errors.New("use notebook authority preview|apply NOTEBOOK")
		}
		params["project"] = pos[3]
		name := ""
		if pos[2] == "preview" {
			name = "preview_notebook_authority_change"
			params["to"] = opt.get("to")
			params["repository"] = opt.get("repository")
			params["operatorWritersStopped"] = opt.yes("operator-writers-stopped")
		} else if pos[2] == "apply" {
			name = "apply_notebook_authority_change"
			params["preview"] = opt.get("preview")
		}
		return name, params, nil
	}
	if len(pos) != 3 {
		return "", nil, errors.New("provide a notebook ID")
	}
	params["project"] = pos[2]
	switch pos[1] {
	case "create":
		if a := opt.get("authority"); a != "" && a != "forgetmenote" {
			return "", nil, errors.New("use notebook connect-github for GitHub authority")
		}
		title := opt.get("name")
		if title == "" {
			title = pos[2]
		}
		return "create_project", map[string]any{"id": pos[2], "name": title}, nil
	case "status":
		return "notebook_authority_status", params, nil
	case "map":
		return "notebook_map", params, nil
	case "refresh":
		return "refresh_repository_source", params, nil
	}
	return "", params, nil
}

func documentCall(pos []string, opt options, in io.Reader) (string, map[string]any, error) {
	if len(pos) < 3 {
		return "", nil, errors.New("provide a notebook ID and document command")
	}
	params := map[string]any{"project": pos[2]}
	if len(pos) > 3 {
		params["path"] = pos[3]
	}
	switch pos[1] {
	case "list":
		return "list_documents", params, nil
	case "read":
		if len(pos) != 4 {
			return "", nil, errors.New("provide a document path")
		}
		return "read_document", params, nil
	case "history":
		return "note_history", params, nil
	case "write":
		if len(pos) != 4 || opt.get("file") == "" {
			return "", nil, errors.New("provide notebook, path, and --file")
		}
		if opt.yes("create") == (opt.get("expected-revision") != "") {
			return "", nil, errors.New("choose --create or provide --expected-revision")
		}
		data, e := inputFile(opt.get("file"), in)
		if e != nil {
			return "", nil, e
		}
		params["requireCheckpoint"] = true
		params["content"] = string(data)
		params["revision"] = opt.get("expected-revision")
		return "save_note", params, nil
	case "restore":
		if len(pos) != 4 || opt.get("source-revision") == "" || opt.get("expected-revision") == "" {
			return "", nil, errors.New("restore requires path, --source-revision and --expected-revision")
		}
		params["sourceRevision"] = opt.get("source-revision")
		params["revision"] = opt.get("expected-revision")
		return "restore_note", params, nil
	}
	return "", params, nil
}

func backupCall(pos []string, opt options) (string, map[string]any, error) {
	if len(pos) != 3 {
		return "", nil, errors.New("use backup status|run|pause|resume|disconnect NOTEBOOK")
	}
	params := map[string]any{"project": pos[2]}
	if pos[1] == "status" {
		return "managed_backup_status", params, nil
	}
	if !slices.Contains([]string{"run", "pause", "resume", "disconnect"}, pos[1]) {
		return "", params, nil
	}
	action := pos[1]
	if action == "run" {
		action = "now"
	}
	params["action"] = action
	if opt.get("generation") == "" {
		return "", nil, errors.New("--generation from backup status is required")
	}
	// The GitHub binding unless --kind names a drive destination.
	if kind := opt.get("kind"); kind != "" {
		if !slices.Contains([]string{"github", "gdrive", "dropbox"}, kind) {
			return "", nil, errors.New("--kind is github, gdrive or dropbox")
		}
		params["kind"] = kind
	}
	return "control_managed_backup", params, nil
}

// addFlagParams copies the shared optional flags into a tool call.
func addFlagParams(name string, params map[string]any, opt options) error {
	for flag, field := range map[string]string{"origin": "origin", "source": "source", "revision": "revision", "mode": "mode", "after": "after", "query": "query"} {
		if name == "call_data_operation" && flag == "revision" {
			continue
		}
		if value := opt.get(flag); value != "" {
			params[field] = value
		}
	}
	for flag, field := range map[string]string{"start-line": "startLine", "end-line": "endLine", "limit": "limit", "generation": "generation"} {
		if value := opt.get(flag); value != "" {
			n, e := strconv.ParseUint(value, 10, 64)
			if e != nil {
				return fmt.Errorf("--%s requires a nonnegative integer", flag)
			}
			params[field] = n
		}
	}
	return nil
}

func printResult(out io.Writer, name string, result json.RawMessage, raw bool) error {
	if raw {
		_, err := fmt.Fprintln(out, string(result))
		return err
	}
	switch name {
	case "read_document":
		var doc struct{ Content string }
		if err := json.Unmarshal(result, &doc); err != nil {
			return err
		}
		_, err := fmt.Fprint(out, doc.Content)
		return err
	case "list_projects":
		var books []struct {
			ID, Name, Role string
			Authority      struct{ Type string }
		}
		if err := json.Unmarshal(result, &books); err != nil {
			return err
		}
		w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tAUTHORITY\tROLE\tNAME")
		for _, p := range books {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", p.ID, p.Authority.Type, p.Role, p.Name)
		}
		return w.Flush()
	}
	return printJSON(out, result)
}

func inputFile(path string, in io.Reader) ([]byte, error) {
	if path == "-" {
		return io.ReadAll(in)
	}
	return regularFile(path)
}
func printJSON(out io.Writer, value any) error {
	e := json.NewEncoder(out)
	e.SetIndent("", "  ")
	return e.Encode(value)
}
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var api *Error
	if errors.As(err, &api) {
		switch api.Status {
		case 401, 403:
			return 3
		case 409:
			return 4
		}
		if api.Status >= 500 {
			return 5
		}
		return 2
	}
	var draft *DraftError
	if errors.As(err, &draft) {
		return 4
	}
	return 1
}
func runAuth(ctx context.Context, pos []string, opt options, dir string, in io.Reader, out, diagnostics io.Writer) error {
	if len(pos) != 2 {
		return errors.New("use auth login or auth logout")
	}
	server := opt.get("server")
	if server == "" {
		server = os.Getenv("FORGETMENOTE_SERVER")
	}
	if pos[1] == "logout" {
		return withConfig(dir, func(cfg *configuration) error {
			if server == "" {
				server = cfg.Active
			}
			if session, ok := cfg.Sessions[server]; ok {
				origin, err := ValidateServer(server)
				if err != nil {
					return err
				}
				token := session.Token
				if session.Refresh != "" {
					token = session.Refresh
				}
				if err = postForm(ctx, origin+"/oauth/revoke", url.Values{"token": {token}}, nil); err != nil {
					return fmt.Errorf("could not revoke session; local credentials retained: %w", err)
				}
			}
			delete(cfg.Sessions, server)
			if cfg.Active == server {
				cfg.Active = ""
			}
			fmt.Fprintln(out, "Signed out and revoked the session.")
			return nil
		})
	}
	if pos[1] != "login" {
		return errors.New("unknown auth command")
	}
	var err error
	server, err = ValidateServer(server)
	if err != nil {
		return err
	}
	var session Session
	if opt.yes("token-stdin") {
		raw, e := io.ReadAll(io.LimitReader(in, 8193))
		if e != nil {
			return e
		}
		if len(raw) > 8192 {
			return errors.New("credential is too long")
		}
		session = Session{Server: server, Token: strings.TrimSpace(string(raw))}
	} else {
		session, err = login(ctx, server, opt.yes("no-browser"), diagnostics, opt.get("data-access"), opt.yes("manage"))
		if err != nil {
			return err
		}
	}
	c := Client{Server: server, Token: session.Token}
	if err = c.Call(ctx, "capabilities", struct{}{}, new(any)); err != nil {
		return err
	}
	if err = saveSession(dir, session); err != nil {
		return err
	}
	fmt.Fprintln(out, "Signed in to "+server)
	return nil
}
