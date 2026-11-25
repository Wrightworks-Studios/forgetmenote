package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"forgetmenote/internal/gitobj"
)

// The Git notebook backend was the reference that notebooks kept in the
// notes store were built to match. Before it was deleted, the scenarios of
// the tests that compared the two backends ran on both at storage-v2
// 14efbd3, where both answered alike, and testdata/recorded keeps what the
// Git backend answered: each step's result and error, the revisions it
// made and the files it changed, with what differs between runs by nature
// named (see recording.named). These tests hold notebooks kept in the notes
// store to those answers.
var updateRecorded = flag.Bool("update-recorded", false, "rewrite testdata/recorded from the notes store's answers, for an intended change of behaviour only")

// A recording is what a scenario's steps did to one notebook: what each
// answered, the revisions it made and the files it changed.
type recording struct {
	t     *testing.T
	s     *Store
	name  string
	names map[string]string // raw text to its name
	count map[string]int    // names given, by kind
	// pending names the content hashes and discussion paths of the files at
	// the head, by the raw path they belong to, once they are seen.
	pending map[string]string
	tree    map[string]string // the head's files when last recorded, named
	head    string
	steps   []recordedStep
}

// A recordedStep is one step of a scenario. A step with no result field is
// one whose result is not compared, as for a refusal; one with no commits
// and no files changed nothing.
type recordedStep struct {
	Name    string             `json:"name"`
	Result  json.RawMessage    `json:"result,omitempty"`
	Error   string             `json:"error,omitempty"`
	Is      []string           `json:"is,omitempty"`
	Commits []recordedCommit   `json:"commits,omitempty"`
	Files   map[string]*string `json:"files,omitempty"` // changed files; null for a removed one
}

// A recordedCommit is a revision a step made.
type recordedCommit struct {
	Revision  string   `json:"revision"`
	Parents   []string `json:"parents"`
	Author    string   `json:"author"`
	Committer string   `json:"committer"`
	Message   string   `json:"message"`
}

func newRecording(t *testing.T, s *Store, name string) *recording {
	return &recording{t: t, s: s, name: name, names: map[string]string{}, count: map[string]int{}, pending: map[string]string{}}
}

// begin records the fixture: it names the revisions so far from the head
// down, <base> first, and records every file at the head.
func (r *recording) begin() {
	r.t.Helper()
	r.head = headOf(r.t, r.s)
	for i, revision := range r.history() {
		name := "<base>"
		if i > 0 {
			name = fmt.Sprintf("<base~%d>", i)
		}
		r.names[revision] = name
	}
	r.tree = map[string]string{}
	step := recordedStep{Name: "fixture"}
	files := r.files()
	r.pendAll(files)
	r.recordFiles(&step, files)
	r.steps = append(r.steps, step)
}

// step runs do and records its result and error, the revisions it made and
// the files it changed; it returns the result.
func (r *recording) step(name string, do func(*Store) (any, error)) any {
	r.t.Helper()
	result, err := do(r.s)
	r.record(name, recordedJSON(result), err)
	return result
}

// refuse runs do, which must fail, and records its error, and any revision
// it made or file it changed, which a refusal must not.
func (r *recording) refuse(name string, do func(*Store) error) {
	r.t.Helper()
	err := do(r.s)
	if err == nil {
		r.t.Errorf("%s: accepted", name)
	}
	r.record(name, nil, err)
}

func (r *recording) record(name string, result []byte, err error) {
	r.t.Helper()
	step := recordedStep{Name: name}
	head := headOf(r.t, r.s)
	var made []string
	if head != r.head {
		made = r.revisionsSince(r.head)
		for _, revision := range made {
			r.count["revision"]++
			r.names[revision] = fmt.Sprintf("<rev %d>", r.count["revision"])
		}
	}
	files := r.files()
	r.pendAll(files)
	if result != nil {
		step.Result = json.RawMessage(r.named(string(result), true))
	}
	if err != nil {
		step.Error = r.named(err.Error(), true)
		for _, class := range []struct {
			name string
			err  error
		}{{"forbidden", ErrForbidden}, {"conflict", ErrConflict}, {"not exist", fs.ErrNotExist}, {"live pending", ErrLivePending}} {
			if errors.Is(err, class.err) {
				step.Is = append(step.Is, class.name)
			}
		}
	}
	for _, revision := range made {
		step.Commits = append(step.Commits, r.commit(revision))
	}
	r.head = head
	r.recordFiles(&step, files)
	r.steps = append(r.steps, step)
}

// recordFiles records in step which of files, the head's, differ from the
// last ones recorded.
func (r *recording) recordFiles(step *recordedStep, files map[string]string) {
	r.t.Helper()
	// Name the Trash index first: it lists its entries in their order.
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	sort.Slice(paths, func(i, j int) bool {
		if (paths[i] == trashIndexPath) != (paths[j] == trashIndexPath) {
			return paths[i] == trashIndexPath
		}
		return r.named(paths[i], false) < r.named(paths[j], false)
	})
	tree := map[string]string{}
	for _, path := range paths {
		content := r.named(files[path], true)
		tree[r.named(path, true)] = content
	}
	for path, content := range tree {
		if old, ok := r.tree[path]; !ok || old != content {
			if step.Files == nil {
				step.Files = map[string]*string{}
			}
			step.Files[path] = &content
		}
	}
	for path := range r.tree {
		if _, ok := tree[path]; !ok {
			if step.Files == nil {
				step.Files = map[string]*string{}
			}
			step.Files[path] = nil
		}
	}
	r.tree = tree
}

// pendAll notes the content hash and the discussion path that belong to
// each file, to name them after the file once they are seen; a content
// hash belongs to the first file, by path, that has the content.
func (r *recording) pendAll(files map[string]string) {
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		if hash := ContentHash([]byte(files[path])); r.pending[hash] == "" {
			r.pending[hash] = "hash of " + path
		}
		if strings.HasSuffix(path, ".md") {
			r.pending[discussionHash(path)] = "discussion of " + path
		}
	}
}

// discussionHash is the hash that names the discussion of the note at path.
func discussionHash(path string) string {
	return strings.TrimSuffix(strings.TrimPrefix(commentPath(path), "comments/"), ".json")
}

// recordedJSON is v as JSON, with the characters HTML escapes as they are.
func recordedJSON(v any) []byte {
	var out strings.Builder
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(v); err != nil {
		panic(err)
	}
	return []byte(strings.TrimSuffix(out.String(), "\n"))
}

var (
	recordedTime = regexp.MustCompile(`\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(\.\d+)?Z`)
	recordedHex  = regexp.MustCompile(`\b(?:[0-9a-f]{64}|[0-9a-f]{40}|[0-9a-f]{32})\b`)
)

// named replaces in text what differs between runs by nature: the
// notebook's directory, times, revisions (named by the fixture or in the
// order the steps made them), random IDs such as Trash entries', comments'
// and new fmn-ids (in the order they appear), and SHA-256 hashes: a
// discussion's path or a file's content hash after the file, others in the
// order they appear. A revision the notebook never had stays as it is. With
// register unset, nothing new is named.
func (r *recording) named(text string, register bool) string {
	text = strings.ReplaceAll(text, r.s.Root, "<root>")
	text = recordedTime.ReplaceAllString(text, "<time>")
	return recordedHex.ReplaceAllStringFunc(text, func(hex string) string {
		if name, ok := r.names[hex]; ok {
			return name
		}
		if !register || len(hex) == 40 {
			return hex
		}
		if owner, ok := r.pending[hex]; ok {
			kind, path, _ := strings.Cut(owner, " of ")
			r.names[hex] = "<" + kind + " of " + r.named(path, true) + ">"
			return r.names[hex]
		}
		kind := "id"
		if len(hex) == 64 {
			kind = "hash"
		}
		r.count[kind]++
		r.names[hex] = fmt.Sprintf("<%s %d>", kind, r.count[kind])
		// A Trash entry's discussion is named after the entry.
		r.pending[discussionHash("Trash/"+hex+".md")] = "discussion of Trash/" + hex + ".md"
		return r.names[hex]
	})
}

// sorted names items, a list, and sorts them, so that their order does not
// depend on random IDs.
func (r *recording) sorted(items any) []string {
	r.t.Helper()
	var list []json.RawMessage
	raw, _ := json.Marshal(items)
	if err := json.Unmarshal(raw, &list); err != nil {
		r.t.Fatal(err)
	}
	out := []string{}
	for _, item := range list {
		out = append(out, r.named(string(item), true))
	}
	sort.Strings(out)
	return out
}

// files is every file at the head, path to content; a file that is not a
// regular one starts with its mode.
func (r *recording) files() map[string]string {
	r.t.Helper()
	ctx := context.Background()
	files, err := r.s.notes.Files(ctx, gitobj.ID{})
	if err != nil {
		r.t.Fatal(err)
	}
	out := map[string]string{}
	for _, f := range files {
		raw, err := r.s.notes.ReadBlob(ctx, f.Blob)
		if err != nil {
			r.t.Fatal(err)
		}
		out[f.Path] = recordedContent(f.Mode, raw)
	}
	return out
}

func recordedContent(mode uint32, content []byte) string {
	if mode == gitobj.ModeFile {
		return string(content)
	}
	return fmt.Sprintf("<mode %06o> %s", mode, content)
}

// commit is the revision as a step made it.
func (r *recording) commit(revision string) recordedCommit {
	r.t.Helper()
	info, err := r.s.notes.Commit(context.Background(), mustParse(r.t, revision))
	if err != nil {
		r.t.Fatal(err)
	}
	return r.recordedCommit(revision, info.Commit)
}

func (r *recording) recordedCommit(revision string, c gitobj.Commit) recordedCommit {
	out := recordedCommit{Revision: r.named(revision, false), Parents: []string{}, Author: c.Author.Name + " <" + c.Author.Email + ">", Committer: c.Committer.Name + " <" + c.Committer.Email + ">", Message: r.named(c.Message, true)}
	for _, parent := range c.Parents {
		out.Parents = append(out.Parents, r.named(parent.String(), false))
	}
	return out
}

// revisionsSince is every revision after from up to the head, oldest
// first.
func (r *recording) revisionsSince(from string) []string {
	r.t.Helper()
	ctx := context.Background()
	info, err := r.s.notes.Commit(ctx, mustParse(r.t, from))
	if err != nil {
		r.t.Fatal(err)
	}
	infos, err := r.s.notes.Revisions(ctx, info.Seq, 0)
	if err != nil {
		r.t.Fatal(err)
	}
	var out []string
	for _, info := range infos {
		out = append(out, info.ID.String())
	}
	return out
}

// history is every revision up to the head, newest first.
func (r *recording) history() []string {
	r.t.Helper()
	infos, err := r.s.notes.Revisions(context.Background(), 0, 0)
	if err != nil {
		r.t.Fatal(err)
	}
	var out []string
	for i := len(infos) - 1; i >= 0; i-- {
		out = append(out, infos[i].ID.String())
	}
	return out
}

// checkRecorded fails unless the recordings are file's in
// testdata/recorded, step by step; with -update-recorded, it rewrites the
// file.
func checkRecorded(t *testing.T, file string, recordings ...*recording) {
	t.Helper()
	got := map[string][]recordedStep{}
	for _, r := range recordings {
		got[r.name] = r.steps
	}
	path := filepath.Join("testdata", "recorded", file+".json")
	if *updateRecorded {
		var out strings.Builder
		encoder := json.NewEncoder(&out)
		encoder.SetEscapeHTML(false)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(got); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(out.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var want map[string][]recordedStep
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	for _, r := range recordings {
		steps, ok := want[r.name]
		if !ok {
			t.Errorf("%s: no recording", r.name)
			continue
		}
		for i := 0; i < len(r.steps) || i < len(steps); i++ {
			var gotStep, wantStep []byte
			if i < len(r.steps) {
				gotStep, _ = json.Marshal(r.steps[i])
			}
			if i < len(steps) {
				wantStep, _ = json.Marshal(steps[i])
			}
			if string(gotStep) != string(wantStep) {
				t.Errorf("%s, step %d: the Git backend's answer differs\n got %s\nwant %s", r.name, i, gotStep, wantStep)
			}
		}
	}
	if len(want) != len(recordings) {
		t.Errorf("%d recordings, the file has %d", len(recordings), len(want))
	}
}
