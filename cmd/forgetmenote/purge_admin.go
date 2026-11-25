package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"forgetmenote/internal/gitexport"
	"forgetmenote/internal/instanceconfig"
	"forgetmenote/internal/notestore"
	"forgetmenote/internal/workspace"
)

// runPurgeAdmin lets an operator purge content from a notebook of the running
// instance (docs/purge.md) through its own purge routes, with the
// instance access key in DATA/admin.token, so the process that serves the
// notebook rewrites it, rebuilds its Git export and cleans what was derived
// from it, exactly as the browser flow does:
//
//   - prepare plans a purge, prints the plan and writes it, with what running
//     it needs, to a private plan file. Text to purge is read from a file or
//     stdin, never the command line, which other users and shell history see.
//   - execute runs the plan in that file with a reason, prints the receipt
//     and deletes the plan file, which held the text; it deletes it too when
//     the notebook has moved past the plan, which can then never run.
//   - list prints the notebook's purge receipts, and the purges that kept
//     none yet as unfinished.
//   - rebuild, with the instance stopped, rebuilds a notebook's Git export
//     for a purge whose receipt says the rebuild failed, or that stopped
//     before it finished, so the backup worker pushes it as that purge.
func runPurgeAdmin(args []string, input io.Reader, stdout io.Writer) error {
	usage := errors.New("usage: forgetmenote admin purge prepare|execute|list|rebuild -notebook ID [options]; see docs/purge.md")
	if len(args) > 0 && args[0] == "rebuild" {
		return runPurgeRebuild(args[1:], stdout)
	}
	if len(args) == 0 || args[0] != "prepare" && args[0] != "execute" && args[0] != "list" {
		return usage
	}
	command := args[0]
	flags := flag.NewFlagSet("admin purge "+command, flag.ContinueOnError)
	data := flags.String("data", env("DATA_DIR", "/data"), "instance data directory, which holds admin.token")
	server := flags.String("server", purgeServer(), "the running instance's local HTTP address")
	origin := flags.String("origin", env("PUBLIC_URL", ""), "the instance's public origin (PUBLIC_URL)")
	notebook := flags.String("notebook", "", "notebook ID")
	planFile := flags.String("plan", "", "plan file: written by prepare, run by execute")
	var kind, path, versions, textFile, patternFile, paths, reason, operator *string
	if command == "prepare" {
		kind = flags.String("kind", "", "note, versions or text")
		path = flags.String("path", "", "the note a note or versions purge removes")
		versions = flags.String("versions", "", "comma-separated revisions whose versions a versions purge removes")
		textFile = flags.String("text-file", "", "file holding the exact text a text purge removes, or - for stdin")
		patternFile = flags.String("pattern-file", "", "file holding the RE2 pattern a text purge removes every match of, or - for stdin")
		paths = flags.String("paths", "", "comma-separated notes a text purge covers; none covers the whole notebook")
	}
	if command == "execute" {
		reason = flags.String("reason", "", "why: kept in the receipt; never quote the purged content")
		operator = flags.String("operator", "", "who runs the purge, for the receipt")
	}
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	explicit := map[string]bool{}
	flags.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	if file := os.Getenv("INSTANCE_CONFIG_FILE"); file != "" {
		if core, err := instanceconfig.Load(file); err == nil {
			if !explicit["data"] && core.Server.Data != "" {
				*data = core.Server.Data
			}
			if !explicit["origin"] && core.Server.Origin != "" {
				*origin = core.Server.Origin
			}
		}
	}
	if flags.NArg() != 0 || !workspace.ValidNotebookID(*notebook) {
		return usage
	}
	token, err := os.ReadFile(filepath.Join(*data, "admin.token"))
	if err != nil {
		return fmt.Errorf("the instance access key: %w", err)
	}
	client, err := newPurgeClient(*server, *origin, strings.TrimSpace(string(token)), *notebook)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	switch command {
	case "list":
		raw, err := client.do(ctx, "GET", "/api/purge", nil)
		if err != nil {
			return err
		}
		_, err = stdout.Write(raw)
		return err
	case "prepare":
		if *planFile == "" {
			return errors.New("prepare needs -plan FILE, a new private file for the plan")
		}
		scope := workspace.PurgeScope{Kind: *kind, Path: *path, Versions: list(*versions), Paths: list(*paths)}
		if *textFile != "" && *patternFile != "" {
			return errors.New("give -text-file or -pattern-file, not both")
		}
		if *textFile != "" {
			if scope.Text, err = purgeInput(*textFile, input); err != nil {
				return err
			}
		}
		if *patternFile != "" {
			if scope.Pattern, err = purgeInput(*patternFile, input); err != nil {
				return err
			}
		}
		raw, err := client.do(ctx, "POST", "/api/purge/prepare", map[string]any{"scope": scope})
		if err != nil {
			return err
		}
		var plan map[string]any
		if err := json.Unmarshal(raw, &plan); err != nil {
			return err
		}
		saved, err := json.Marshal(map[string]any{"notebook": *notebook, "scope": scope, "revision": plan["revision"], "confirmation": plan["confirmation"]})
		if err != nil {
			return err
		}
		f, err := os.OpenFile(*planFile, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return fmt.Errorf("the plan file: %w", err)
		}
		_, err = f.Write(saved)
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return err
		}
		// What is printed holds neither the text nor what runs the plan.
		delete(plan, "confirmation")
		if s, ok := plan["scope"].(map[string]any); ok {
			delete(s, "text")
			delete(s, "pattern")
		}
		return json.NewEncoder(stdout).Encode(plan)
	}
	if *planFile == "" || strings.TrimSpace(*reason) == "" || strings.TrimSpace(*operator) == "" {
		return errors.New("execute needs -plan FILE from prepare, -reason and -operator")
	}
	raw, err := os.ReadFile(*planFile)
	if err != nil {
		return err
	}
	var plan struct {
		Notebook     string               `json:"notebook"`
		Scope        workspace.PurgeScope `json:"scope"`
		Revision     string               `json:"revision"`
		Confirmation string               `json:"confirmation"`
	}
	if err := json.Unmarshal(raw, &plan); err != nil || plan.Notebook != *notebook {
		return errors.New("the plan file is not a plan for this notebook")
	}
	receipt, err := client.do(ctx, "POST", "/api/purge/execute", map[string]any{"scope": plan.Scope, "revision": plan.Revision, "confirmation": plan.Confirmation, "reason": *reason, "operator": *operator})
	var refused purgeRefusal
	switch {
	case errors.As(err, &refused) && refused.status == http.StatusConflict:
		// The notebook moved past the plan, which can never run now.
		if removeErr := os.Remove(*planFile); removeErr != nil {
			return fmt.Errorf("%w; its plan file %s holds the purged text: delete it (%v)", err, *planFile, removeErr)
		}
		return fmt.Errorf("%w; its plan file %s, which held the purged text, was deleted: prepare the purge again", err, *planFile)
	case err != nil:
		return fmt.Errorf("%w; the plan file %s still holds the purged text: run execute again, or delete it", err, *planFile)
	}
	if err := os.Remove(*planFile); err != nil {
		fmt.Fprintf(os.Stderr, "the purge ran, but its plan file %s, which holds the purged text, could not be deleted: %v\n", *planFile, err)
	}
	_, err = stdout.Write(receipt)
	return err
}

// purgeServer is the local address of an instance listening on LISTEN_ADDR.
func purgeServer() string {
	listen := env("LISTEN_ADDR", ":8080")
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "http://127.0.0.1:8080"
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

func list(value string) []string {
	var out []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

// purgeInput reads the text or pattern of a text purge from a file or stdin,
// without its final line break.
func purgeInput(name string, stdin io.Reader) (string, error) {
	var raw []byte
	var err error
	if name == "-" {
		raw, err = io.ReadAll(io.LimitReader(stdin, 1<<20))
	} else {
		raw, err = os.ReadFile(name)
	}
	if err != nil {
		return "", err
	}
	text := strings.TrimSuffix(strings.TrimSuffix(string(raw), "\n"), "\r")
	if text == "" {
		return "", errors.New("the text or pattern to purge is empty")
	}
	return text, nil
}

type purgeClient struct {
	server *url.URL
	host   string
	token  string
	prefix string
	http   *http.Client
}

func newPurgeClient(server, origin, token, notebook string) (*purgeClient, error) {
	base, err := url.Parse(server)
	if err != nil || base.Host == "" || base.User != nil || base.Scheme != "http" && base.Scheme != "https" {
		return nil, errors.New("-server must be the instance's HTTP(S) address")
	}
	host := base.Host
	if origin != "" {
		public, err := url.Parse(origin)
		if err != nil || public.Host == "" {
			return nil, errors.New("-origin must be the instance's public origin")
		}
		host = public.Host
	}
	if len(token) < 32 {
		return nil, errors.New("the instance access key is missing or too short")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	// A purge rewrites history and rebuilds the export before it answers.
	return &purgeClient{server: base, host: host, token: token, prefix: "/notebooks/" + notebook,
		http: &http.Client{Transport: transport, Timeout: 30 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (c *purgeClient) do(ctx context.Context, method, path string, body any) ([]byte, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(raw)
	}
	target := *c.server
	target.Path = strings.TrimSuffix(target.Path, "/") + c.prefix + path
	request, err := http.NewRequestWithContext(ctx, method, target.String(), reader)
	if err != nil {
		return nil, err
	}
	request.Host = c.host
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(request)
	if err != nil {
		return nil, fmt.Errorf("the instance did not answer: %w", err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		var failure struct {
			Message string `json:"message"`
		}
		json.Unmarshal(raw, &failure)
		if failure.Message == "" {
			failure.Message = strings.TrimSpace(string(raw[:min(len(raw), 300)]))
		}
		return nil, purgeRefusal{response.StatusCode, failure.Message}
	}
	return raw, nil
}

// purgeRefusal is the instance's answer to a purge request it refused.
type purgeRefusal struct {
	status  int
	message string
}

func (r purgeRefusal) Error() string { return fmt.Sprintf("HTTP %d: %s", r.status, r.message) }

// runPurgeRebuild rebuilds a notebook's Git export for a purge, on a stopped
// instance, which holds no export.
func runPurgeRebuild(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("admin purge rebuild", flag.ContinueOnError)
	data := flags.String("data", env("DATA_DIR", "/data"), "stopped instance data directory")
	notebook := flags.String("notebook", "", "notebook ID")
	purge := flags.String("purge", "", "the purge ID its receipt names")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || !workspace.ValidNotebookID(*notebook) || *purge == "" {
		return errors.New("usage: forgetmenote admin purge rebuild -notebook ID -purge PURGE [-data DIR] (stop the instance first)")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	db, dir, config, err := stateDatabase(ctx, flags, *data)
	if err != nil {
		return err
	}
	defer db.Close()
	if live, err := db.LiveGateways(ctx, db.Pool); err != nil || live > 0 {
		if err == nil {
			err = fmt.Errorf("%d gateways are serving; stop the instance first", live)
		}
		return err
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
	if err := exporter.Rebuild(ctx, *notebook, *purge); err != nil {
		return err
	}
	head, err := exporter.Branch(*notebook)
	if err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(map[string]string{"notebook": *notebook, "purge": *purge, "head": head.String()})
}
