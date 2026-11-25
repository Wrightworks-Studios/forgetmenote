package client

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"forgetmenote/internal/durable"
)

type EditResult struct {
	ID        string `json:"id"`
	Path      string `json:"path"`
	Revision  string `json:"revision"`
	Unchanged bool   `json:"unchanged"`
}
type DraftError struct {
	ID, Path string
	Err      error
}

func (e *DraftError) Error() string {
	return e.Err.Error() + "; draft retained at " + e.Path + " (resume " + e.ID + ")"
}
func (e *DraftError) Unwrap() error { return e.Err }

func (c *Client) Edit(ctx context.Context, project, path, dir string, editor func(string) error) (EditResult, error) {
	user, generation, err := c.editAuthority(ctx, project)
	if err != nil {
		return EditResult{}, err
	}
	var doc struct {
		Content, Revision, NoteID string
		Editable                  bool
	}
	if err := c.Call(ctx, "read_document", map[string]any{"project": project, "path": path}, &doc); err != nil {
		return EditResult{}, err
	}
	if !doc.Editable {
		return EditResult{}, &Error{Status: 403, Code: "read_only", Message: "this document is read-only; edit GitHub-authoritative content in its upstream repository"}
	}
	if len(doc.Content) <= 1<<20 {
		var live struct{ Pending bool }
		if err := c.Call(ctx, "read_live_note", map[string]string{"project": project, "path": path}, &live); err != nil {
			return EditResult{}, err
		}
		if live.Pending {
			return EditResult{}, &Error{Status: 409, Code: "live_pending", Message: "checkpoint pending collaboration edits before opening a whole-file edit"}
		}
	}
	key := make([]byte, 16)
	rand.Read(key)
	id := hex.EncodeToString(key)
	if err = os.MkdirAll(dir, 0700); err != nil {
		return EditResult{}, err
	}
	folder := filepath.Join(dir, id)
	if err = os.Mkdir(folder, 0700); err != nil {
		return EditResult{}, err
	}
	record := EditRecord{ID: id, Server: c.Server, User: user, Project: project, Path: path, NoteID: doc.NoteID, Revision: doc.Revision, Generation: generation}
	raw, _ := json.Marshal(record)
	for name, data := range map[string][]byte{"session.json": raw, "base.md": []byte(doc.Content), "note.md": []byte(doc.Content)} {
		if err = privateWrite(filepath.Join(folder, name), data); err != nil {
			return EditResult{}, &DraftError{id, filepath.Join(folder, "note.md"), err}
		}
	}
	return c.ResumeEdit(ctx, dir, id, editor)
}

type EditRecord struct {
	CurrentNoteID   string `json:"currentNoteId,omitempty"`
	CurrentRevision string `json:"currentRevision,omitempty"`
	Rebased         bool   `json:"rebased,omitempty"`
	ID              string `json:"id"`
	Server          string `json:"server"`
	User            string `json:"user"`
	Project         string `json:"project"`
	Path            string `json:"path"`
	NoteID          string `json:"noteId,omitempty"`
	Revision        string `json:"revision"`
	Generation      uint64 `json:"authorityGeneration"`
}

func (c *Client) editAuthority(ctx context.Context, project string) (string, uint64, error) {
	var caps struct {
		User     string
		Projects []struct {
			ID        string
			Authority struct {
				Type       string
				Generation uint64
			}
		}
	}
	if err := c.Call(ctx, "capabilities", struct{}{}, &caps); err != nil {
		return "", 0, err
	}
	for _, p := range caps.Projects {
		if p.ID == project {
			if p.Authority.Type != "forgetmenote" {
				return "", 0, &Error{Status: 403, Code: "read_only", Message: "notebook is GitHub-authoritative"}
			}
			return caps.User, p.Authority.Generation, nil
		}
	}
	return "", 0, &Error{Status: 403, Code: "forbidden", Message: "notebook access is unavailable"}
}
func readEdit(dir, id string) (EditRecord, string, error) {
	var r EditRecord
	if !regexp.MustCompile(`^[a-f0-9]{32}$`).MatchString(id) {
		return r, "", errors.New("invalid edit ID")
	}
	folder := filepath.Join(dir, id)
	st, err := os.Lstat(folder)
	if err != nil {
		return r, "", err
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return r, "", errors.New("invalid edit directory")
	}
	raw, err := regularFile(filepath.Join(folder, "session.json"))
	if err != nil {
		return r, "", err
	}
	if json.Unmarshal(raw, &r) != nil || r.ID != id {
		return r, "", errors.New("invalid edit record")
	}
	return r, folder, nil
}
func ListEdits(dir string) ([]EditRecord, error) {
	out := []EditRecord{}
	files, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		if f.IsDir() {
			r, _, e := readEdit(dir, f.Name())
			if e != nil {
				return nil, e
			}
			out = append(out, r)
		}
	}
	return out, nil
}
func (c *Client) ResumeEdit(ctx context.Context, dir, id string, editor func(string) error) (EditResult, error) {
	r, folder, err := readEdit(dir, id)
	if err != nil {
		return EditResult{}, err
	}
	path := filepath.Join(folder, "note.md")
	fail := func(err error) (EditResult, error) { return EditResult{}, &DraftError{id, path, err} }
	if r.Server != c.Server {
		return fail(errors.New("select the edit's original server before resuming"))
	}
	if _, err = regularFile(path); err != nil {
		return fail(err)
	}
	if editor != nil {
		if err = editor(path); err != nil {
			return fail(fmt.Errorf("editor failed: %w", err))
		}
	}
	content, err := regularFile(path)
	if err != nil {
		return fail(err)
	}
	base, err := regularFile(filepath.Join(folder, "base.md"))
	if err != nil {
		return fail(err)
	}
	if string(content) == string(base) && !r.Rebased {
		if err = os.RemoveAll(folder); err != nil {
			return fail(err)
		}
		return EditResult{ID: id, Path: r.Path, Revision: r.Revision, Unchanged: true}, nil
	}
	user, generation, err := c.editAuthority(ctx, r.Project)
	if err != nil {
		return fail(err)
	}
	if user != r.User || generation != r.Generation {
		return fail(errors.New("account or notebook authority changed since this edit began"))
	}
	var out struct{ Revision string }
	err = c.Call(ctx, "save_note", map[string]any{"project": r.Project, "path": r.Path, "content": string(content), "revision": r.Revision, "authorityGeneration": r.Generation, "requireCheckpoint": true}, &out)
	if err != nil {
		var current struct {
			Content, Revision string
			Metadata          struct {
				ID string `json:"id"`
			}
		}
		if e := c.Call(ctx, "read_note", map[string]string{"project": r.Project, "path": r.Path}, &current); e == nil {
			if current.Content == string(content) && current.Metadata.ID == r.NoteID && current.Revision != "" {
				// A lost response can follow a successful commit. Establish the accepted
				// bytes and authority by reading; never repeat the mutation blindly.
				if user, gen, e := c.editAuthority(ctx, r.Project); e == nil && user == r.User && gen == r.Generation {
					if e = os.RemoveAll(folder); e != nil {
						return fail(e)
					}
					return EditResult{ID: id, Path: r.Path, Revision: current.Revision}, nil
				}
			}
			if e = privateWrite(filepath.Join(folder, "current.md"), []byte(current.Content)); e != nil {
				return fail(errors.Join(err, e))
			}
			r.CurrentRevision = current.Revision
			r.CurrentNoteID = current.Metadata.ID
			raw, _ := json.Marshal(r)
			if e = privateWrite(filepath.Join(folder, "session.json"), raw); e != nil {
				return fail(errors.Join(err, e))
			}
		}
		return fail(err)
	}
	if out.Revision == "" {
		return fail(errors.New("save response omitted its committed revision"))
	}
	if err = os.RemoveAll(folder); err != nil {
		return EditResult{ID: id, Path: r.Path, Revision: out.Revision}, fmt.Errorf("saved, but local edit cleanup failed: %w", err)
	}
	return EditResult{ID: id, Path: r.Path, Revision: out.Revision}, nil
}

// RebaseEdit is explicit: use the revision of current.md already fetched during
// conflict recovery. A further server change will still conflict on submission.
func RebaseEdit(dir, id string) error {
	r, folder, err := readEdit(dir, id)
	if err != nil {
		return err
	}
	if r.CurrentRevision == "" {
		return errors.New("no fetched conflict revision to rebase onto")
	}
	if r.NoteID != r.CurrentNoteID {
		return errors.New("the server path now identifies a different note; retain this draft and choose a new destination")
	}
	if _, err = regularFile(filepath.Join(folder, "current.md")); err != nil {
		return err
	}
	r.Revision, r.CurrentRevision, r.Rebased = r.CurrentRevision, "", true
	raw, _ := json.Marshal(r)
	return privateWrite(filepath.Join(folder, "session.json"), raw)
}

func regularFile(path string) ([]byte, error) {
	st, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, errors.New("expected an ordinary file, not a symlink or special file")
	}
	return os.ReadFile(path)
}

// syncPrivateDirectory makes a completed rename durable; tests replace it to inject faults.
var syncPrivateDirectory = durable.SyncDir

func privateWrite(path string, data []byte) error {
	return durable.WriteFile(path, ".fmn-*", data, syncPrivateDirectory)
}
