package web

// MCP contract suite. MCP tool names, inputs, outputs and error classes are a
// stable contract for the agents that call them: changes are additive only.
//
// TestMCPContract drives every MCP tool whose handler reads or writes notebook
// storage through a real MCP SDK client against a real server, on a fixture
// notebook it builds through those same tools. Each scenario's calls are
// recorded, normalized and compared with testdata/mcp-contract/<scenario>.json;
// tools.json records which tools the suite calls and their input and output
// schemas. The same goldens must pass against the storage v2 backend.
//
// Regenerate after an intended change, then review the diff:
//
//	go test ./internal/web -run '^TestMCPContract$' -count=1 -update-mcp-contract
//	git diff internal/web/testdata/mcp-contract
//
// Reading a golden file or a diff:
//
//   - A file is a JSON array of steps in call order. Each step has the step
//     label, the tool, the caller ("as": admin, editor, reader, outsider or
//     readonly), the arguments sent and exactly one outcome: "result" (the
//     structured result), "error" (the tool error text with isError set) or
//     "protocolError" (a JSON-RPC error). "check" steps record an assertion the
//     test made outside MCP, such as an assembled bundle matching its sha256.
//   - Volatile values are replaced by labels numbered by first appearance
//     within one file: REVn for 40-hex Git object IDs (commit SHAs), HASHn for
//     64-hex digests, IDn for 32-hex random IDs (comment, trash, live session
//     and copied fmn-id values), USERn for pseudonymous author names, <TIME>
//     for timestamps (other layouts are spelled out, for example
//     <TIME YYYY-MM-DDThh:mm:ss.fZ>), <DATE> for dates, <MONTH> for memory
//     recheck months, <ORIGIN> for the test server's URL and <TMP> for its
//     data directory (the fixture's own path, and that path with symlinks
//     resolved). "<masked>" replaces a field whose value changes on every
//     run, such as bundle bytes. Hashes of content the test wrote and
//     fmn-ids it chose are kept verbatim, so content addressing is compared
//     exactly. Strings over 2048 characters are reduced to their length and a
//     digest of their normalized text.
//   - Labels are positional, so one extra or missing commit renumbers every
//     later REVn in that file. When most of a file shifts, look for the first
//     changed step: it is the real difference, and the rest usually follows.
//     Equal labels mean equal values: "revision": "REV7" in two steps is the
//     same commit.
//   - Scenarios share notebooks and run in order; run the whole test, never
//     a single scenario.
//   - A mismatch prints a line diff: "-" lines are the recorded golden, "+"
//     lines the current server, under "@@ golden line N @@" headers.
//   - A step whose outcome flips between result and error fails immediately
//     with the normalized response, because later steps depend on it. A
//     probed step, which nothing later depends on, shows a flip as a golden
//     difference instead.
//   - Goldens are recorded in UTC. The test sets TZ=UTC, which Git honours,
//     but Go reads the zone once per process, so a server timestamp formatted
//     in time.Local rather than UTC shows up on a non-UTC host as <TIME>
//     becoming <TIME YYYY-MM-DDThh:mm:ss±hh:mm>. That is a server change
//     (production runs in UTC); the mismatch message names the host zone.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/sources"
	"forgetmenote/internal/workspace"
)

var updateMCPContract = flag.Bool("update-mcp-contract", false, "rewrite the MCP contract golden files in testdata/mcp-contract")

const mcpContractDir = "testdata/mcp-contract"

// Tools that touch notebook storage but that this suite does not call yet.
// tools.json records each reason next to the tool.
var mcpContractDeferred = map[string]string{
	"add_task":                          "deferred: task queues need a Slack-linked task fixture; the task book is notebook content",
	"list_tasks":                        "deferred: task queues need a Slack-linked task fixture",
	"update_task":                       "deferred: task queues need a Slack-linked task fixture",
	"task_history":                      "deferred: task queues need a Slack-linked task fixture",
	"set_task_queues":                   "deferred: task queues need a Slack-linked task fixture; creates the task book notebook through the catalog",
	"assistant_context":                 "deferred: needs an enabled personal assistant fixture",
	"update_assistant_memory":           "deferred: needs an enabled personal assistant fixture",
	"save_assistant_settings":           "deferred: needs an enabled personal assistant fixture",
	"set_assistant_responsibilities":    "deferred: needs an enabled personal assistant fixture",
	"prepare_source":                    "deferred: asynchronous source jobs; archive_source covers the same commit path",
	"source_job":                        "deferred: asynchronous source jobs",
	"cancel_source_job":                 "deferred: asynchronous source jobs",
	"resume_source_job":                 "deferred: asynchronous source jobs",
	"import_source_job":                 "deferred: asynchronous source jobs; archive_source covers the same commit path",
	"import_repository":                 "deferred: repository notebooks are read-only upstream snapshots outside notebook storage v2",
	"refresh_repository_source":         "deferred: repository notebooks are read-only upstream snapshots",
	"repository_source_file":            "deferred: repository notebooks are read-only upstream snapshots",
	"repository_source_tree":            "deferred: repository notebooks are read-only upstream snapshots",
	"repository_source_map":             "deferred: repository notebooks are read-only upstream snapshots",
	"repository_source_status":          "deferred: repository notebooks are read-only upstream snapshots",
	"repository_source_changes":         "deferred: repository notebooks are read-only upstream snapshots",
	"search_repository_sources":         "deferred: repository notebooks are read-only upstream snapshots",
	"retain_repository_citation":        "deferred: repository notebooks are read-only upstream snapshots",
	"configure_repository_index":        "deferred: repository notebooks are read-only upstream snapshots",
	"set_semantic_config":               "deferred: derived index configuration; configure_semantic_search covers the engine path",
	"preview_notebook_authority_change": "deferred: authority handoff needs a GitHub connection and managed backup fixture",
	"apply_notebook_authority_change":   "deferred: authority handoff needs a GitHub connection and managed backup fixture",
	"cancel_notebook_authority_change":  "deferred: authority handoff needs a GitHub connection and managed backup fixture",
	"control_managed_backup":            "deferred: needs a configured managed backup destination and backup worker",
}

func TestMCPContract(t *testing.T) {
	// Git records the committing process's zone offset in each commit, and
	// history reports it; production containers run in UTC.
	t.Setenv("TZ", "UTC")
	run := newContractRun(t)
	run.scenario("session", contractSession)
	run.scenario("notes", contractNotes)
	run.scenario("structure", contractStructure)
	run.scenario("locks", contractLocks)
	run.scenario("comments", contractComments)
	run.scenario("attachments", contractAttachments)
	run.scenario("instructions", contractInstructions)
	run.scenario("live", contractLive)
	run.scenario("changes", contractChanges)
	run.scenario("knowledge", contractKnowledge)
	run.scenario("sources", contractSources)
	run.scenario("trash", contractTrash)
	run.scenario("memory", contractMemory)
	run.scenario("errors", contractErrors)
	run.scenario("backup", contractBackup)
	// Cases added by the storage v2 contract audit, on a notebook of their
	// own so the scenarios above keep their recording.
	run.scenario("renames", contractRenames)
	run.scenario("livereads", contractLiveReads)
	run.scenario("cursors", contractCursors)
	run.scenario("discussions", contractDiscussions)
	run.scenario("unknowns", contractUnknowns)
	run.scenario("derived", contractDerived)
	run.scenario("limits", contractLimits)
	run.scenario("export", contractExport)
	run.tools()
	run.finish()
}

func TestMCPContractHelpers(t *testing.T) {
	// A small difference is aligned: only the changed line is marked.
	if got := contractDiff("a\nb\nc\n", "a\nB\nc\n"); got != "@@ golden line 1 @@\n  a\n- b\n+ B\n  c\n  \n" {
		t.Errorf("small diff:\n%s", got)
	}
	// Differences near both ends of a large golden are listed, not aligned.
	var lines []string
	for i := range 3000 {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	want := strings.Join(lines, "\n")
	got := strings.Join(slices.Insert(slices.Insert(slices.Clone(lines), 2990, "added late"), 10, "added early"), "\n")
	diff := contractDiff(want, got)
	if !strings.HasPrefix(diff, "@@ golden lines 11-2990 differ from current lines 11-2992 (too large to align") || !strings.Contains(diff, "\n+ added early\n") || strings.Count(diff, "\n") != 81 {
		t.Errorf("large diff:\n%s", diff)
	}
	// A missing intermediate key masks nothing.
	entry := map[string]any{"result": map[string]any{"data": "kept"}}
	masked("result.missing.data", "arguments.data")(entry)
	if entry["result"].(map[string]any)["data"] != "kept" {
		t.Errorf("masked a field outside the path: %v", entry)
	}
	masked("result.data")(entry)
	if entry["result"].(map[string]any)["data"] != "<masked>" {
		t.Errorf("did not mask the path: %v", entry)
	}
}

// TestMCPContractCommitTimes checks the one spelling the goldens cannot: the
// normalizer reads Git 2.54's "Z" and production Git's "+00:00" as one UTC
// contract, and commit times must keep production's numeric offset, +00:00
// rather than Git 2.54's "Z", so production output does not change.
func TestMCPContractCommitTimes(t *testing.T) {
	t.Setenv("TZ", "UTC")
	app, _ := fixture(t)
	ts := httptest.NewServer(app)
	t.Cleanup(ts.Close)
	app.origin = ts.URL
	session := contractClient(t, app, ts.URL, catalog.Bootstrap, "notes:read notes:write notes:manage")
	call := func(tool string, args map[string]any) contractValue {
		t.Helper()
		result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: tool, Arguments: args})
		if err != nil || result.IsError {
			t.Fatalf("%s: %v %s", tool, err, contractText(result))
		}
		return contractValue{t, contractResult(t, result)}
	}
	call("create_project", map[string]any{"id": "times", "name": "Times"})
	initial := call("project_status", map[string]any{"project": "times"}).str("revision")
	call("save_note", map[string]any{"project": "times", "path": "a.md", "content": "# A\n", "revision": ""})
	times := map[string]string{
		"note_history":         call("note_history", map[string]any{"project": "times", "path": "a.md"}).str("0.authoredAt"),
		"read_project_changes": call("read_project_changes", map[string]any{"project": "times", "since": initial}).str("commits.0.authoredAt"),
	}
	for tool, at := range times {
		if _, err := time.Parse(time.RFC3339, at); err != nil || !strings.HasSuffix(at, "+00:00") {
			t.Errorf("%s authoredAt = %q, want an RFC 3339 time ending in +00:00", tool, at)
		}
	}
}

const contractNotebook = "contract"

// fmn-ids the fixture chooses; normalization keeps them verbatim.
const (
	contractChapterOneID = "c0ffee00000000000000000000000001"
	contractChapterTwoID = "c0ffee00000000000000000000000002"
	contractUnknownID    = "c0ffee000000000000000000000000ff" // no note has it
)

type contractRun struct {
	t        *testing.T
	app      *Server
	origin   string
	data     []string // the data directory's spellings, longest first
	clients  map[string]*mcp.ClientSession
	literals map[string]bool
	called   map[string]bool
	written  map[string]bool
	marks    map[string]string // revisions one scenario keeps for a later one
}

func newContractRun(t *testing.T) *contractRun {
	app, _ := fixture(t)
	ts := httptest.NewServer(app)
	t.Cleanup(ts.Close)
	app.origin = ts.URL
	r := &contractRun{t: t, app: app, origin: ts.URL, data: []string{filepath.Clean(app.data)}, clients: map[string]*mcp.ClientSession{}, literals: map[string]bool{contractChapterOneID: true, contractChapterTwoID: true, contractUnknownID: true}, called: map[string]bool{}, written: map[string]bool{}, marks: map[string]string{}}
	// Errors may spell the data directory through a symlink, such as
	// macOS's /var for /private/var.
	if resolved, err := filepath.EvalSymlinks(app.data); err == nil && resolved != r.data[0] {
		r.data = append(r.data, resolved)
		sort.Slice(r.data, func(i, j int) bool { return len(r.data[i]) > len(r.data[j]) })
	}
	for _, member := range []struct{ name, user, scope string }{
		{"admin", catalog.Bootstrap, "notes:read notes:write notes:manage"},
		{"editor", "contract-editor", "notes:read notes:write"},
		{"reader", "contract-reader", "notes:read notes:write"},
		{"outsider", "contract-outsider", "notes:read notes:write"},
		{"readonly", catalog.Bootstrap, "notes:read"},
	} {
		r.clients[member.name] = contractClient(t, app, ts.URL, member.user, member.scope)
	}
	return r
}

func contractClient(t *testing.T, app *Server, origin, user, scope string) *mcp.ClientSession {
	t.Helper()
	token := "contract-" + digest(user+scope)
	app.oauth.Mu.Lock()
	app.oauth.Access[digest(token)] = withOAuthFamily(accessGrant{Subject: user, Client: "contract-client", Scope: scope, Resource: origin + "/mcp", Expires: time.Now().Add(time.Hour).Unix(), Epoch: app.catalog.Epoch(user)})
	// Source imports recheck the grant through its refresh family.
	app.oauth.Refresh[digest(token+"-refresh")] = app.oauth.Access[digest(token)]
	app.oauth.Mu.Unlock()
	client := mcp.NewClient(&mcp.Implementation{Name: "contract", Version: "1"}, nil)
	session, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: origin + "/mcp", HTTPClient: &http.Client{Transport: attributionTransport{token: token}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

func (r *contractRun) scenario(name string, fn func(*contractScenario)) {
	sc := &contractScenario{run: r, t: r.t, name: name, norm: newContractNormalizer(r), steps: []any{}}
	fn(sc)
	if sc.unrecorded {
		r.written[name+".json"] = true
		return
	}
	r.compare(name, sc.steps)
}

// tools records the tool list: every tool's coverage, and the input and output
// schemas of the tools the suite calls.
func (r *contractRun) tools() {
	listed, err := r.clients["admin"].ListTools(r.t.Context(), nil)
	if err != nil {
		r.t.Fatal(err)
	}
	out := []any{}
	for _, tool := range listed.Tools {
		entry := map[string]any{"name": tool.Name, "readOnly": tool.Annotations != nil && tool.Annotations.ReadOnlyHint}
		switch {
		case r.called[tool.Name]:
			entry["coverage"] = "called"
			entry["inputSchema"] = contractJSON(r.t, tool.InputSchema)
			if tool.OutputSchema != nil {
				entry["outputSchema"] = contractJSON(r.t, tool.OutputSchema)
			}
		case mcpContractDeferred[tool.Name] != "":
			entry["coverage"] = mcpContractDeferred[tool.Name]
		default:
			entry["coverage"] = "not called"
		}
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].(map[string]any)["name"].(string) < out[j].(map[string]any)["name"].(string)
	})
	r.compare("tools", out)
}

func (r *contractRun) compare(name string, value any) {
	r.t.Helper()
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		r.t.Fatal(err)
	}
	got := buffer.Bytes()
	path := filepath.Join(mcpContractDir, name+".json")
	r.written[name+".json"] = true
	if *updateMCPContract {
		if err := os.MkdirAll(mcpContractDir, 0o755); err != nil {
			r.t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			r.t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		r.t.Errorf("%s: %v; record it with -update-mcp-contract", path, err)
		return
	}
	if !bytes.Equal(got, want) {
		r.t.Errorf("%s differs from the recorded contract (rerun with -update-mcp-contract and review git diff):%s\n%s", path, contractZoneNote(), contractDiff(string(want), string(got)))
	}
}

// finish reports golden files that no scenario produced, or removes them on -update-mcp-contract.
func (r *contractRun) finish() {
	entries, err := os.ReadDir(mcpContractDir)
	if err != nil {
		r.t.Fatal(err)
	}
	for _, entry := range entries {
		if r.written[entry.Name()] {
			continue
		}
		if *updateMCPContract {
			os.Remove(filepath.Join(mcpContractDir, entry.Name()))
			continue
		}
		r.t.Errorf("%s/%s is not produced by any scenario; remove it with -update-mcp-contract", mcpContractDir, entry.Name())
	}
}

// contractZoneNote explains a mismatch that may come from the host zone: Go
// formats time.Local in the zone it read at process start, whatever TZ the
// test sets later. It is empty when the process zone is UTC.
func contractZoneNote() string {
	_, winter := time.Date(2026, 1, 1, 0, 0, 0, 0, time.Local).Zone()
	_, summer := time.Date(2026, 7, 1, 0, 0, 0, 0, time.Local).Zone()
	if winter == 0 && summer == 0 {
		return ""
	}
	return fmt.Sprintf(" (this process's time zone is %s, not UTC: a <TIME> that became <TIME ...±hh:mm> is a server timestamp formatted in time.Local)", time.Local)
}

// contractDiffCells bounds the longest-common-subsequence table of a diff
// (4 bytes a cell); larger differences list lines without aligning them.
const contractDiffCells = 4 << 20

// contractDiff is a line diff of two goldens with three lines of context,
// cut off after 120 lines.
func contractDiff(want, got string) string {
	a, b := strings.Split(want, "\n"), strings.Split(got, "\n")
	prefix := 0
	for prefix < len(a) && prefix < len(b) && a[prefix] == b[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(a)-prefix && suffix < len(b)-prefix && a[len(a)-1-suffix] == b[len(b)-1-suffix] {
		suffix++
	}
	x, y := a[prefix:len(a)-suffix], b[prefix:len(b)-suffix]
	if len(x)*len(y) > contractDiffCells {
		// Too large to align: the common prefix and suffix are trimmed, so
		// the first line of each side is the first difference.
		var out strings.Builder
		fmt.Fprintf(&out, "@@ golden lines %d-%d differ from current lines %d-%d (too large to align; first 40 of each) @@\n", prefix+1, prefix+len(x), prefix+1, prefix+len(y))
		for _, l := range x[:min(len(x), 40)] {
			fmt.Fprintf(&out, "- %s\n", l)
		}
		for _, l := range y[:min(len(y), 40)] {
			fmt.Fprintf(&out, "+ %s\n", l)
		}
		return out.String()
	}
	// common[i][j] is the longest common subsequence of x[i:] and y[j:].
	common := make([][]int32, len(x)+1)
	for i := range common {
		common[i] = make([]int32, len(y)+1)
	}
	for i := len(x) - 1; i >= 0; i-- {
		for j := len(y) - 1; j >= 0; j-- {
			if x[i] == y[j] {
				common[i][j] = common[i+1][j+1] + 1
			} else {
				common[i][j] = max(common[i+1][j], common[i][j+1])
			}
		}
	}
	type line struct {
		mark byte
		text string
		at   int
	}
	var lines []line
	for i := max(0, prefix-3); i < prefix; i++ {
		lines = append(lines, line{' ', a[i], i + 1})
	}
	i, j := 0, 0
	for i < len(x) || j < len(y) {
		switch {
		case i < len(x) && j < len(y) && x[i] == y[j]:
			lines = append(lines, line{' ', x[i], prefix + i + 1})
			i, j = i+1, j+1
		case i < len(x) && (j == len(y) || common[i+1][j] >= common[i][j+1]):
			lines = append(lines, line{'-', x[i], prefix + i + 1})
			i++
		default:
			lines = append(lines, line{'+', y[j], prefix + i + 1})
			j++
		}
	}
	for k := len(a) - suffix; k < min(len(a), len(a)-suffix+3); k++ {
		lines = append(lines, line{' ', a[k], k + 1})
	}
	var out strings.Builder
	printed, gap := 0, true
	for k, l := range lines {
		near := false
		for d := max(0, k-3); d <= min(len(lines)-1, k+3); d++ {
			near = near || lines[d].mark != ' '
		}
		if !near {
			gap = true
			continue
		}
		if printed == 120 {
			out.WriteString("... diff truncated\n")
			break
		}
		if gap {
			fmt.Fprintf(&out, "@@ golden line %d @@\n", l.at)
			gap = false
		}
		fmt.Fprintf(&out, "%c %s\n", l.mark, l.text)
		printed++
	}
	return out.String()
}

type contractScenario struct {
	run   *contractRun
	t     *testing.T
	name  string
	norm  *contractNormalizer
	steps []any
	// unrecorded runs the steps without comparing them with the recording,
	// which is the full build's.
	unrecorded bool
}

// contractValue is a raw (unnormalized) tool result, for arguments of later steps.
type contractValue struct {
	t *testing.T
	v any
}

func (c contractValue) get(path string) any {
	v := c.v
	if path == "" {
		return v
	}
	for _, part := range strings.Split(path, ".") {
		switch node := v.(type) {
		case map[string]any:
			v = node[part]
		case []any:
			i, err := strconv.Atoi(part)
			if err != nil || i < 0 || i >= len(node) {
				return nil
			}
			v = node[i]
		default:
			return nil
		}
	}
	return v
}

func (c contractValue) str(path string) string {
	c.t.Helper()
	s, ok := c.get(path).(string)
	if !ok {
		raw, _ := json.Marshal(c.v)
		c.t.Fatalf("contract value %s is %T, not a string, in %s", path, c.get(path), raw)
	}
	return s
}

func (c contractValue) list(path string) []any {
	l, _ := c.get(path).([]any)
	return l
}

type contractOption func(map[string]any)

// masked replaces volatile fields before recording. Each path starts with
// arguments. or result. and continues through object keys.
func masked(paths ...string) contractOption {
	return func(entry map[string]any) {
		for _, path := range paths {
			parts := strings.Split(path, ".")
			node := entry
			for _, part := range parts[:len(parts)-1] {
				node, _ = node[part].(map[string]any)
				if node == nil {
					break
				}
			}
			if _, ok := node[parts[len(parts)-1]]; ok {
				node[parts[len(parts)-1]] = "<masked>"
			}
		}
	}
}

func (sc *contractScenario) ok(as, step, tool string, args map[string]any, options ...contractOption) contractValue {
	sc.t.Helper()
	return sc.call(as, step, tool, args, false, options...)
}

func (sc *contractScenario) fails(as, step, tool string, args map[string]any, options ...contractOption) contractValue {
	sc.t.Helper()
	return sc.call(as, step, tool, args, true, options...)
}

func (sc *contractScenario) call(as, step, tool string, args map[string]any, wantError bool, options ...contractOption) contractValue {
	sc.t.Helper()
	value, entry, failed := sc.record(as, step, tool, args, options...)
	if failed != wantError {
		recorded, _ := json.MarshalIndent(entry, "", "  ")
		sc.t.Fatalf("%s/%s: want error=%v, got:\n%s", sc.name, step, wantError, recorded)
	}
	return value
}

// record makes one call and records it as a step, whatever its outcome.
func (sc *contractScenario) record(as, step, tool string, args map[string]any, options ...contractOption) (contractValue, contractEntry, bool) {
	sc.t.Helper()
	client := sc.run.clients[as]
	if client == nil {
		sc.t.Fatalf("unknown contract caller %s", as)
	}
	sc.run.called[tool] = true
	if content, ok := args["content"].(string); ok {
		sc.run.literals[workspace.ContentHash([]byte(content))] = true
	}
	ctx, cancel := context.WithTimeout(sc.t.Context(), 30*time.Second)
	defer cancel()
	result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	var raw any
	failed := err != nil || result.IsError
	recorded := map[string]any{"arguments": contractJSON(sc.t, args)}
	if !failed {
		raw = contractResult(sc.t, result)
		recorded["result"] = contractJSON(sc.t, raw)
	}
	for _, option := range options {
		option(recorded)
	}
	// Arguments are normalized before results, so labels follow call order.
	entry := contractEntry{Step: step, Tool: tool, As: as, Arguments: sc.norm.value(recorded["arguments"])}
	switch {
	case err != nil:
		entry.ProtocolError = sc.norm.text(err.Error())
	case result.IsError:
		entry.Error = sc.norm.text(contractText(result))
	default:
		entry.Result = sc.norm.value(recorded["result"])
		if entry.Result == nil {
			entry.Result = json.RawMessage("null")
		}
	}
	sc.steps = append(sc.steps, entry)
	return contractValue{sc.t, raw}, entry, failed
}

// contractEntry is one recorded step. Exactly one of Result, Error,
// ProtocolError and Check is set.
type contractEntry struct {
	Step          string `json:"step"`
	Tool          string `json:"tool,omitempty"`
	As            string `json:"as,omitempty"`
	Arguments     any    `json:"arguments,omitempty"`
	Result        any    `json:"result,omitempty"`
	Error         string `json:"error,omitempty"`
	ProtocolError string `json:"protocolError,omitempty"`
	Check         any    `json:"check,omitempty"`
}

// check records an assertion made outside MCP.
func (sc *contractScenario) check(step string, value any) {
	sc.steps = append(sc.steps, contractEntry{Step: step, Check: sc.norm.value(contractJSON(sc.t, value))})
}

func contractText(result *mcp.CallToolResult) string {
	var parts []string
	for _, content := range result.Content {
		if text, ok := content.(*mcp.TextContent); ok {
			parts = append(parts, text.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func contractResult(t *testing.T, result *mcp.CallToolResult) any {
	t.Helper()
	if result.StructuredContent != nil {
		return contractJSON(t, result.StructuredContent)
	}
	text := contractText(result)
	var value any
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()
	if decoder.Decode(&value) != nil {
		return text
	}
	return value
}

// contractJSON is value's JSON form, with numbers kept exact.
func contractJSON(t *testing.T, value any) any {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

var (
	contractHex       = regexp.MustCompile(`\b[0-9a-f]{32,64}\b`)
	contractTimestamp = regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}([T ])\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:?\d{2}| [+-]\d{4})?\b`)
	contractZone      = regexp.MustCompile(`[+-]\d{2}(:?)\d{2}`)
	contractDate      = regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}\b`)
	contractRecheck   = regexp.MustCompile(`recheck: \d{4}-\d{2}\b`)
	contractUser      = regexp.MustCompile(`\bUser [0-9a-f]{12}\b`)
)

type contractNormalizer struct {
	run    *contractRun
	labels map[string]string
	counts map[string]int
}

func newContractNormalizer(run *contractRun) *contractNormalizer {
	return &contractNormalizer{run: run, labels: map[string]string{}, counts: map[string]int{}}
}

// value normalizes v in a fixed order (sorted object keys), so labels number
// by first appearance deterministically.
func (n *contractNormalizer) value(v any) any {
	switch node := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(node))
		for key := range node {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		out := map[string]any{}
		for _, key := range keys {
			out[key] = n.value(node[key])
			if key == "recheck" {
				if s, ok := out[key].(string); ok && s != "" {
					out[key] = "<MONTH>"
				}
			}
		}
		return out
	case []any:
		out := make([]any, len(node))
		for i, item := range node {
			out[i] = n.value(item)
		}
		return out
	case string:
		return n.text(node)
	default:
		return v
	}
}

func (n *contractNormalizer) text(s string) string {
	if n.run.origin != "" {
		s = strings.ReplaceAll(s, n.run.origin, "<ORIGIN>")
	}
	for _, dir := range n.run.data {
		s = strings.ReplaceAll(s, dir, "<TMP>")
	}
	s = contractTimestamp.ReplaceAllStringFunc(s, contractTime)
	s = contractRecheck.ReplaceAllString(s, "recheck: <MONTH>")
	s = contractDate.ReplaceAllString(s, "<DATE>")
	s = contractHex.ReplaceAllStringFunc(s, n.label)
	s = contractUser.ReplaceAllStringFunc(s, func(name string) string { return "User " + n.labelAs("USER", name) })
	if len(s) > 2048 {
		sum := workspace.ContentHash([]byte(s))
		return fmt.Sprintf("<%d chars, normalized sha256 %s>", len(s), sum[:16])
	}
	return s
}

// contractTime keeps a timestamp's format, not its value: <TIME> is RFC 3339
// UTC with whole seconds; any other layout is spelled out, such as
// <TIME YYYY-MM-DDThh:mm:ss.fZ> for fractional seconds.
func contractTime(match string) string {
	parts := contractTimestamp.FindStringSubmatch(match)
	layout := "YYYY-MM-DD" + parts[1] + "hh:mm:ss"
	if parts[2] != "" {
		layout += ".f"
	}
	// Newer Git (2.54) spells a UTC offset "Z", older Git (2.43, and production
	// images) "+00:00", so both are the same whole-second UTC contract.
	zone := parts[3]
	if zone == "+00:00" {
		zone = "Z"
	}
	layout += contractZone.ReplaceAllString(zone, "±hh${1}mm")
	if layout == "YYYY-MM-DDThh:mm:ssZ" {
		return "<TIME>"
	}
	return "<TIME " + layout + ">"
}

func (n *contractNormalizer) label(token string) string {
	if n.run.literals[token] {
		return token
	}
	switch len(token) {
	case 32:
		return n.labelAs("ID", token)
	case 40:
		return n.labelAs("REV", token)
	case 64:
		return n.labelAs("HASH", token)
	}
	return token
}

func (n *contractNormalizer) labelAs(class, token string) string {
	if label, ok := n.labels[token]; ok {
		return label
	}
	n.counts[class]++
	label := class + strconv.Itoa(n.counts[class])
	n.labels[token] = label
	return label
}

func contractPNG(t *testing.T, c color.Color) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	for x := range 2 {
		for y := range 2 {
			img.Set(x, y, c)
		}
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func contractArgs(pairs ...any) map[string]any {
	out := map[string]any{"project": contractNotebook}
	for i := 0; i+1 < len(pairs); i += 2 {
		out[pairs[i].(string)] = pairs[i+1]
	}
	return out
}

func (sc *contractScenario) head() string {
	sc.t.Helper()
	return sc.headOf(contractNotebook)
}

func (sc *contractScenario) headOf(notebook string) string {
	sc.t.Helper()
	status, err := sc.run.app.catalog.Access(catalog.Bootstrap, notebook, "read")
	if err != nil {
		sc.t.Fatal(err)
	}
	state, err := status.Status(sc.t.Context())
	if err != nil {
		sc.t.Fatal(err)
	}
	return state.Revision
}

const (
	contractIndex = "# Contract notebook\n\nStart with [Chapter one](manuscript/chapter-1.md).\n"
	contractOne   = "---\nfmn-id: " + contractChapterOneID + "\ntitle: Chapter one\ntags: [fiction, draft]\nstatus: draft\n---\n# Chapter one\n\nIshmael walked to the harbour.\n\n## The harbour\n\nThe whale appeared at dawn.\nNobody on the quay believed him.\n"
	contractTwo   = "---\nfmn-id: " + contractChapterTwoID + "\ntitle: Chapter two\n---\n# Chapter two\n\nThe captain read [the whale notes](../research/whales.md) twice.\n\n## Departure\n\nThey sailed before the whale returned.\n"
	contractWhale = "---\ntags: [research]\n---\n# Sperm whales\n\nSperm whales dive deeper than a mile. See https://example.org/whales for the survey.\n"
)

func contractSession(sc *contractScenario) {
	sc.ok("admin", "create the fixture notebook", "create_project", map[string]any{"id": contractNotebook, "name": "Contract notebook"})
	for _, member := range []struct{ user, role string }{{"contract-editor", "edit"}, {"contract-reader", "read"}} {
		if err := sc.run.app.catalog.Member(catalog.Bootstrap, contractNotebook, member.user, member.role); err != nil {
			sc.t.Fatal(err)
		}
	}
	sc.ok("admin", "list accessible notebooks", "list_projects", map[string]any{})
	sc.ok("admin", "list accessible notebooks by the notebook name", "list_notebooks", map[string]any{})
	sc.ok("editor", "an editor sees only granted notebooks", "list_projects", map[string]any{})
	sc.ok("admin", "create a second notebook by the notebook name", "create_notebook", map[string]any{"id": "contract-scratch", "name": "Scratch notebook"})
	if err := sc.run.app.catalog.SetUser(catalog.Bootstrap, catalog.User{ID: "contract-person"}); err != nil {
		sc.t.Fatal(err)
	}
	sc.ok("admin", "provision a work notebook for a person", "provision_work_notebook", map[string]any{"user": "contract-person", "id": "contract-work", "name": "Work notebook"})
	sc.ok("admin", "provisioning again is idempotent", "provision_work_notebook", map[string]any{"user": "contract-person", "id": "contract-work", "name": "Work notebook"})
	sc.fails("admin", "creating an existing notebook is refused", "create_project", map[string]any{"id": contractNotebook, "name": "Again"})
	sc.ok("admin", "notebook authority", "notebook_authority_status", contractArgs())
	sc.ok("admin", "managed backup status", "managed_backup_status", contractArgs())
	sc.ok("admin", "every notebook's managed backups", "managed_backup_overview", map[string]any{})
	sc.fails("editor", "the backup overview is for Admins", "managed_backup_overview", map[string]any{})
	sc.ok("admin", "discover capabilities", "capabilities", map[string]any{}, masked("result.instance"))
	sc.ok("reader", "a reader's capabilities", "capabilities", map[string]any{}, masked("result.instance"))
}

func contractNotes(sc *contractScenario) {
	status := sc.ok("admin", "status of the new notebook", "project_status", contractArgs())
	sc.ok("admin", "status by the notebook name", "notebook_status", contractArgs())
	sc.ok("admin", "browse the empty root", "browse_folder", contractArgs())
	sc.ok("admin", "search the empty notebook", "search_notes", contractArgs("query", ""))
	first := sc.ok("admin", "create the index", "save_note", contractArgs("path", "index.md", "content", contractIndex, "revision", ""))
	one := sc.ok("admin", "create chapter one with an fmn-id", "save_note", contractArgs("path", "manuscript/chapter-1.md", "content", contractOne, "revision", ""))
	sc.ok("admin", "create chapter two", "save_note", contractArgs("path", "manuscript/chapter-2.md", "content", contractTwo, "revision", ""))
	sc.ok("admin", "create a research note", "save_note", contractArgs("path", "research/whales.md", "content", contractWhale, "revision", ""))
	sc.fails("admin", "creating an existing note with an empty revision conflicts", "save_note", contractArgs("path", "index.md", "content", "# Replaced\n", "revision", ""))
	sc.fails("admin", "save_note refuses a non-Markdown path", "save_note", contractArgs("path", "notes/data.txt", "content", "plain", "revision", ""))
	sc.fails("admin", "save_note refuses a path outside the notebook", "save_note", contractArgs("path", "../escape.md", "content", "escape", "revision", ""))
	sc.fails("admin", "a duplicate fmn-id is refused", "save_note", contractArgs("path", "manuscript/clone.md", "content", strings.Replace(contractOne, "# Chapter one", "# Clone", 1), "revision", ""))
	read := sc.ok("admin", "read the index", "read_note", contractArgs("path", "index.md"))
	updated := strings.Replace(contractIndex, "Start with", "Begin with", 1) + "\nSee also [Chapter two](manuscript/chapter-2.md).\n"
	sc.ok("admin", "update the index with its current revision", "save_note", contractArgs("path", "index.md", "content", updated, "revision", read.str("revision")))
	sc.fails("admin", "a stale revision conflicts", "save_note", contractArgs("path", "index.md", "content", "# Stale\n", "revision", read.str("revision")))
	sc.ok("admin", "read the index at its first revision", "read_note", contractArgs("path", "index.md", "revision", first.str("revision")))
	sc.fails("admin", "a note is absent at the notebook's initial revision", "read_note", contractArgs("path", "index.md", "revision", status.str("revision")))
	sc.fails("admin", "read a missing note", "read_note", contractArgs("path", "missing.md"))
	sc.fails("admin", "read_note refuses a non-Markdown path", "read_note", contractArgs("path", "assets/cover.png"))
	sc.fails("admin", "read at an unknown revision", "read_note", contractArgs("path", "index.md", "revision", strings.Repeat("0", 40)))
	sc.ok("admin", "list every note", "search_notes", contractArgs("query", ""))
	sc.ok("admin", "search for a word", "search_notes", contractArgs("query", "whale"))
	sc.ok("admin", "filter by tag", "search_notes", contractArgs("query", "", "tag", "research"))
	sc.ok("admin", "filter by status", "search_notes", contractArgs("query", "", "status", "draft"))
	sc.ok("admin", "filter by folder", "search_notes", contractArgs("query", "", "path", "manuscript/"))
	sc.ok("admin", "browse the root", "browse_folder", contractArgs())
	sc.ok("admin", "browse a folder", "browse_folder", contractArgs("path", "manuscript"))
	sc.ok("admin", "browsing a missing folder is empty", "browse_folder", contractArgs("path", "nowhere"))
	sc.ok("admin", "count collections", "note_collections", contractArgs())
	two := sc.ok("admin", "read chapter two", "read_note", contractArgs("path", "manuscript/chapter-2.md"))
	two = sc.ok("admin", "set chapter two's properties", "set_note_properties", contractArgs("path", "manuscript/chapter-2.md", "revision", two.str("revision"), "metadata", map[string]any{"title": "Chapter two: departure", "kind": "", "status": "review", "tags": []string{"fiction"}}))
	sc.fails("admin", "set properties with a stale revision", "set_note_properties", contractArgs("path", "manuscript/chapter-2.md", "revision", read.str("revision"), "metadata", map[string]any{"title": "Stale", "kind": "", "status": "", "tags": []string{}}))
	two = sc.ok("admin", "move chapter two to Drafts", "set_note_container", contractArgs("path", "manuscript/chapter-2.md", "container", "draft", "revision", two.str("revision")))
	sc.ok("admin", "collections after the move to Drafts", "note_collections", contractArgs())
	sc.ok("admin", "search excluding drafts", "search_notes", contractArgs("query", "whale", "drafts", "exclude"))
	sc.fails("admin", "an unknown container is refused", "set_note_container", contractArgs("path", "manuscript/chapter-2.md", "container", "attic", "revision", two.str("revision")))
	sc.ok("admin", "return chapter two to Notes", "set_note_container", contractArgs("path", "manuscript/chapter-2.md", "container", "", "revision", two.str("revision")))
	sc.ok("admin", "history of one note", "note_history", contractArgs("path", "index.md"))
	sc.ok("admin", "history of a folder", "note_history", contractArgs("path", "manuscript"))
	sc.ok("admin", "history of the notebook", "note_history", contractArgs())
	current := sc.ok("admin", "read the index before restoring", "read_note", contractArgs("path", "index.md"))
	sc.ok("admin", "restore the index's first version", "restore_note", contractArgs("path", "index.md", "sourceRevision", first.str("revision"), "revision", current.str("revision")))
	sc.fails("admin", "restore with a stale revision", "restore_note", contractArgs("path", "index.md", "sourceRevision", first.str("revision"), "revision", current.str("revision")))
	sc.fails("admin", "restore without a source revision", "restore_note", contractArgs("path", "index.md", "sourceRevision", "", "revision", current.str("revision")))
	outline := sc.ok("admin", "outline of chapter one", "document_outline", contractArgs("path", "manuscript/chapter-1.md"))
	// Heading links use the heading's auto ID, here "the-harbour".
	anchor := strings.ToLower(strings.ReplaceAll(outline.str("sections.1.title"), " ", "-"))
	sc.ok("admin", "resolve a link by fmn-id", "resolve_notebook_link", contractArgs("noteId", contractChapterOneID))
	sc.ok("admin", "resolve a link to an outline heading", "resolve_notebook_link", contractArgs("noteId", contractChapterOneID, "heading", anchor))
	sc.ok("admin", "resolve a path at a historical revision", "resolve_notebook_link", contractArgs("path", "index.md", "revision", first.str("revision")))
	sc.ok("admin", "resolve a path and fmn-id at the note's first revision", "resolve_notebook_link", contractArgs("path", "manuscript/chapter-1.md", "noteId", contractChapterOneID, "revision", one.str("revision")))
	sc.ok("reader", "a reader resolves a link", "resolve_notebook_link", contractArgs("path", "manuscript/chapter-1.md"))
	sc.fails("admin", "an unknown heading is refused", "resolve_notebook_link", contractArgs("noteId", contractChapterOneID, "heading", "no-such-heading"))
	sc.fails("admin", "an unknown fmn-id is refused", "resolve_notebook_link", contractArgs("noteId", contractUnknownID))
	sc.fails("admin", "an invalid fmn-id is refused", "resolve_notebook_link", contractArgs("noteId", "not-an-fmn-id"))
	sc.fails("admin", "an fmn-id the note at that revision lacks is refused", "resolve_notebook_link", contractArgs("path", "index.md", "noteId", contractChapterOneID, "revision", first.str("revision")))
	sc.fails("admin", "a missing path is refused", "resolve_notebook_link", contractArgs("path", "missing.md"))
	sc.fails("outsider", "a non-member cannot resolve links", "resolve_notebook_link", contractArgs("noteId", contractChapterOneID))
	sc.ok("admin", "find link targets", "find_notebook_link_targets", contractArgs("query", "chapter"))
	sc.ok("admin", "every link target", "find_notebook_link_targets", contractArgs("query", ""))
	sc.ok("reader", "a reader finds link targets", "find_notebook_link_targets", contractArgs("query", "chapter"))
	sc.fails("outsider", "a non-member cannot find link targets", "find_notebook_link_targets", contractArgs("query", ""))
	sc.ok("admin", "find term mentions", "find_term_mentions", contractArgs("terms", []any{map[string]any{"name": "whale", "aliases": []string{"whales"}}}))
	sc.ok("admin", "list documents", "list_documents", contractArgs())
	sc.ok("admin", "list documents under a prefix", "list_documents", contractArgs("pathPrefix", "manuscript/"))
	sc.ok("admin", "map the notebook", "notebook_map", contractArgs())
	sc.ok("admin", "read a document", "read_document", contractArgs("path", "manuscript/chapter-1.md"))
	sc.ok("admin", "read a document by its fmn-id", "read_document", contractArgs("path", "", "noteId", contractChapterOneID))
	sc.ok("admin", "read a line range of a document", "read_document", contractArgs("path", "manuscript/chapter-1.md", "startLine", 7, "endLine", 9))
	sc.fails("admin", "read a missing document", "read_document", contractArgs("path", "missing.md"))
	sc.ok("admin", "search selected notebooks", "search_notebooks", map[string]any{"projects": []string{contractNotebook}, "query": "whale"})
	sc.ok("admin", "find recipe notes", "find_recipes", contractArgs())
}

func contractStructure(sc *contractScenario) {
	status := sc.ok("admin", "status before moving", "project_status", contractArgs())
	sc.fails("editor", "an editor cannot move notes", "move_notes", contractArgs("from", "research/whales.md", "to", "research/sperm-whales.md", "revision", status.str("revision")))
	moved := sc.ok("admin", "rename a note; links to it are rewritten", "move_notes", contractArgs("from", "research/whales.md", "to", "research/sperm-whales.md", "revision", status.str("revision")))
	sc.fails("admin", "a stale move conflicts", "move_notes", contractArgs("from", "index.md", "to", "home.md", "revision", status.str("revision")))
	sc.ok("admin", "the linking note was rewritten", "read_note", contractArgs("path", "manuscript/chapter-2.md"))
	moved = sc.ok("admin", "move a folder", "move_notes", contractArgs("from", "research", "to", "reference", "revision", moved.str("revision")))
	sc.fails("admin", "moving onto an occupied path is refused", "move_notes", contractArgs("from", "index.md", "to", "manuscript/chapter-1.md", "revision", moved.str("revision")))
	sc.fails("admin", "moving a missing note", "move_notes", contractArgs("from", "missing.md", "to", "found.md", "revision", moved.str("revision")))
	copied := sc.ok("admin", "duplicate a note; the copy gets a new fmn-id", "duplicate_notes", contractArgs("from", "manuscript/chapter-1.md", "to", "manuscript/chapter-1-copy.md", "revision", moved.str("revision")))
	sc.ok("admin", "read the copy", "read_note", contractArgs("path", "manuscript/chapter-1-copy.md"))
	sc.ok("admin", "duplicate a folder", "duplicate_notes", contractArgs("from", "reference", "to", "reference-copy", "revision", copied.str("revision")))
	sc.fails("admin", "duplicate with a stale revision", "duplicate_notes", contractArgs("from", "index.md", "to", "index-copy.md", "revision", copied.str("revision")))
	sc.ok("admin", "browse after moves", "browse_folder", contractArgs())
	sc.ok("admin", "link targets follow moves; copies get new fmn-ids", "find_notebook_link_targets", contractArgs("query", ""))
	sc.ok("admin", "history follows the renamed note", "note_history", contractArgs("path", "reference/sperm-whales.md"))
}

func contractLocks(sc *contractScenario) {
	sc.ok("admin", "no locks yet", "note_locks", contractArgs())
	status := sc.ok("admin", "status before locking", "project_status", contractArgs())
	sc.fails("editor", "an editor cannot change locks", "set_note_lock", contractArgs("path", "manuscript/chapter-2.md", "locked", true, "revision", status.str("revision")))
	locked := sc.ok("admin", "lock a note", "set_note_lock", contractArgs("path", "manuscript/chapter-2.md", "locked", true, "revision", status.str("revision")))
	sc.fails("admin", "a stale lock change conflicts", "set_note_lock", contractArgs("path", "index.md", "locked", true, "revision", status.str("revision")))
	sc.ok("admin", "list all locks", "note_locks", contractArgs())
	sc.ok("admin", "locks for the locked note", "note_locks", contractArgs("path", "manuscript/chapter-2.md"))
	note := sc.ok("admin", "read the locked note", "read_note", contractArgs("path", "manuscript/chapter-2.md"))
	sc.fails("admin", "saving a locked note is refused", "save_note", contractArgs("path", "manuscript/chapter-2.md", "content", "# Overwritten\n", "revision", note.str("revision")))
	away := sc.ok("admin", "an owner may move a locked note that needs no link edits", "move_notes", contractArgs("from", "manuscript/chapter-2.md", "to", "manuscript/chapter-two.md", "revision", locked.str("revision")))
	sc.ok("admin", "the lock moves with the note", "note_locks", contractArgs())
	sc.ok("admin", "an fmn-id link follows the renamed note", "resolve_notebook_link", contractArgs("path", "manuscript/chapter-2.md", "noteId", contractChapterTwoID))
	sc.fails("admin", "the old path alone no longer resolves", "resolve_notebook_link", contractArgs("path", "manuscript/chapter-2.md"))
	back := sc.ok("admin", "move the locked note back", "move_notes", contractArgs("from", "manuscript/chapter-two.md", "to", "manuscript/chapter-2.md", "revision", away.str("revision")))
	unlocked := sc.ok("admin", "unlock the note", "set_note_lock", contractArgs("path", "manuscript/chapter-2.md", "locked", false, "revision", back.str("revision")))
	folder := sc.ok("admin", "lock a folder", "set_note_lock", contractArgs("path", "reference-copy", "locked", true, "revision", unlocked.str("revision")))
	sc.ok("admin", "a note inherits its folder's lock", "note_locks", contractArgs("path", "reference-copy/sperm-whales.md"))
	inherited := sc.ok("admin", "read a note in the locked folder", "read_note", contractArgs("path", "reference-copy/sperm-whales.md"))
	sc.fails("admin", "saving into a locked folder is refused", "save_note", contractArgs("path", "reference-copy/sperm-whales.md", "content", "# Changed\n", "revision", inherited.str("revision")))
	sc.fails("admin", "creating a note in a locked folder is refused", "save_note", contractArgs("path", "reference-copy/new.md", "content", "# New\n", "revision", ""))
	sc.ok("admin", "unlock the folder", "set_note_lock", contractArgs("path", "reference-copy", "locked", false, "revision", folder.str("revision")))
	sc.ok("admin", "no locks remain", "note_locks", contractArgs())
}

func contractComments(sc *contractScenario) {
	discussion := sc.ok("admin", "an empty discussion", "list_comments", contractArgs("path", "manuscript/chapter-1.md"))
	sc.fails("reader", "a reader cannot comment", "add_comment", contractArgs("path", "manuscript/chapter-1.md", "text", "Reader remark", "revision", discussion.str("revision")))
	anchored := sc.ok("admin", "add an anchored comment", "add_comment", contractArgs("path", "manuscript/chapter-1.md", "text", "Is dawn right here?", "revision", discussion.str("revision"), "anchor", map[string]any{"quote": "The whale appeared at dawn.", "prefix": "", "suffix": ""}))
	thread := anchored.str("comments.0.id")
	sc.fails("admin", "a stale discussion revision conflicts", "add_comment", contractArgs("path", "manuscript/chapter-1.md", "text", "Late remark", "revision", discussion.str("revision")))
	sc.fails("admin", "an anchor that is not in the note is refused", "add_comment", contractArgs("path", "manuscript/chapter-1.md", "text", "Where?", "revision", anchored.str("revision"), "anchor", map[string]any{"quote": "This text is absent."}))
	reply := sc.ok("editor", "reply to the thread", "add_comment", contractArgs("path", "manuscript/chapter-1.md", "text", "Dawn matches chapter two.", "revision", anchored.str("revision"), "replyTo", thread))
	second := sc.ok("admin", "add a plain comment", "add_comment", contractArgs("path", "manuscript/chapter-1.md", "text", "Tighten the harbour scene.", "revision", reply.str("revision")))
	sc.fails("reader", "a reader cannot resolve", "resolve_comment", contractArgs("path", "manuscript/chapter-1.md", "id", thread, "revision", second.str("revision")))
	resolved := sc.ok("admin", "resolve the thread", "resolve_comment", contractArgs("path", "manuscript/chapter-1.md", "id", thread, "revision", second.str("revision")))
	replyID := reply.str("comments.0.replies.0.id")
	sc.fails("admin", "delete with a stale revision", "delete_comment", contractArgs("path", "manuscript/chapter-1.md", "id", thread, "replyId", replyID, "revision", second.str("revision")))
	deleted := sc.ok("admin", "delete the reply as a moderator", "delete_comment", contractArgs("path", "manuscript/chapter-1.md", "id", thread, "replyId", replyID, "revision", resolved.str("revision")))
	sc.fails("admin", "delete a missing comment", "delete_comment", contractArgs("path", "manuscript/chapter-1.md", "id", strings.Repeat("a", 32), "revision", deleted.str("revision")))
	sc.ok("admin", "the discussion after changes", "list_comments", contractArgs("path", "manuscript/chapter-1.md"))
	sc.ok("reader", "a reader reads the discussion", "list_comments", contractArgs("path", "manuscript/chapter-1.md"))
	note := sc.ok("admin", "read the note under review", "read_note", contractArgs("path", "manuscript/chapter-1.md"))
	finding := map[string]any{"key": "dawn-check", "path": "manuscript/chapter-1.md", "title": "Timing of the sighting", "text": "Chapter two says the whale returned later; check dawn.", "revision": note.str("revision"), "hash": workspace.ContentHash([]byte(note.str("content"))), "evidence": []any{map[string]any{"path": "manuscript/chapter-2.md", "explanation": "Mentions the whale's return."}}}
	filed := sc.ok("admin", "file a review finding", "file_review_finding", contractArgs("finding", finding))
	sc.ok("admin", "retrying the same finding replays it", "file_review_finding", contractArgs("finding", finding))
	stale := contractWith(finding, "hash", strings.Repeat("0", 64))
	stale["key"] = "stale-hash"
	sc.fails("admin", "a finding with a stale note hash conflicts", "file_review_finding", contractArgs("finding", stale))
	sc.ok("admin", "the review inbox lists the finding", "review_inbox", contractArgs())
	sc.ok("admin", "resolve the finding", "review_decision", contractArgs("kind", "finding", "id", filed.str("comment.id"), "path", "manuscript/chapter-1.md", "revision", filed.str("revision"), "decision", "resolve"))
	sc.fails("admin", "an unknown decision kind is refused", "review_decision", contractArgs("kind", "verdict", "id", filed.str("comment.id"), "decision", "resolve"))
}

func contractWith(in map[string]any, pairs ...any) map[string]any {
	out := map[string]any{}
	for key, value := range in {
		out[key] = value
	}
	for i := 0; i+1 < len(pairs); i += 2 {
		out[pairs[i].(string)] = pairs[i+1]
	}
	return out
}

func contractAttachments(sc *contractScenario) {
	red := contractPNG(sc.t, color.RGBA{200, 30, 30, 255})
	blue := contractPNG(sc.t, color.RGBA{30, 30, 200, 255})
	for _, data := range [][]byte{red, blue} {
		sc.run.literals[workspace.ContentHash(data)] = true
	}
	sc.ok("admin", "no attachments yet", "list_attachments", contractArgs())
	uploaded := sc.ok("admin", "upload an image", "upload_attachment", contractArgs("path", "assets/cover.png", "data", base64.StdEncoding.EncodeToString(red), "revision", ""))
	sc.fails("admin", "uploading over an existing file needs its revision", "upload_attachment", contractArgs("path", "assets/cover.png", "data", base64.StdEncoding.EncodeToString(blue), "revision", ""))
	sc.fails("admin", "an attachment outside assets/ is refused", "upload_attachment", contractArgs("path", "manuscript/cover.png", "data", base64.StdEncoding.EncodeToString(red), "revision", ""))
	sc.fails("admin", "an unsupported media type is refused", "upload_attachment", contractArgs("path", "assets/run.sh", "data", base64.StdEncoding.EncodeToString([]byte("#!/bin/sh\n")), "revision", ""))
	sc.fails("admin", "invalid base64 is refused", "upload_attachment", contractArgs("path", "assets/broken.png", "data", "not base64!", "revision", ""))
	sc.fails("reader", "a reader cannot upload", "upload_attachment", contractArgs("path", "assets/reader.png", "data", base64.StdEncoding.EncodeToString(red), "revision", ""))
	sc.ok("admin", "list attachments", "list_attachments", contractArgs())
	first := sc.ok("admin", "read the first chunk", "read_attachment", contractArgs("path", "assets/cover.png", "limit", 32))
	sc.ok("admin", "read the rest at the pinned revision", "read_attachment", contractArgs("path", "assets/cover.png", "offset", 32, "revision", first.str("revision")))
	sc.fails("admin", "a later chunk needs the revision", "read_attachment", contractArgs("path", "assets/cover.png", "offset", 32))
	sc.fails("admin", "an oversized chunk limit is refused", "read_attachment", contractArgs("path", "assets/cover.png", "limit", 262145))
	sc.fails("admin", "read a missing attachment", "read_attachment", contractArgs("path", "assets/missing.png"))
	sc.ok("admin", "replace the image with its revision", "upload_attachment", contractArgs("path", "assets/cover.png", "data", base64.StdEncoding.EncodeToString(blue), "revision", uploaded.str("revision")))
	sc.ok("admin", "read the old image at its first revision", "read_attachment", contractArgs("path", "assets/cover.png", "revision", uploaded.str("revision")))
	note := sc.ok("admin", "read chapter one before setting a cover", "read_note", contractArgs("path", "manuscript/chapter-1.md"))
	sc.ok("admin", "use the image as a cover", "set_note_properties", contractArgs("path", "manuscript/chapter-1.md", "revision", note.str("revision"), "metadata", map[string]any{"title": "Chapter one", "kind": "", "status": "draft", "tags": []string{"fiction", "draft"}, "cover": "assets/cover.png"}))
	sc.ok("admin", "browse shows the cover", "browse_folder", contractArgs("path", "manuscript"))
	restore := contractLimit(sc.t, "FMN_LIMIT_FILE_BYTES", "1024")
	sc.fails("admin", "an attachment over the file limit is refused", "upload_attachment", contractArgs("path", "assets/large.png", "data", base64.StdEncoding.EncodeToString(append(slices.Clone(red), bytes.Repeat([]byte{0}, 2048)...)), "revision", ""))
	sc.fails("admin", "a note over the file limit is refused", "save_note", contractArgs("path", "large.md", "content", "# Large\n\n"+strings.Repeat("words ", 400), "revision", ""))
	restore()
}

// contractLimit changes an operator limit until the returned function runs.
func contractLimit(t *testing.T, key, value string) func() {
	previous, had := os.LookupEnv(key)
	t.Setenv(key, value)
	return func() {
		if had {
			os.Setenv(key, previous)
		} else {
			os.Unsetenv(key)
		}
	}
}

func contractInstructions(sc *contractScenario) {
	none := sc.ok("admin", "no instructions yet", "project_instructions", contractArgs())
	revision, _ := none.get("revision").(string)
	sc.fails("editor", "an editor cannot set instructions", "set_project_instructions", contractArgs("content", "Write in British English.\n", "revision", revision))
	sc.fails("admin", "instructions cannot be written through save_note", "save_note", contractArgs("path", "AGENTS.md", "content", "Bypass.\n", "revision", ""))
	set := sc.ok("admin", "set instructions", "set_project_instructions", contractArgs("content", "Write in British English.\nNever change dialogue.\n", "revision", revision))
	sc.ok("reader", "read instructions", "project_instructions", contractArgs())
	sc.ok("reader", "read instructions by the notebook name", "notebook_instructions", contractArgs())
	sc.fails("admin", "stale instructions conflict", "set_project_instructions", contractArgs("content", "Stale.\n", "revision", revision))
	sc.ok("admin", "update instructions by the notebook name", "set_notebook_instructions", contractArgs("content", "Write in British English.\nNever change dialogue.\nKeep chapter titles.\n", "revision", set.str("revision")))
	sc.ok("admin", "the instructions file is listed with notes", "search_notes", contractArgs("query", ""))
}

func contractLive(sc *contractScenario) {
	live := sc.ok("admin", "read a note live", "read_live_note", contractArgs("path", "manuscript/chapter-1.md"))
	content := live.str("content")
	start := strings.Index(content, "dawn")
	edit := map[string]any{"start": start, "end": start + len("dawn"), "old": "dawn", "text": "dusk"}
	sc.fails("reader", "a reader cannot patch", "patch_live_note", contractArgs("path", "manuscript/chapter-1.md", "base", live.str("liveRevision"), "operationId", "contract-reader-1", "edits", []any{edit}))
	patched := sc.ok("admin", "patch one word", "patch_live_note", contractArgs("path", "manuscript/chapter-1.md", "base", live.str("liveRevision"), "operationId", "contract-op-1", "edits", []any{edit}))
	sc.ok("admin", "an identical retry replays", "patch_live_note", contractArgs("path", "manuscript/chapter-1.md", "base", live.str("liveRevision"), "operationId", "contract-op-1", "edits", []any{edit}))
	sc.fails("admin", "reusing an operation ID for different edits conflicts", "patch_live_note", contractArgs("path", "manuscript/chapter-1.md", "base", live.str("liveRevision"), "operationId", "contract-op-1", "edits", []any{map[string]any{"start": start, "end": start + len("dawn"), "old": "dawn", "text": "noon"}}))
	sc.fails("admin", "an overlapping edit from the old base conflicts", "patch_live_note", contractArgs("path", "manuscript/chapter-1.md", "base", live.str("liveRevision"), "operationId", "contract-op-2", "edits", []any{map[string]any{"start": start, "end": start + len("dawn"), "old": "dawn", "text": "night"}}))
	sc.fails("admin", "old text that does not match is refused", "patch_live_note", contractArgs("path", "manuscript/chapter-1.md", "base", patched.str("liveRevision"), "operationId", "contract-op-3", "edits", []any{map[string]any{"start": 0, "end": 3, "old": "xyz", "text": "abc"}}))
	harbour := strings.Index(content, "Nobody")
	sc.ok("editor", "a disjoint edit from the old base rebases", "patch_live_note", contractArgs("path", "manuscript/chapter-1.md", "base", live.str("liveRevision"), "operationId", "contract-op-4", "edits", []any{map[string]any{"start": harbour, "end": harbour + len("Nobody"), "old": "Nobody", "text": "No one"}}))
	sc.ok("admin", "Git still has the committed text", "read_note", contractArgs("path", "manuscript/chapter-1.md"))
	sc.ok("admin", "live text is pending", "read_live_note", contractArgs("path", "manuscript/chapter-1.md"))
	sc.ok("admin", "checkpoint the live edits", "checkpoint_live_note", contractArgs("path", "manuscript/chapter-1.md"))
	sc.ok("admin", "an unchanged checkpoint makes no commit", "checkpoint_live_note", contractArgs("path", "manuscript/chapter-1.md"))
	sc.ok("admin", "Git has the checkpointed text", "read_note", contractArgs("path", "manuscript/chapter-1.md"))
	sc.ok("admin", "history records the checkpoint", "note_history", contractArgs("path", "manuscript/chapter-1.md"))
	sc.fails("admin", "read a missing note live", "read_live_note", contractArgs("path", "missing.md"))
	sc.fails("reader", "a reader cannot checkpoint", "checkpoint_live_note", contractArgs("path", "manuscript/chapter-1.md"))
}

func contractChanges(sc *contractScenario) {
	history := sc.ok("admin", "notebook history", "note_history", contractArgs())
	commits := history.list("")
	if len(commits) < 3 {
		sc.t.Fatalf("history has %d commits", len(commits))
	}
	oldest := contractValue{sc.t, commits[len(commits)-1]}.str("revision")
	middle := contractValue{sc.t, commits[len(commits)/2]}.str("revision")
	sc.ok("admin", "changes since the oldest commit", "read_project_changes", contractArgs("since", oldest))
	sc.ok("admin", "changes between two commits", "read_notebook_changes", contractArgs("since", oldest, "revision", middle))
	sc.ok("admin", "a snapshot's metadata", "read_project_changes", contractArgs("snapshot", true))
	sc.ok("admin", "a historical snapshot's metadata", "read_project_changes", contractArgs("snapshot", true, "revision", middle))
	sc.ok("admin", "changes since an unknown commit ask for a fresh review", "read_project_changes", contractArgs("since", strings.Repeat("1", 40)))
	checkpoint := sc.ok("admin", "a new consumer has no checkpoint", "consumer_checkpoint", contractArgs("consumer", "contract-review"))
	previous, _ := checkpoint.get("checkpoint.revision").(string)
	head := sc.head()
	sc.ok("admin", "complete a workflow at the head", "complete_workflow", contractArgs("completion", map[string]any{"consumer": "contract-review", "previous": previous, "revision": head, "summary": "Reviewed every chapter.", "reset": false}))
	sc.fails("admin", "completing from a stale checkpoint conflicts", "complete_workflow", contractArgs("completion", map[string]any{"consumer": "contract-review", "previous": previous, "revision": head, "summary": "Again.", "reset": false}))
	sc.ok("admin", "the consumer's checkpoint and changes since it", "consumer_checkpoint", contractArgs("consumer", "contract-review"))
	two := sc.ok("admin", "read chapter two for a proposal", "read_note", contractArgs("path", "manuscript/chapter-2.md"))
	twoHash := workspace.ContentHash([]byte(two.str("content")))
	proposed := strings.Replace(two.str("content"), "twice", "three times", 1)
	group := map[string]any{"id": "contract-proposal", "summary": "Count the readings", "edits": []any{map[string]any{"path": "manuscript/chapter-2.md", "content": proposed, "revision": two.str("revision"), "hash": twoHash}}}
	sc.fails("reader", "a reader cannot propose", "submit_change_group", contractArgs("mode", "proposal", "group", group))
	proposal := sc.ok("admin", "propose a change group", "submit_change_group", contractArgs("mode", "proposal", "group", group))
	sc.ok("admin", "the inbox lists the proposal", "review_inbox", contractArgs())
	sc.ok("admin", "accept the proposal", "review_decision", contractArgs("kind", "proposal", "id", proposal.str("id"), "decision", "accept"))
	sc.ok("admin", "accepting again is idempotent", "review_decision", contractArgs("kind", "proposal", "id", proposal.str("id"), "decision", "accept"))
	sc.ok("admin", "chapter two after acceptance", "read_note", contractArgs("path", "manuscript/chapter-2.md"))
	staleGroup := map[string]any{"id": "contract-stale", "summary": "Stale apply", "edits": []any{map[string]any{"path": "manuscript/chapter-2.md", "content": "# Stale\n", "revision": two.str("revision"), "hash": twoHash}}}
	sc.fails("admin", "applying from a stale base conflicts", "submit_change_group", contractArgs("mode", "apply", "group", staleGroup))
	index := sc.ok("admin", "read the index for a group", "read_note", contractArgs("path", "index.md"))
	apply := map[string]any{"id": "contract-apply", "summary": "Add an epilogue and link it", "edits": []any{
		map[string]any{"path": "index.md", "content": index.str("content") + "\nRead the [epilogue](epilogue.md).\n", "revision": index.str("revision"), "hash": workspace.ContentHash([]byte(index.str("content")))},
		map[string]any{"path": "epilogue.md", "content": "# Epilogue\n\nThe whale was never seen again.\n", "revision": "", "hash": ""},
	}}
	sc.fails("editor", "an editor cannot apply deletions", "submit_change_group", contractArgs("mode", "apply", "group", map[string]any{"id": "contract-delete", "summary": "Delete the index", "edits": []any{map[string]any{"path": "index.md", "content": "", "revision": index.str("revision"), "hash": workspace.ContentHash([]byte(index.str("content"))), "delete": true}}}))
	sc.ok("editor", "apply a change group as one commit", "submit_change_group", contractArgs("mode", "apply", "group", apply))
	sc.ok("editor", "retrying the same group replays it", "submit_change_group", contractArgs("mode", "apply", "group", apply))
	sc.fails("admin", "an unknown mode is refused", "submit_change_group", contractArgs("mode", "merge", "group", apply))
	sc.ok("admin", "history shows the grouped commit", "note_history", contractArgs("path", "epilogue.md"))
}

func contractKnowledge(sc *contractScenario) {
	sc.ok("admin", "knowledge status", "knowledge_status", contractArgs())
	settings := sc.ok("admin", "retrieval settings", "get_retrieval_settings", contractArgs())
	value := settings.get("settings")
	sc.fails("editor", "an editor cannot change retrieval settings", "set_retrieval_settings", contractArgs("settings", value, "expectedId", settings.str("id")))
	changed := contractJSON(sc.t, value).(map[string]any)
	changed["search"].(map[string]any)["drafts"] = "exclude"
	sc.ok("admin", "set retrieval settings", "set_retrieval_settings", contractArgs("settings", changed, "expectedId", settings.str("id")))
	sc.fails("admin", "stale retrieval settings conflict", "set_retrieval_settings", contractArgs("settings", value, "expectedId", settings.str("id")))
	sc.ok("admin", "hybrid search", "hybrid_search", contractArgs("query", map[string]any{"text": "whale"}))
	sc.ok("admin", "literal text search", "hybrid_search", contractArgs("query", map[string]any{"text": "appeared at", "mode": "text"}))
	sc.ok("admin", "hybrid search filtered by path", "hybrid_search", contractArgs("query", map[string]any{"text": "whale", "path": "manuscript/"}))
	sc.fails("admin", "hybrid search needs text", "hybrid_search", contractArgs("query", map[string]any{}))
	sc.fails("admin", "semantic search without an embedding endpoint or vector", "semantic_search", contractArgs("query", map[string]any{"text": "whale"}))
	sc.fails("editor", "an editor cannot configure semantic search", "configure_semantic_search", contractArgs("model", "contract-fixture-v1", "dimensions", 3))
	config := sc.ok("admin", "configure agent-supplied embeddings", "configure_semantic_search", contractArgs("model", "contract-fixture-v1", "dimensions", 3))
	pending := sc.ok("admin", "passages missing embeddings", "get_indexing_passages", contractArgs())
	vectors := []any{}
	for i, passage := range pending.list("passages") {
		p := contractValue{sc.t, passage}
		vector := []float64{0, 0, 0}
		vector[i%3] = 1
		if strings.Contains(p.str("text"), "whale") {
			vector = []float64{0.9, 0.1, 0}
		}
		vectors = append(vectors, map[string]any{"id": p.str("id"), "hash": p.str("hash"), "noteHash": p.str("noteHash"), "vector": vector})
	}
	submission := map[string]any{"revision": pending.str("status.revision"), "configId": pending.str("status.configId"), "vectors": vectors}
	sc.fails("reader", "a reader cannot submit embeddings", "submit_embeddings", contractArgs("submission", submission))
	sc.ok("admin", "submit embeddings", "submit_embeddings", contractArgs("submission", submission))
	sc.fails("admin", "embeddings with the wrong dimensions are refused", "submit_embeddings", contractArgs("submission", contractWith(submission, "vectors", []any{contractWith(vectors[0].(map[string]any), "vector", []float64{1, 0})})))
	sc.ok("admin", "knowledge status after indexing", "knowledge_status", contractArgs())
	sc.ok("admin", "semantic search by vector", "semantic_search", contractArgs("query", map[string]any{"vector": []float64{1, 0, 0}, "configId": config.str("configId")}))
	sc.ok("admin", "meaning search by vector", "hybrid_search", contractArgs("query", map[string]any{"text": "whale", "mode": "meaning", "vector": []float64{1, 0, 0}, "configId": config.str("configId")}))
	sc.ok("admin", "sources and backlinks of a note", "get_note_sources", contractArgs("path", "manuscript/chapter-2.md"))
	sc.ok("admin", "backlinks of a linked note", "get_note_sources", contractArgs("path", "reference/sperm-whales.md"))
	sc.fails("admin", "sources of a missing note", "get_note_sources", contractArgs("path", "missing.md"))
}

func contractSources(sc *contractScenario) {
	// A build that leaves out source integrations offers fewer formats than
	// the recorded contract lists (docs/integrations.md).
	sc.unrecorded = len(sources.Formats()) != len(workspace.SourceFormats())
	sc.ok("admin", "supported source formats", "source_formats", contractArgs())
	text := "Harbour log, morning watch.\nA large whale sighted off the quay.\n"
	sc.run.literals[workspace.ContentHash([]byte(text))] = true
	archived := sc.ok("admin", "archive an uploaded text source", "archive_source", contractArgs("filename", "harbour-log.txt", "title", "Harbour log", "data", base64.StdEncoding.EncodeToString([]byte(text))))
	path := archived.str("path")
	sc.ok("admin", "read the source note", "read_note", contractArgs("path", path))
	sc.ok("admin", "verify the archived source", "verify_source", contractArgs("path", path))
	sc.ok("admin", "read the archived original", "read_source_original", contractArgs("path", archived.str("metadata.source.original"), "revision", archived.str("revision")))
	sc.fails("admin", "read_source_original reads only archived originals", "read_source_original", contractArgs("path", path))
	sc.fails("admin", "refreshing a source with a stale revision conflicts", "archive_source", contractArgs("path", path, "revision", strings.Repeat("2", 40), "filename", "harbour-log.txt", "data", base64.StdEncoding.EncodeToString([]byte(text+"Later entry.\n"))))
	sc.fails("reader", "a reader cannot archive sources", "archive_source", contractArgs("filename", "reader.txt", "data", base64.StdEncoding.EncodeToString([]byte("Reader source.\n"))))
	sc.fails("admin", "verify a note that is not a source", "verify_source", contractArgs("path", "index.md"))
	// The page waiting for an agent's text is the scratch notebook's only
	// source. Two sources in one notebook would list in the order of their
	// random IDs, and later scenarios list the contract notebook's sources.
	scratch := func(pairs ...any) map[string]any {
		return contractArgs(append([]any{"project", "contract-scratch"}, pairs...)...)
	}
	blank := sc.ok("admin", "archive a page without text", "archive_source", scratch("filename", "notice.html", "title", "Harbour notice", "data", base64.StdEncoding.EncodeToString([]byte("<p></p>"))))
	waiting := sc.ok("admin", "list the sources awaiting text", "list_sources_awaiting_text", scratch())
	agentText := map[string]any{"unit": "page", "sections": []map[string]any{{"number": 1, "title": "Notice", "text": "No whaling within the harbour."}}}
	sc.ok("admin", "give the page an agent's text", "archive_source", scratch("path", blank.str("path"), "revision", waiting.str("revision"), "filename", "notice.html", "data", base64.StdEncoding.EncodeToString([]byte("<p></p>")), "extractor", "Contract agent; contract-model", "text", agentText))
	sc.ok("admin", "no source awaits text", "list_sources_awaiting_text", scratch())
	sc.fails("admin", "agent text needs the original", "archive_source", scratch("filename", "notice.png", "extractor", "Contract agent; contract-model", "text", agentText))
}

func contractTrash(sc *contractScenario) {
	sc.ok("admin", "an empty trash", "list_trash", contractArgs())
	status := sc.ok("admin", "status before trashing", "project_status", contractArgs())
	sc.fails("editor", "an editor cannot trash notes", "trash_notes", contractArgs("paths", []string{"manuscript/chapter-1-copy.md"}, "revision", status.str("revision")))
	trashed := sc.ok("admin", "trash two notes", "trash_notes", contractArgs("paths", []string{"manuscript/chapter-1-copy.md", "reference-copy/sperm-whales.md"}, "revision", status.str("revision")))
	sc.fails("admin", "trashing with a stale revision conflicts", "trash_notes", contractArgs("paths", []string{"epilogue.md"}, "revision", status.str("revision")))
	sc.fails("admin", "trashing a missing note", "trash_notes", contractArgs("paths", []string{"missing.md"}, "revision", trashed.str("revision")))
	listing := sc.ok("admin", "list the trash", "list_trash", contractArgs())
	sc.ok("admin", "ordinary search excludes Trash", "search_notes", contractArgs("query", "harbour"))
	sc.ok("admin", "search including Trash", "search_notes", contractArgs("query", "harbour", "includeTrash", true))
	sc.fails("admin", "a trashed note cannot be read at its old path", "read_note", contractArgs("path", "manuscript/chapter-1-copy.md"))
	var copyID, copyPath, whaleID string
	for _, item := range listing.list("entries") {
		v := contractValue{sc.t, item}
		switch v.str("originalPath") {
		case "manuscript/chapter-1-copy.md":
			copyID, copyPath = v.str("id"), v.str("path")
		case "reference-copy/sperm-whales.md":
			whaleID = v.str("id")
		}
	}
	if copyID == "" || whaleID == "" {
		raw, _ := json.Marshal(listing.v)
		sc.t.Fatalf("trash listing lacks the trashed notes: %s", raw)
	}
	sc.fails("admin", "a link into Trash does not resolve", "resolve_notebook_link", contractArgs("path", copyPath))
	sc.fails("admin", "the instructions file does not resolve as a link", "resolve_notebook_link", contractArgs("path", "AGENTS.md"))
	sc.ok("admin", "link targets exclude Trash and instructions", "find_notebook_link_targets", contractArgs("query", ""))
	restored := sc.ok("admin", "restore one note to its original path", "restore_trash", contractArgs("id", whaleID, "revision", listing.str("revision")))
	sc.fails("admin", "restoring with a stale revision conflicts", "restore_trash", contractArgs("id", copyID, "revision", listing.str("revision")))
	sc.fails("admin", "restoring onto an occupied path is refused", "restore_trash", contractArgs("id", copyID, "to", "index.md", "revision", restored.str("revision")))
	sc.fails("admin", "restore an unknown item", "restore_trash", contractArgs("id", strings.Repeat("b", 32), "revision", restored.str("revision")))
	preview := sc.ok("admin", "preview emptying", "list_trash", contractArgs())
	sc.fails("admin", "a wrong confirmation is refused", "empty_trash", contractArgs("revision", preview.str("revision"), "confirmation", "yes"))
	sc.fails("admin", "a stale preview is refused", "empty_trash", contractArgs("revision", listing.str("revision"), "confirmation", listing.str("confirmation")))
	sc.ok("admin", "empty the trash", "empty_trash", contractArgs("revision", preview.str("revision"), "confirmation", preview.str("confirmation")))
	sc.ok("admin", "the trash is empty", "list_trash", contractArgs())
	sc.ok("admin", "history keeps the emptied note", "note_history", contractArgs("path", "manuscript/chapter-1-copy.md"))
}

func contractMemory(sc *contractScenario) {
	sc.ok("admin", "nothing remembered yet", "recall_memory", map[string]any{})
	sc.ok("admin", "remember an agent observation", "remember", map[string]any{"items": []any{map[string]any{"text": "The contract suite records MCP behaviour.", "kind": "fact"}}})
	saved := sc.ok("admin", "remember user-directed items", "remember", map[string]any{"userDirected": true, "items": []any{
		map[string]any{"text": "Prefer British spelling in manuscripts.", "kind": "preference"},
		map[string]any{"text": "Chapter titles are fixed once reviewed.", "kind": "decision", "anchor": "manuscript/chapter-1.md"},
	}})
	sc.ok("admin", "remember project memory", "remember", map[string]any{"userDirected": true, "projectKey": "https://example.org/contract.git", "items": []any{map[string]any{"text": "The fixture notebook is called contract.", "kind": "fact"}}})
	sc.ok("admin", "remembering an item again reports a duplicate", "remember", map[string]any{"userDirected": true, "items": []any{map[string]any{"text": "Prefer British spelling in manuscripts.", "kind": "preference"}}})
	sc.fails("admin", "a credential is refused", "remember", map[string]any{"userDirected": true, "items": []any{map[string]any{"text": "Token ghp_0123456789abcdefghijklmnopqrstuvwxyzAB works.", "kind": "fact"}}})
	recall := sc.ok("admin", "recall everything", "recall_memory", map[string]any{"projectKey": "https://example.org/contract.git"})
	sc.ok("admin", "recall by query", "recall_memory", map[string]any{"query": "spelling in manuscripts"})
	id := ""
	for _, item := range recall.list("core.items") {
		if v := (contractValue{sc.t, item}); strings.Contains(v.str("text"), "British") {
			id = v.str("id")
		}
	}
	sc.ok("admin", "confirm an item holds", "confirm_memory", map[string]any{"id": id, "result": "holds", "userDirected": true})
	sc.ok("admin", "replace an item", "confirm_memory", map[string]any{"id": id, "result": "changed", "replacement": "Prefer British spelling everywhere.", "reason": "Asked for it in every note.", "userDirected": true})
	sc.fails("admin", "confirm an unknown item", "confirm_memory", map[string]any{"id": "m0000000", "result": "holds", "userDirected": true})
	sc.ok("admin", "recall after the replacement", "recall_memory", map[string]any{})
	notebook := saved.str("notebook")
	sc.ok("admin", "the memory file is a note", "read_note", map[string]any{"project": notebook, "path": "memory.md"})
}

func contractErrors(sc *contractScenario) {
	sc.ok("reader", "a reader reads a note", "read_note", contractArgs("path", "index.md"))
	index := sc.ok("admin", "read the index", "read_note", contractArgs("path", "index.md"))
	sc.fails("reader", "a reader cannot save", "save_note", contractArgs("path", "index.md", "content", "# Reader\n", "revision", index.str("revision")))
	sc.fails("readonly", "a read-only credential cannot save", "save_note", contractArgs("path", "index.md", "content", "# Read-only\n", "revision", index.str("revision")))
	sc.fails("outsider", "a non-member cannot read", "read_note", contractArgs("path", "index.md"))
	sc.fails("outsider", "a non-member cannot see status", "project_status", contractArgs())
	sc.fails("admin", "an unknown notebook", "read_note", map[string]any{"project": "missing-notebook", "path": "index.md"})
	sc.fails("admin", "an unknown argument is refused", "read_note", contractArgs("path", "index.md", "unexpected", true))
	sc.fails("admin", "a missing required argument is refused", "save_note", map[string]any{"project": contractNotebook, "content": "x", "revision": ""})
	sc.fails("admin", "a wrongly typed argument is refused", "read_note", contractArgs("path", 7))
	sc.fails("admin", "a stale authority generation conflicts", "save_note", contractArgs("path", "index.md", "content", "# Generation\n", "revision", index.str("revision"), "authorityGeneration", 999))
	sc.fails("admin", "a malformed revision conflicts", "save_note", contractArgs("path", "index.md", "content", "# Malformed\n", "revision", "not-a-revision"))
	sc.fails("readonly", "a read-only credential cannot upload", "upload_attachment", contractArgs("path", "assets/readonly.png", "data", base64.StdEncoding.EncodeToString([]byte("x")), "revision", ""))
}

func contractBackup(sc *contractScenario) {
	status := sc.ok("admin", "status before export", "project_status", contractArgs())
	sc.fails("outsider", "a non-member cannot export", "export_project_backup", contractArgs())
	first := sc.ok("admin", "export the first chunk", "export_project_backup", contractArgs("limit", 4096), masked("result.data", "result.total"))
	data, err := base64.StdEncoding.DecodeString(first.str("data"))
	if err != nil {
		sc.t.Fatal(err)
	}
	digest := first.str("sha256")
	total, _ := strconv.ParseInt(string(first.get("total").(json.Number)), 10, 64)
	sc.fails("admin", "a later chunk with the wrong sha256 is refused", "export_project_backup", contractArgs("offset", 4096, "sha256", strings.Repeat("3", 64)))
	sc.fails("admin", "an oversized export chunk is refused", "export_project_backup", contractArgs("limit", 262145))
	// A short second chunk ends mid-stream, so its nextOffset is recorded;
	// the last chunk's nextOffset depends on the bundle's size.
	if total <= 4096+1024 {
		sc.t.Fatalf("the fixture bundle has %d bytes; a middle chunk needs more than %d", total, 4096+1024)
	}
	for chunk := 2; int64(len(data)) < total; chunk++ {
		limit, mask := 262144, masked("result.data", "result.total", "result.nextOffset")
		if chunk == 2 {
			limit, mask = 1024, masked("result.data", "result.total")
		}
		next := sc.ok("admin", fmt.Sprintf("export chunk %d", chunk), "export_notebook_backup", contractArgs("offset", len(data), "limit", limit, "sha256", digest), mask)
		part, err := base64.StdEncoding.DecodeString(next.str("data"))
		if err != nil || len(part) == 0 {
			sc.t.Fatalf("chunk %d: %d bytes, %v", chunk, len(part), err)
		}
		data = append(data, part...)
	}
	if workspace.ContentHash(data) != digest {
		sc.t.Errorf("assembled bundle sha256 %s, export reported %s", workspace.ContentHash(data), digest)
	}
	sc.check("assembled bundle matches the reported sha256", map[string]any{"sha256": workspace.ContentHash(data), "matches": workspace.ContentHash(data) == digest})
	dir := sc.t.TempDir()
	bundle := filepath.Join(dir, "contract.bundle")
	if err := os.WriteFile(bundle, data, 0o600); err != nil {
		sc.t.Fatal(err)
	}
	git := func(args ...string) string {
		out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
		if err != nil {
			sc.t.Fatalf("git %v: %v %s", args, err, out)
		}
		return string(out)
	}
	git("init", "-q")
	git("bundle", "verify", "-q", bundle)
	heads := git("bundle", "list-heads", bundle)
	sc.check("bundle heads", map[string]any{"heads": strings.Split(strings.TrimSpace(heads), "\n"), "head": status.str("revision")})
	encoded := base64.StdEncoding.EncodeToString(data)
	sc.fails("editor", "an editor without management consent cannot import", "import_project_bundle", map[string]any{"id": "contract-restored", "name": "Restored contract notebook", "data": encoded}, masked("arguments.data"))
	sc.ok("admin", "import the bundle as a new notebook", "import_project_bundle", map[string]any{"id": "contract-restored", "name": "Restored contract notebook", "data": encoded}, masked("arguments.data"))
	sc.fails("admin", "an import never replaces an existing notebook", "import_notebook_bundle", map[string]any{"id": "contract-restored", "name": "Again", "data": encoded}, masked("arguments.data"))
	sc.fails("admin", "an invalid bundle is refused", "import_project_bundle", map[string]any{"id": "contract-invalid", "name": "Invalid", "data": base64.StdEncoding.EncodeToString([]byte("not a bundle"))})
	restored := map[string]any{"project": "contract-restored"}
	sc.ok("admin", "the restored notebook keeps the head commit", "project_status", restored)
	sc.ok("admin", "the restored notebook keeps history", "note_history", restored)
	sc.ok("admin", "the restored notebook reads the same note", "read_note", contractWith(restored, "path", "manuscript/chapter-1.md"))
	sc.ok("admin", "the restored notebook keeps discussions", "list_comments", contractWith(restored, "path", "manuscript/chapter-1.md"))
	sc.ok("admin", "the restored notebook keeps instructions", "project_instructions", restored)
	sc.ok("admin", "the restored notebook keeps attachments", "list_attachments", restored)
	sc.ok("admin", "the restored notebook lists the same notes", "search_notes", contractWith(restored, "query", ""))
}

// The storage v2 contract audit's cases. Storage v2 rewrote history,
// revisions, live editing, changes since a cursor, discussions, locks, Trash
// and export over Postgres. These scenarios exercise what that rewrite
// touched, on notebooks of their own so the scenarios above keep their
// recording, and call only tools those scenarios already call, so tools.json
// is unchanged: history across renames, moves, copies, Trash and deletion;
// each note's version; reads while live edits are pending; cursors across
// those operations; discussions and locks across moves; refusals of unknown
// revisions and paths, with their /api/v1 error classes; derived reads at old
// revisions; the 50-commit history limit and older revisions as bases; and
// the export's commit objects and tree. They were first recorded on the Git
// backend that the notes store replaced.

const contractHistory = "contract-history"

// contractAlphaID is the fmn-id of the audit notebook's renamed note.
const contractAlphaID = "c0ffee000000000000000000000000a1"

const contractAlpha = "---\nfmn-id: " + contractAlphaID + "\ntitle: Alpha\n---\n# Alpha\n\nThe first draft.\n"

// historyArgs is contractArgs for the audit's notebook.
func historyArgs(pairs ...any) map[string]any {
	out := contractArgs(pairs...)
	out["project"] = contractHistory
	return out
}

// probe records a step whose outcome no later step depends on, so the golden
// alone fixes whether it succeeds: a flip shows as a golden difference
// instead of stopping the run.
func (sc *contractScenario) probe(as, step, tool string, args map[string]any, options ...contractOption) {
	sc.t.Helper()
	sc.record(as, step, tool, args, options...)
}

// quiet calls a tool without recording a step, for values a check compares.
func (sc *contractScenario) quiet(as, tool string, args map[string]any) contractValue {
	sc.t.Helper()
	sc.run.called[tool] = true
	ctx, cancel := context.WithTimeout(sc.t.Context(), 30*time.Second)
	defer cancel()
	result, err := sc.run.clients[as].CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		sc.t.Fatalf("%s/%s: %v", sc.name, tool, err)
	}
	if result.IsError {
		sc.t.Fatalf("%s/%s: %s", sc.name, tool, contractText(result))
	}
	return contractValue{sc.t, contractResult(sc.t, result)}
}

// apiClass records how /api/v1 classifies the same call as the bootstrap
// administrator: its status, error code and message.
func (sc *contractScenario) apiClass(step, tool string, args map[string]any) {
	sc.t.Helper()
	body, err := json.Marshal(args)
	if err != nil {
		sc.t.Fatal(err)
	}
	request := httptest.NewRequest("POST", sc.run.origin+"/api/v1/actions/"+tool, bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+testToken)
	recorder := httptest.NewRecorder()
	sc.run.app.ServeHTTP(recorder, request)
	var out struct {
		Error struct{ Code, Message string }
	}
	json.Unmarshal(recorder.Body.Bytes(), &out)
	sc.check(step, map[string]any{"tool": tool, "arguments": args, "status": recorder.Code, "code": out.Error.Code, "message": out.Error.Message})
}

// contractEdit is a live patch edit replacing the first old in content.
func contractEdit(t *testing.T, content, old, text string) map[string]any {
	t.Helper()
	start := strings.Index(content, old)
	if start < 0 {
		t.Fatalf("%q is not in %q", old, content)
	}
	return map[string]any{"start": start, "end": start + len(old), "old": old, "text": text}
}

// trashEntry finds a Trash listing's entry for a note's original path.
func trashEntry(listing contractValue, original string) (id, path string) {
	for _, item := range listing.list("entries") {
		v := contractValue{listing.t, item}
		if v.str("originalPath") == original {
			return v.str("id"), v.str("path")
		}
	}
	raw, _ := json.Marshal(listing.v)
	listing.t.Fatalf("the Trash listing lacks %s: %s", original, raw)
	return "", ""
}

func contractRenames(sc *contractScenario) {
	sc.run.literals[contractAlphaID] = true
	marks := sc.run.marks
	sc.ok("admin", "create the audit notebook", "create_project", map[string]any{"id": contractHistory, "name": "History notebook"})
	for _, member := range []struct{ user, role string }{{"contract-editor", "edit"}, {"contract-reader", "read"}} {
		if err := sc.run.app.catalog.Member(catalog.Bootstrap, contractHistory, member.user, member.role); err != nil {
			sc.t.Fatal(err)
		}
	}
	first := sc.ok("admin", "create a note with an fmn-id", "save_note", historyArgs("path", "drafts/alpha.md", "content", contractAlpha, "revision", ""))
	marks["first"] = first.str("revision")
	sc.ok("admin", "create a linking note without an fmn-id", "save_note", historyArgs("path", "drafts/beta.md", "content", "# Beta\n\nA note without an fmn-id links to [Alpha](alpha.md).\n", "revision", ""))
	edited := sc.ok("admin", "edit the note with an fmn-id", "save_note", historyArgs("path", "drafts/alpha.md", "content", strings.Replace(contractAlpha, "first", "second", 1), "revision", first.str("revision")))
	marks["beforeRename"] = edited.str("revision")
	moved := sc.ok("admin", "rename the note with an fmn-id; the linking note is rewritten", "move_notes", historyArgs("from", "drafts/alpha.md", "to", "drafts/alpha-renamed.md", "revision", edited.str("revision")))
	marks["rename"] = moved.str("revision")
	sc.ok("admin", "history follows a renamed note with an fmn-id", "note_history", historyArgs("path", "drafts/alpha-renamed.md"))
	sc.probe("admin", "history of the vacated path", "note_history", historyArgs("path", "drafts/alpha.md"))
	sc.ok("admin", "a renamed note's version is the move", "read_note", historyArgs("path", "drafts/alpha-renamed.md"))
	sc.ok("admin", "a rewritten linking note's version is the move", "read_note", historyArgs("path", "drafts/beta.md"))
	moved = sc.ok("admin", "rename the note without an fmn-id", "move_notes", historyArgs("from", "drafts/beta.md", "to", "drafts/beta-renamed.md", "revision", moved.str("revision")))
	marks["beforeFolder"] = moved.str("revision")
	sc.ok("admin", "history follows a renamed note without an fmn-id", "note_history", historyArgs("path", "drafts/beta-renamed.md"))
	moved = sc.ok("admin", "move the folder", "move_notes", historyArgs("from", "drafts", "to", "chapters", "revision", moved.str("revision")))
	marks["folder"] = moved.str("revision")
	sc.ok("admin", "history follows a note through a folder move", "note_history", historyArgs("path", "chapters/alpha-renamed.md"))
	sc.ok("admin", "history of the moved folder", "note_history", historyArgs("path", "chapters"))
	sc.probe("admin", "history of the vacated folder", "note_history", historyArgs("path", "drafts"))
	sc.ok("admin", "a note's version after a folder move", "read_note", historyArgs("path", "chapters/beta-renamed.md"))
	sc.ok("admin", "read the old path at a revision before the moves", "read_note", historyArgs("path", "drafts/alpha.md", "revision", edited.str("revision")))
	sc.fails("admin", "the old path is absent at the head", "read_note", historyArgs("path", "drafts/alpha.md"))
	reused := sc.ok("admin", "a new note takes the vacated path", "save_note", historyArgs("path", "drafts/alpha.md", "content", "# Another alpha\n\nUnrelated to the first.\n", "revision", ""))
	sc.ok("admin", "history of the reused path", "note_history", historyArgs("path", "drafts/alpha.md"))
	sc.ok("admin", "the renamed note's history is unchanged by the reuse", "note_history", historyArgs("path", "chapters/alpha-renamed.md"))
	revision := reused.str("revision")
	for i, pair := range [][2]string{{"chapters/beta-renamed.md", "chapters/beta-2.md"}, {"chapters/beta-2.md", "chapters/beta-3.md"}} {
		revision = sc.ok("admin", fmt.Sprintf("rename the note again (%d)", i+1), "move_notes", historyArgs("from", pair[0], "to", pair[1], "revision", revision)).str("revision")
	}
	sc.ok("admin", "history follows a chain of renames", "note_history", historyArgs("path", "chapters/beta-3.md"))
	copied := sc.ok("admin", "duplicate a renamed note", "duplicate_notes", historyArgs("from", "chapters/alpha-renamed.md", "to", "chapters/alpha-copy.md", "revision", revision))
	sc.ok("admin", "history of a copy", "note_history", historyArgs("path", "chapters/alpha-copy.md"))
	sc.ok("admin", "a copy's version is the duplication", "read_note", historyArgs("path", "chapters/alpha-copy.md"))
	marks["beforeTrash"] = copied.str("revision")
	trashed := sc.ok("admin", "trash a renamed note", "trash_notes", historyArgs("paths", []string{"chapters/beta-3.md"}, "revision", copied.str("revision")))
	marks["trash"] = trashed.str("revision")
	listing := sc.ok("admin", "the trashed note in the listing", "list_trash", historyArgs())
	id, trashPath := trashEntry(listing, "chapters/beta-3.md")
	sc.probe("admin", "history of a trashed note at its Trash path", "note_history", historyArgs("path", trashPath))
	restored := sc.ok("admin", "restore it to a new path", "restore_trash", historyArgs("id", id, "to", "chapters/beta-restored.md", "revision", listing.str("revision")))
	marks["restore"] = restored.str("revision")
	sc.ok("admin", "history follows a note through Trash and a restore to a new path", "note_history", historyArgs("path", "chapters/beta-restored.md"))
	sc.ok("admin", "a restored note's version is the restore", "read_note", historyArgs("path", "chapters/beta-restored.md"))
	reread := sc.ok("admin", "read the reused path before deleting it", "read_note", historyArgs("path", "drafts/alpha.md"))
	marks["beforeDelete"] = reread.str("revision")
	group := map[string]any{"id": "history-delete", "summary": "Delete the reused note", "edits": []any{map[string]any{"path": "drafts/alpha.md", "content": "", "revision": reread.str("revision"), "hash": workspace.ContentHash([]byte(reread.str("content"))), "delete": true}}}
	raw, _ := json.Marshal(group)
	marks["deleteGroup"] = string(raw)
	deleted := sc.ok("admin", "delete a note through a change group", "submit_change_group", historyArgs("mode", "apply", "group", group))
	marks["delete"] = deleted.str("revision")
	sc.probe("admin", "history of a deleted note", "note_history", historyArgs("path", "drafts/alpha.md"))
	sc.ok("admin", "recreate the deleted path", "save_note", historyArgs("path", "drafts/alpha.md", "content", "# Third alpha\n", "revision", ""))
	sc.ok("admin", "history of a path deleted and recreated", "note_history", historyArgs("path", "drafts/alpha.md"))
	sc.ok("admin", "an fmn-id link resolves after the moves", "resolve_notebook_link", historyArgs("noteId", contractAlphaID))
	sc.ok("admin", "the notebook's history", "note_history", historyArgs())
	sc.ok("reader", "a reader sees the same history", "note_history", historyArgs("path", "chapters/alpha-renamed.md"))
	sc.fails("outsider", "a non-member cannot read history", "note_history", historyArgs("path", "chapters/alpha-renamed.md"))
}

func contractLiveReads(sc *contractScenario) {
	const path, renamed = "chapters/alpha-renamed.md", "chapters/alpha-live.md"
	committed := sc.ok("admin", "read the committed note", "read_note", historyArgs("path", path))
	live := sc.ok("admin", "open a live session", "read_live_note", historyArgs("path", path))
	edit := contractEdit(sc.t, live.str("content"), "second", "third")
	sc.ok("admin", "patch a word live", "patch_live_note", historyArgs("path", path, "base", live.str("liveRevision"), "operationId", "history-op-1", "edits", []any{edit}))
	sc.ok("admin", "read_note keeps the committed text while live edits are pending", "read_note", historyArgs("path", path))
	sc.ok("admin", "read_document while live edits are pending", "read_document", historyArgs("path", path))
	sc.ok("admin", "search while live edits are pending", "search_notes", historyArgs("query", "third"))
	sc.ok("admin", "status while live edits are pending", "project_status", historyArgs())
	sc.ok("admin", "history has no checkpoint while live edits are pending", "note_history", historyArgs("path", path))
	sc.probe("admin", "a save that requires a checkpoint while live edits are pending", "save_note", historyArgs("path", path, "content", "# Saved\n", "revision", committed.str("revision"), "requireCheckpoint", true))
	sc.probe("admin", "a save from the committed revision while live edits are pending", "save_note", historyArgs("path", path, "content", "# Saved\n", "revision", committed.str("revision")))
	sc.ok("admin", "the live note after the refused saves", "read_live_note", historyArgs("path", path))
	sc.ok("admin", "history after the refused saves", "note_history", historyArgs("path", path))
	sc.probe("admin", "retrying a patch after its edits were committed", "patch_live_note", historyArgs("path", path, "base", live.str("liveRevision"), "operationId", "history-op-1", "edits", []any{edit}))
	again := sc.ok("admin", "the retry did not apply the edit twice", "read_live_note", historyArgs("path", path))
	status := sc.ok("admin", "status before moving a note with live edits", "project_status", historyArgs())
	sc.ok("admin", "patch before the move", "patch_live_note", historyArgs("path", path, "base", again.str("liveRevision"), "operationId", "history-op-2", "edits", []any{contractEdit(sc.t, again.str("content"), "draft.", "draft, revised.")}))
	sc.probe("admin", "moving a note with pending live edits", "move_notes", historyArgs("from", path, "to", renamed, "revision", status.str("revision")))
	sc.ok("admin", "the live note after the move attempt", "read_live_note", historyArgs("path", path))
	sc.ok("admin", "checkpoint before moving", "checkpoint_live_note", historyArgs("path", path))
	status = sc.ok("admin", "status after the checkpoint", "project_status", historyArgs())
	moved := sc.ok("admin", "move the checkpointed note", "move_notes", historyArgs("from", path, "to", renamed, "revision", status.str("revision")))
	sc.ok("admin", "history of a note moved with pending live edits", "note_history", historyArgs("path", renamed))
	sc.ok("admin", "the committed text after the move", "read_note", historyArgs("path", renamed))
	moving := sc.ok("admin", "a live read at the new path", "read_live_note", historyArgs("path", renamed))
	sc.fails("admin", "a live read at the vacated path", "read_live_note", historyArgs("path", path))
	sc.probe("admin", "a patch based on the vacated path's live revision", "patch_live_note", historyArgs("path", renamed, "base", again.str("liveRevision"), "operationId", "history-op-3", "edits", []any{contractEdit(sc.t, moving.str("content"), "revised", "reworked")}))
	current := sc.ok("admin", "the live note before trashing it", "read_live_note", historyArgs("path", renamed))
	sc.ok("admin", "patch before trashing", "patch_live_note", historyArgs("path", renamed, "base", current.str("liveRevision"), "operationId", "history-op-4", "edits", []any{contractEdit(sc.t, current.str("content"), "The ", "Then the ")}))
	trashed := sc.ok("admin", "trash a note with pending live edits", "trash_notes", historyArgs("paths", []string{renamed}, "revision", moved.str("revision")))
	listing := sc.ok("admin", "the Trash listing", "list_trash", historyArgs())
	id, trashPath := trashEntry(listing, renamed)
	sc.ok("admin", "Trash keeps the live edits", "read_note", historyArgs("path", trashPath))
	sc.fails("admin", "a trashed note has no live session at its old path", "read_live_note", historyArgs("path", renamed))
	restored := sc.ok("admin", "restore the note", "restore_trash", historyArgs("id", id, "revision", trashed.str("revision")))
	sc.ok("admin", "the restored note's live text", "read_live_note", historyArgs("path", renamed))
	sc.ok("admin", "history across live edits, a move, Trash and a restore", "note_history", historyArgs("path", renamed))
	locked := sc.ok("admin", "lock the live note", "set_note_lock", historyArgs("path", renamed, "locked", true, "revision", restored.str("revision")))
	held := sc.ok("admin", "a live read of a locked note", "read_live_note", historyArgs("path", renamed))
	sc.fails("admin", "a live patch to a locked note is refused", "patch_live_note", historyArgs("path", renamed, "base", held.str("liveRevision"), "operationId", "history-op-5", "edits", []any{contractEdit(sc.t, held.str("content"), "Then", "Now")}))
	sc.ok("admin", "unlock the live note", "set_note_lock", historyArgs("path", renamed, "locked", false, "revision", locked.str("revision")))
	sc.ok("admin", "a checkpoint with nothing pending makes no commit", "checkpoint_live_note", historyArgs("path", renamed))
	sc.ok("editor", "an editor reads live", "read_live_note", historyArgs("path", renamed))
	sc.fails("outsider", "a non-member cannot read live", "read_live_note", historyArgs("path", renamed))
	sc.fails("admin", "a live read of a non-Markdown path is refused", "read_live_note", historyArgs("path", "chapters/notes.txt"))
}

func contractCursors(sc *contractScenario) {
	marks := sc.run.marks
	unknown := strings.Repeat("4", 40)
	head := sc.ok("admin", "status for the cursor cases", "project_status", historyArgs()).str("revision")
	sc.ok("admin", "changes since the first commit", "read_project_changes", historyArgs("since", marks["first"]))
	sc.ok("admin", "changes across a rename", "read_project_changes", historyArgs("since", marks["beforeRename"], "revision", marks["rename"]))
	sc.ok("admin", "changes across a folder move", "read_project_changes", historyArgs("since", marks["beforeFolder"], "revision", marks["folder"]))
	sc.ok("admin", "changes across Trash and a restore to a new path", "read_project_changes", historyArgs("since", marks["beforeTrash"], "revision", marks["restore"]))
	sc.ok("admin", "changes across a deletion", "read_notebook_changes", historyArgs("since", marks["beforeDelete"], "revision", marks["delete"]))
	sc.ok("admin", "changes since the head are empty", "read_project_changes", historyArgs("since", head))
	sc.probe("admin", "changes from a newer revision to an older one", "read_project_changes", historyArgs("since", marks["folder"], "revision", marks["rename"]))
	sc.probe("admin", "changes since another notebook's revision", "read_project_changes", historyArgs("since", sc.head()))
	sc.probe("admin", "changes up to an unknown revision", "read_project_changes", historyArgs("since", marks["first"], "revision", unknown))
	sc.probe("admin", "a snapshot at an unknown revision", "read_project_changes", historyArgs("snapshot", true, "revision", unknown))
	sc.ok("admin", "a snapshot before the folder move", "read_project_changes", historyArgs("snapshot", true, "revision", marks["rename"]))
	sc.ok("admin", "a snapshot with a note in Trash", "read_project_changes", historyArgs("snapshot", true, "revision", marks["trash"]))
	sc.ok("admin", "a new consumer of the audit notebook", "consumer_checkpoint", historyArgs("consumer", "history-review"))
	sc.ok("admin", "complete a workflow at an older revision", "complete_workflow", historyArgs("completion", map[string]any{"consumer": "history-review", "previous": "", "revision": marks["folder"], "summary": "Reviewed up to the folder move.", "reset": false}))
	sc.ok("admin", "changes since that checkpoint", "consumer_checkpoint", historyArgs("consumer", "history-review"))
	sc.probe("admin", "completing at an unknown revision", "complete_workflow", historyArgs("completion", map[string]any{"consumer": "history-review", "previous": marks["folder"], "revision": unknown, "summary": "Unknown.", "reset": false}))
	sc.probe("admin", "completing at another notebook's revision", "complete_workflow", historyArgs("completion", map[string]any{"consumer": "history-review", "previous": marks["folder"], "revision": sc.head(), "summary": "Foreign.", "reset": false}))
	sc.ok("admin", "complete the workflow at the head", "complete_workflow", historyArgs("completion", map[string]any{"consumer": "history-review", "previous": marks["folder"], "revision": head, "summary": "Reviewed everything.", "reset": false}))
	sc.ok("admin", "the checkpoint at the head", "consumer_checkpoint", historyArgs("consumer", "history-review"))
	third := sc.ok("admin", "read a note to replace", "read_note", historyArgs("path", "drafts/alpha.md"))
	replaced := sc.ok("admin", "delete a note and add one with the same text in one group", "submit_change_group", historyArgs("mode", "apply", "group", map[string]any{"id": "history-replace", "summary": "Rename by hand", "edits": []any{
		map[string]any{"path": "drafts/alpha.md", "content": "", "revision": third.str("revision"), "hash": workspace.ContentHash([]byte(third.str("content"))), "delete": true},
		map[string]any{"path": "drafts/gamma.md", "content": third.str("content"), "revision": "", "hash": ""},
	}}))
	sc.ok("admin", "changes across a delete and an add of the same text", "read_project_changes", historyArgs("since", third.str("revision"), "revision", replaced.str("revision")))
	var group map[string]any
	if err := json.Unmarshal([]byte(marks["deleteGroup"]), &group); err != nil {
		sc.t.Fatal(err)
	}
	sc.ok("admin", "retrying an applied change group after later commits replays it", "submit_change_group", historyArgs("mode", "apply", "group", group))
}

func contractDiscussions(sc *contractScenario) {
	const path, renamed, copy = "chapters/beta-restored.md", "chapters/beta-final.md", "chapters/beta-final-copy.md"
	empty := sc.ok("admin", "the restored note has no discussion", "list_comments", historyArgs("path", path))
	anchored := sc.ok("admin", "add an anchored comment", "add_comment", historyArgs("path", path, "text", "Is this link still right?", "revision", empty.str("revision"), "anchor", map[string]any{"quote": "A note without an fmn-id", "prefix": "", "suffix": ""}))
	thread := anchored.str("comments.0.id")
	moved := sc.ok("admin", "rename the commented note", "move_notes", historyArgs("from", path, "to", renamed, "revision", anchored.str("revision")))
	sc.ok("admin", "the discussion follows the renamed note", "list_comments", historyArgs("path", renamed))
	sc.probe("admin", "the vacated path's discussion", "list_comments", historyArgs("path", path))
	copied := sc.ok("admin", "duplicate the commented note", "duplicate_notes", historyArgs("from", renamed, "to", copy, "revision", moved.str("revision")))
	sc.ok("admin", "a copy's discussion", "list_comments", historyArgs("path", copy))
	locked := sc.ok("admin", "lock the folder", "set_note_lock", historyArgs("path", "chapters", "locked", true, "revision", copied.str("revision")))
	sc.probe("editor", "an editor replies on a note in a locked folder", "add_comment", historyArgs("path", renamed, "text", "Still right.", "revision", locked.str("revision"), "replyTo", thread))
	sc.fails("admin", "a note in a locked folder cannot be trashed", "trash_notes", historyArgs("paths", []string{copy}, "revision", sc.headOf(contractHistory)))
	sc.ok("admin", "locks for a missing path inside the locked folder", "note_locks", historyArgs("path", "chapters/missing.md"))
	unlocked := sc.ok("admin", "unlock the folder", "set_note_lock", historyArgs("path", "chapters", "locked", false, "revision", sc.headOf(contractHistory)))
	sc.probe("admin", "unlocking a path that is not locked", "set_note_lock", historyArgs("path", renamed, "locked", false, "revision", unlocked.str("revision")))
	sc.probe("admin", "locking a missing path", "set_note_lock", historyArgs("path", "nowhere/missing.md", "locked", true, "revision", sc.headOf(contractHistory)))
	trashed := sc.ok("admin", "trash the commented note", "trash_notes", historyArgs("paths", []string{renamed}, "revision", sc.headOf(contractHistory)))
	listing := sc.ok("admin", "the Trash listing with the commented note", "list_trash", historyArgs())
	id, trashPath := trashEntry(listing, renamed)
	sc.probe("admin", "the discussion of a note in Trash", "list_comments", historyArgs("path", trashPath))
	sc.probe("admin", "the trashed note's old path has no discussion", "list_comments", historyArgs("path", renamed))
	sc.ok("admin", "restore the commented note", "restore_trash", historyArgs("id", id, "revision", trashed.str("revision")))
	discussion := sc.ok("admin", "the discussion survives Trash and restore", "list_comments", historyArgs("path", renamed))
	sc.fails("admin", "a comment on a missing note", "add_comment", historyArgs("path", "chapters/missing.md", "text", "Hello?", "revision", discussion.str("revision")))
	sc.fails("admin", "a reply to an unknown thread", "add_comment", historyArgs("path", renamed, "text", "Reply", "revision", discussion.str("revision"), "replyTo", strings.Repeat("c", 32)))
	preview := sc.ok("admin", "the Trash holds the change group's deletion", "list_trash", historyArgs())
	sc.ok("admin", "empty the Trash", "empty_trash", historyArgs("revision", preview.str("revision"), "confirmation", preview.str("confirmation")))
	empty = sc.ok("admin", "the emptied Trash", "list_trash", historyArgs())
	sc.probe("admin", "emptying an empty Trash", "empty_trash", historyArgs("revision", empty.str("revision"), "confirmation", empty.str("confirmation")))
}

func contractUnknowns(sc *contractScenario) {
	const note = "chapters/alpha-live.md"
	unknown, zero := strings.Repeat("4", 40), strings.Repeat("0", 40)
	foreign := sc.head()
	sc.run.literals[workspace.ContentHash(contractPNG(sc.t, color.RGBA{20, 160, 20, 255}))] = true
	uploaded := sc.ok("admin", "upload an attachment", "upload_attachment", historyArgs("path", "assets/dot.png", "data", base64.StdEncoding.EncodeToString(contractPNG(sc.t, color.RGBA{20, 160, 20, 255})), "revision", ""))
	head := uploaded.str("revision")
	sc.run.marks["upload"] = head
	for _, r := range []struct{ name, revision string }{
		{"an unknown revision", unknown},
		{"the all-zero revision", zero},
		{"another notebook's revision", foreign},
	} {
		sc.probe("admin", "read_note at "+r.name, "read_note", historyArgs("path", note, "revision", r.revision))
	}
	// The normalizer labels only full lower-case IDs, so these two are masked.
	sc.probe("admin", "read_note at an abbreviated head revision", "read_note", historyArgs("path", note, "revision", head[:12]), masked("arguments.revision"))
	sc.probe("admin", "read_note at the upper-case head revision", "read_note", historyArgs("path", note, "revision", strings.ToUpper(head)), masked("arguments.revision"))
	for _, r := range []struct{ name, revision string }{{"an unknown revision", unknown}, {"another notebook's revision", foreign}} {
		sc.probe("admin", "read_attachment at "+r.name, "read_attachment", historyArgs("path", "assets/dot.png", "revision", r.revision))
		sc.probe("admin", "read_document at "+r.name, "read_document", historyArgs("path", note, "revision", r.revision))
		sc.probe("admin", "document_outline at "+r.name, "document_outline", historyArgs("path", note, "revision", r.revision))
		sc.probe("admin", "list_documents at "+r.name, "list_documents", historyArgs("revision", r.revision))
		sc.probe("admin", "notebook_map at "+r.name, "notebook_map", historyArgs("revision", r.revision))
		sc.probe("admin", "find_term_mentions at "+r.name, "find_term_mentions", historyArgs("terms", []any{map[string]any{"name": "alpha"}}, "revision", r.revision))
		sc.probe("admin", "resolve_notebook_link at "+r.name, "resolve_notebook_link", historyArgs("path", note, "revision", r.revision))
		sc.probe("admin", "restore_note from "+r.name, "restore_note", historyArgs("path", note, "sourceRevision", r.revision, "revision", head))
		sc.probe("admin", "save_note expecting "+r.name, "save_note", historyArgs("path", note, "content", "# Overwritten\n", "revision", r.revision))
		sc.probe("admin", "set_note_properties expecting "+r.name, "set_note_properties", historyArgs("path", note, "revision", r.revision, "metadata", map[string]any{"title": "Changed", "kind": "", "status": "", "tags": []string{}}))
		sc.probe("admin", "set_note_container expecting "+r.name, "set_note_container", historyArgs("path", note, "container", "draft", "revision", r.revision))
		sc.probe("admin", "set_note_lock expecting "+r.name, "set_note_lock", historyArgs("path", note, "locked", true, "revision", r.revision))
		sc.probe("admin", "add_comment expecting "+r.name, "add_comment", historyArgs("path", note, "text", "Unknown base.", "revision", r.revision))
		sc.probe("admin", "upload_attachment expecting "+r.name, "upload_attachment", historyArgs("path", "assets/dot.png", "data", base64.StdEncoding.EncodeToString(contractPNG(sc.t, color.RGBA{20, 20, 160, 255})), "revision", r.revision))
		sc.probe("admin", "set_project_instructions expecting "+r.name, "set_project_instructions", historyArgs("content", "Unknown base.\n", "revision", r.revision))
		sc.probe("admin", "duplicate_notes expecting "+r.name, "duplicate_notes", historyArgs("from", note, "to", "chapters/unknown-copy.md", "revision", r.revision))
		sc.probe("admin", "move_notes expecting "+r.name, "move_notes", historyArgs("from", note, "to", "chapters/unknown-move.md", "revision", r.revision))
		sc.probe("admin", "trash_notes expecting "+r.name, "trash_notes", historyArgs("paths", []string{note}, "revision", r.revision))
	}
	sc.probe("admin", "save_note expecting the all-zero revision", "save_note", historyArgs("path", note, "content", "# Overwritten\n", "revision", zero))
	for _, path := range []string{"./" + note, "chapters//alpha-live.md", "/" + note, "chapters/../" + note, "Chapters/alpha-live.md", "chapters/./alpha-live.md"} {
		sc.probe("admin", "read_note spelled "+path, "read_note", historyArgs("path", path))
	}
	for _, path := range []string{"chapters/missing.md", "chapters/", "./chapters", "nowhere"} {
		sc.probe("admin", "note_history of "+path, "note_history", historyArgs("path", path))
	}
	revision := sc.headOf(contractHistory)
	sc.probe("admin", "list_comments of a missing note", "list_comments", historyArgs("path", "chapters/missing.md"))
	sc.probe("admin", "document_outline of a missing note", "document_outline", historyArgs("path", "chapters/missing.md"))
	sc.probe("admin", "set_note_properties of a missing note", "set_note_properties", historyArgs("path", "chapters/missing.md", "revision", revision, "metadata", map[string]any{"title": "Missing", "kind": "", "status": "", "tags": []string{}}))
	sc.probe("admin", "set_note_container of a missing note", "set_note_container", historyArgs("path", "chapters/missing.md", "container", "draft", "revision", revision))
	sc.probe("admin", "checkpoint_live_note of a missing note", "checkpoint_live_note", historyArgs("path", "chapters/missing.md"))
	sc.probe("admin", "patch_live_note of a missing note", "patch_live_note", historyArgs("path", "chapters/missing.md", "base", "0:0", "operationId", "history-missing", "edits", []any{map[string]any{"start": 0, "end": 0, "old": "", "text": "x"}}))
	sc.probe("admin", "duplicate_notes of a missing note", "duplicate_notes", historyArgs("from", "chapters/missing.md", "to", "chapters/missing-copy.md", "revision", revision))
	sc.probe("admin", "restore_note of a path absent at the source revision", "restore_note", historyArgs("path", note, "sourceRevision", sc.run.marks["first"], "revision", revision))
	sc.probe("admin", "read_attachment absent at an older revision", "read_attachment", historyArgs("path", "assets/dot.png", "revision", sc.run.marks["first"]))
	sc.probe("admin", "verify_source of a missing note", "verify_source", historyArgs("path", "chapters/missing.md"))
	sc.apiClass("/api/v1 class: read_note at an unknown revision", "read_note", historyArgs("path", note, "revision", unknown))
	sc.apiClass("/api/v1 class: read_note at another notebook's revision", "read_note", historyArgs("path", note, "revision", foreign))
	sc.apiClass("/api/v1 class: read_note of a missing note", "read_note", historyArgs("path", "chapters/missing.md"))
	sc.apiClass("/api/v1 class: read_note outside the notebook", "read_note", historyArgs("path", "../escape.md"))
	sc.apiClass("/api/v1 class: save_note expecting an unknown revision", "save_note", historyArgs("path", note, "content", "# Overwritten\n", "revision", unknown))
	sc.apiClass("/api/v1 class: move_notes of a missing note", "move_notes", historyArgs("from", "chapters/missing.md", "to", "chapters/found.md", "revision", revision))
	sc.apiClass("/api/v1 class: trash_notes of a missing note", "trash_notes", historyArgs("paths", []string{"chapters/missing.md"}, "revision", revision))
	sc.apiClass("/api/v1 class: note_history of a missing note", "note_history", historyArgs("path", "chapters/missing.md"))
	sc.apiClass("/api/v1 class: restore_note from an unknown revision", "restore_note", historyArgs("path", note, "sourceRevision", unknown, "revision", revision))
	sc.apiClass("/api/v1 class: read_attachment at an unknown revision", "read_attachment", historyArgs("path", "assets/dot.png", "revision", unknown))
}

// contractExport compares the audit notebook's export with what the tools
// report: the bundle's head, every commit object (author and committer
// times replaced by <EPOCH>), the tree, each note's version and history.
func contractExport(sc *contractScenario) {
	status := sc.ok("admin", "status before exporting the audit notebook", "project_status", historyArgs())
	var data []byte
	digest := ""
	for chunk := 1; ; chunk++ {
		args := historyArgs("offset", len(data), "limit", 262144)
		if digest != "" {
			args["sha256"] = digest
		}
		part := sc.ok("admin", fmt.Sprintf("export chunk %d", chunk), "export_project_backup", args, masked("result.data", "result.total", "result.nextOffset", "result.sha256"))
		piece, err := base64.StdEncoding.DecodeString(part.str("data"))
		if err != nil || len(piece) == 0 {
			sc.t.Fatalf("chunk %d: %d bytes, %v", chunk, len(piece), err)
		}
		data, digest = append(data, piece...), part.str("sha256")
		total, _ := strconv.ParseInt(string(part.get("total").(json.Number)), 10, 64)
		if int64(len(data)) >= total {
			break
		}
	}
	dir := sc.t.TempDir()
	bundle := filepath.Join(dir, "history.bundle")
	if err := os.WriteFile(bundle, data, 0o600); err != nil {
		sc.t.Fatal(err)
	}
	repo := filepath.Join(dir, "repo")
	git := func(args ...string) string {
		out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
		if err != nil {
			sc.t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("clone", "-q", bundle, repo)
	in := func(args ...string) string { return git(append([]string{"-C", repo}, args...)...) }
	head := in("rev-parse", "HEAD")
	sc.check("the export's head is the notebook's revision", map[string]any{"head": head, "revision": status.str("revision")})
	epoch := regexp.MustCompile(`> \d+ ([+-]\d{4})$`)
	revisions := strings.Split(in("rev-list", "--topo-order", "HEAD"), "\n")
	commits := []any{}
	for _, revision := range revisions {
		lines := strings.Split(in("cat-file", "commit", revision), "\n")
		for i, line := range lines {
			if strings.HasPrefix(line, "author ") || strings.HasPrefix(line, "committer ") {
				lines[i] = epoch.ReplaceAllString(line, "> <EPOCH> $1")
			}
		}
		commits = append(commits, map[string]any{"commit": revision, "object": lines})
	}
	sc.check("the export's commits, newest first", commits)
	sc.check("the export's tree", strings.Split(in("ls-tree", "-r", "--full-tree", "HEAD"), "\n"))
	history := sc.quiet("admin", "note_history", historyArgs())
	reported := []string{}
	for _, item := range history.list("") {
		reported = append(reported, contractValue{sc.t, item}.str("revision"))
	}
	sc.check("note_history lists the export's commits", map[string]any{"matches": slices.Equal(reported, revisions[:min(len(revisions), 50)]), "history": len(reported), "export": len(revisions)})
	versions := map[string]any{}
	for _, item := range sc.quiet("admin", "search_notes", historyArgs("query", "", "includeTrash", true)).list("") {
		path := contractValue{sc.t, item}.str("path")
		read := sc.quiet("admin", "read_note", historyArgs("path", path))
		versions[path] = map[string]any{"version": read.str("version"), "lastCommit": in("log", "-1", "--format=%H", "HEAD", "--", path)}
	}
	sc.check("each note's version is its last commit in the export", versions)
	sc.ok("admin", "import the audit notebook's bundle", "import_project_bundle", map[string]any{"id": "contract-history-restored", "name": "Restored history notebook", "data": base64.StdEncoding.EncodeToString(data)}, masked("arguments.data"))
	restored := map[string]any{"project": "contract-history-restored"}
	sc.ok("admin", "the restored notebook keeps the head", "project_status", restored)
	sc.ok("admin", "the restored notebook follows the renamed note's history", "note_history", contractWith(restored, "path", "chapters/alpha-renamed.md"))
	sc.ok("admin", "the restored notebook's history of the live note", "note_history", contractWith(restored, "path", "chapters/alpha-live.md"))
	sc.ok("admin", "the restored notebook keeps the discussion", "list_comments", contractWith(restored, "path", "chapters/beta-final.md"))
	note := sc.ok("admin", "read a restored note", "read_note", contractWith(restored, "path", "chapters/beta-final.md"))
	saved := sc.ok("admin", "a save continues the restored history", "save_note", contractWith(restored, "path", "chapters/beta-final.md", "content", note.str("content")+"\nRestored.\n", "revision", note.str("revision")))
	continued := []string{}
	for _, item := range sc.quiet("admin", "note_history", restored).list("")[:2] {
		continued = append(continued, contractValue{sc.t, item}.str("revision"))
	}
	sc.check("the save follows the imported head", map[string]any{"head": head, "saved": saved.str("revision"), "newestTwo": continued})
}

// contractDerived reads what storage v2 derives from revisions (listings,
// links, search, history search, knowledge reads at old revisions) after
// the renames, copies, Trash and live edits above.
func contractDerived(sc *contractScenario) {
	marks := sc.run.marks
	sc.ok("admin", "browse a moved folder", "browse_folder", historyArgs("path", "chapters"))
	sc.ok("admin", "browse the vacated folder", "browse_folder", historyArgs("path", "drafts"))
	sc.ok("admin", "search including Trash", "search_notes", historyArgs("query", "", "includeTrash", true))
	sc.ok("admin", "collections after the moves", "note_collections", historyArgs())
	sc.ok("admin", "link targets after renames, copies and Trash", "find_notebook_link_targets", historyArgs("query", ""))
	sc.ok("admin", "backlinks after renames", "get_note_sources", historyArgs("path", "chapters/alpha-live.md"))
	sc.ok("admin", "hybrid search after renames", "hybrid_search", historyArgs("query", map[string]any{"text": "draft"}))
	sc.probe("admin", "search the notebook's history", "search_notebooks", map[string]any{"projects": []string{contractHistory}, "query": "second draft", "history": true})
	sc.ok("admin", "read a document at a revision before the renames", "read_document", historyArgs("path", "drafts/alpha.md", "revision", marks["beforeRename"]))
	sc.ok("admin", "an outline at a revision before the renames", "document_outline", historyArgs("path", "drafts/alpha.md", "revision", marks["beforeRename"]))
	sc.probe("admin", "documents at a revision before the folder move", "list_documents", historyArgs("revision", marks["rename"]))
	sc.probe("admin", "the map at a revision with a note in Trash", "notebook_map", historyArgs("revision", marks["trash"]))
	sc.probe("admin", "term mentions at a historical revision", "find_term_mentions", historyArgs("terms", []any{map[string]any{"name": "draft"}}, "revision", marks["rename"]))
	sc.ok("admin", "resolve the renamed note at its old path and revision", "resolve_notebook_link", historyArgs("path", "drafts/alpha.md", "noteId", contractAlphaID, "revision", marks["beforeRename"]))
	sc.ok("admin", "read an attachment at its revision", "read_attachment", historyArgs("path", "assets/dot.png", "revision", marks["upload"]))
}

// contractLimits checks the 50-commit history limit and the commit list of
// changes since a cursor on a notebook of 52 revisions of one note.
func contractLimits(sc *contractScenario) {
	const notebook, path = "contract-limits", "log.md"
	args := func(pairs ...any) map[string]any {
		out := contractArgs(pairs...)
		out["project"] = notebook
		return out
	}
	sc.ok("admin", "create a notebook for limits", "create_project", map[string]any{"id": notebook, "name": "Limits notebook"})
	first := sc.ok("admin", "create the note", "save_note", args("path", path, "content", "# Log\n\nEntry 0.\n", "revision", ""))
	revisions := []string{first.str("revision")}
	for i := 1; i <= 51; i++ {
		saved := sc.quiet("admin", "save_note", args("path", path, "content", fmt.Sprintf("# Log\n\nEntry %d.\n", i), "revision", revisions[len(revisions)-1]))
		revisions = append(revisions, saved.str("revision"))
	}
	summary := func(tool string, value contractValue, path string) map[string]any {
		listed := value.list(path)
		out := map[string]any{"tool": tool, "count": len(listed)}
		if len(listed) > 0 {
			out["newest"] = contractValue{sc.t, listed[0]}.str("revision")
			out["oldest"] = contractValue{sc.t, listed[len(listed)-1]}.str("revision")
		}
		return out
	}
	sc.check("the note's 52 revisions, oldest first", revisions)
	sc.check("note history lists the newest 50", summary("note_history", sc.quiet("admin", "note_history", args("path", path)), ""))
	sc.check("notebook history lists the newest 50", summary("note_history", sc.quiet("admin", "note_history", args()), ""))
	changes := sc.quiet("admin", "read_project_changes", args("since", first.str("revision")))
	out := summary("read_project_changes", changes, "commits")
	out["truncated"] = changes.get("commitsTruncated")
	out["files"] = changes.get("files")
	sc.check("changes since the first revision list all 51 later commits", out)
	// A revision older than the head is a valid base for a note that has
	// not changed since; each kind of change checks it its own way.
	base := revisions[len(revisions)-1]
	other := sc.ok("admin", "create a second note", "save_note", args("path", "other.md", "content", "# Other\n", "revision", ""))
	sc.ok("admin", "a save from an older revision of a note unchanged since", "save_note", args("path", path, "content", "# Log\n\nEntry 52.\n", "revision", base))
	sc.fails("admin", "a save from that revision again conflicts", "save_note", args("path", path, "content", "# Log\n\nEntry 53.\n", "revision", base))
	older := other.str("revision")
	sc.probe("admin", "a comment from an older revision", "add_comment", args("path", "other.md", "text", "From an older revision.", "revision", older))
	sc.probe("admin", "properties from an older revision of a note unchanged since", "set_note_properties", args("path", "other.md", "revision", older, "metadata", map[string]any{"title": "Other", "kind": "", "status": "", "tags": []string{}}))
	sc.probe("admin", "a lock from an older revision", "set_note_lock", args("path", "other.md", "locked", false, "revision", older))
	sc.probe("admin", "a move from an older revision", "move_notes", args("from", "other.md", "to", "other-moved.md", "revision", older))
	sc.probe("admin", "Trash from an older revision", "trash_notes", args("paths", []string{path}, "revision", older))
}
