package web

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"

	"forgetmenote/internal/automation"
	"forgetmenote/internal/sources"
	"forgetmenote/internal/workspace"
)

type driveItem struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	MIME     string `json:"mimeType"`
	Version  string `json:"version"`
	Modified string `json:"modifiedTime"`
}

func driveExport(mime, name string) (string, string, bool) {
	switch mime {
	case "application/vnd.google-apps.document":
		return "text/markdown", "document.md", true
	case "application/vnd.google-apps.spreadsheet":
		return "text/csv", "sheet.csv", true
	case "application/vnd.google-apps.presentation":
		return "text/plain", "slides.txt", true
	case "application/vnd.google-apps.drawing":
		return "image/png", "drawing.png", true
	case "application/vnd.google-apps.script":
		return "application/vnd.google-apps.script+json", "script.json", true
	}
	name = path.Base(name)
	if workspace.SourceFormatForName(name).MIME == "" {
		format := workspace.SourceFormatForMIME(mime)
		if len(format.Extensions) == 0 {
			return "", name, false
		}
		name += format.Extensions[0]
	}
	return "", name, true
}

func (s *Server) runDriveSync(ctx context.Context, r *automation.Run, save func() error) error {
	if err := r.Spec.Validate(); err != nil {
		return err
	}
	policy := r.Spec.Drive
	_, err := s.clientAccess(ctx, r.Spec.Actor, r.Spec.Project, "edit")
	if err != nil {
		return err
	}
	state, err := s.automations.Queue.ReadDrive(r.Spec.ID)
	if err != nil {
		return err
	}
	if state.Complete || state.Policy != automation.Hash(policy) {
		state.Policy = automation.Hash(policy)
		state.Complete = false
		state.Folders = nil
		state.Visited = map[string]bool{}
		for _, id := range policy.Folders {
			state.Folders = append(state.Folders, automation.DriveFolder{ID: id})
			state.Visited[id] = true
		}
		for id, v := range state.Files {
			v.Seen = false
			state.Files[id] = v
		}
	}
	checkpoint := func() error {
		if r.DryRun {
			return nil
		}
		return s.automations.Queue.SaveDrive(r.Spec.ID, state)
	}
	if err = checkpoint(); err != nil {
		return err
	}
	processed, unchanged, unsupported := 0, 0, 0
	limit := policy.MaxFiles
	if limit == 0 {
		limit = 32
	}
	// Unchanged trees must also yield: one very large folder cannot monopolize
	// the deterministic worker while contributing no imports to the file budget.
	for pages := 0; len(state.Folders) > 0 && processed < limit && pages < 32; pages++ {
		folder := state.Folders[0]
		result, e := s.automationCall(ctx, r, automation.Call{Connection: policy.Connection, Operation: "list", Arguments: map[string]any{"folder": folder.ID, "pageToken": folder.Cursor}})
		if e != nil {
			return e
		}
		if !result.Receipt.Complete {
			return errors.New("Drive listing incomplete")
		}
		var page struct {
			Files      []driveItem `json:"files"`
			Next       string      `json:"nextPageToken"`
			Incomplete bool        `json:"incompleteSearch"`
		}
		raw, _ := json.Marshal(result.Data)
		if json.Unmarshal(raw, &page) != nil || page.Files == nil || page.Incomplete || len(page.Files) > 1000 {
			return errors.New("invalid or incomplete Drive listing")
		}
		for _, file := range page.Files {
			if file.ID == "" || len(file.ID) > 128 || strings.ContainsAny(file.ID, "/\\\x00") || file.Name == "" {
				return errors.New("invalid Drive file identity")
			}
			if file.MIME == "application/vnd.google-apps.folder" {
				if !state.Visited[file.ID] {
					if len(state.Visited) >= 10000 {
						return errors.New("Drive folder limit reached; narrow the selected roots")
					}
					state.Visited[file.ID] = true
					state.Folders = append(state.Folders, automation.DriveFolder{ID: file.ID})
				}
				continue
			}
			version := automation.Hash([]string{file.Version, file.Modified, file.Name, file.MIME})
			old := state.Files[file.ID]
			old.Seen = true
			if old.Version == version && old.Path != "" {
				state.Files[file.ID] = old
				unchanged++
				continue
			}
			format, filename, ok := driveExport(file.MIME, file.Name)
			if !ok {
				unsupported++
				continue
			}
			if processed >= limit {
				if err = checkpoint(); err != nil {
					return err
				}
				r.Result = fmt.Sprintf("Imported %d, unchanged %d, unsupported %d; continuation pending", processed, unchanged, unsupported)
				return save()
			}
			op := "download"
			args := map[string]any{"id": file.ID}
			if format != "" {
				op = "export"
				args["mimeType"] = format
			}
			content, e := s.automationCall(ctx, r, automation.Call{Connection: policy.Connection, Operation: op, Arguments: args})
			if e != nil {
				return e
			}
			if !content.Receipt.Complete {
				return errors.New("Drive export incomplete")
			}
			var payload struct {
				Data string `json:"data"`
			}
			raw, _ = json.Marshal(content.Data)
			if json.Unmarshal(raw, &payload) != nil {
				return errors.New("invalid Drive export")
			}
			body, e := base64.StdEncoding.Strict().DecodeString(payload.Data)
			if e != nil || len(body) == 0 {
				return errors.New("invalid Drive file data")
			}
			release, e := s.acquireSourceWork(ctx)
			if e != nil {
				return e
			}
			capture, e := sources.Prepare(ctx, filename, "https://drive.google.com/file/d/"+file.ID+"/view", body)
			release()
			if e != nil {
				return e
			}
			capture.Record.ID = automation.Hash([]string{policy.Connection, file.ID})[:32]
			capture.OperationID = "drive-" + automation.Hash([]string{r.Spec.ID, file.ID, version})
			capture.Record.CapturedAt = file.Modified
			if !r.DryRun {
				if err = s.checkAutomationEvidence(ctx, r, true); err != nil {
					return err
				}
				var imported workspace.Document
				var dest *string
				if old.Path == "" {
					dest = &policy.Destination
				}
				auth := ctx.Value(automationKey{}).(automationAuth)
				err = s.catalog.WithProjectAuthority(auth.Spec.Actor, auth.Spec.Project, auth.Account, "edit", auth.Epoch, auth.ProjectEpoch, func(st *workspace.Store) error {
					var e error
					imported, e = st.ImportSource(ctx, capture, old.Path, old.Revision, file.Name, dest)
					return e
				})
				if err != nil {
					return err
				}
				state.Files[file.ID] = automation.DriveFile{Path: imported.Path, Revision: imported.Revision, Version: version, Seen: true}
				if err = checkpoint(); err != nil {
					return err
				}
			}
			processed++
		}
		if page.Next != "" {
			if page.Next == folder.Cursor {
				return errors.New("Drive pagination did not advance")
			}
			state.Folders[0].Cursor = page.Next
		} else {
			state.Folders = state.Folders[1:]
		}
		if err = checkpoint(); err != nil {
			return err
		}
	}
	missing := 0
	if len(state.Folders) == 0 {
		state.Complete = true
		for _, file := range state.Files {
			if !file.Seen {
				missing++
			}
		}
	}
	if err = checkpoint(); err != nil {
		return err
	}
	r.Result = fmt.Sprintf("Imported %d, unchanged %d, unsupported %d, upstream missing %d (retained); complete: %t", processed, unchanged, unsupported, missing, state.Complete)
	return save()
}
