package backups

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"

	"forgetmenote/internal/filestate"
	"forgetmenote/internal/pg"

	"github.com/jackc/pgx/v5"
)

// bindingsFile is where the file-based release kept managed backup bindings.
// Its leases lived in its memory and ended, revoked, when it stopped.
const bindingsFile = "managed-backups/bindings.json"

// drivesFile keeps the drive destinations beside it. The file-based release
// has no drive destinations and never reads it; an import brings them back,
// with their refresh tokens and passwords still sealed under the drive seal
// key, which neither the database nor the export holds.
const drivesFile = "managed-backups/drives.json"

type fileState struct {
	Version  int                `json:"version"`
	Bindings map[string]Binding `json:"bindings"`
}
type drivesState struct {
	Version int                    `json:"version"`
	Drives  map[string]storedDrive `json:"drives"`
}

// ImportFiles copies bindings.json and drives.json into the shared backups
// document with no leases, checking them as the file-based release does when
// it opens.
func ImportFiles(ctx context.Context, db *pg.DB, tx pgx.Tx, src *filestate.Source) (filestate.Sum, error) {
	var tally filestate.Tally
	raw, err := src.Read(bindingsFile)
	if errors.Is(err, fs.ErrNotExist) {
		return tally.Sum(), nil
	}
	if err != nil {
		return filestate.Sum{}, err
	}
	var file fileState
	if len(raw) > maxStateBytes || json.Unmarshal(raw, &file) != nil {
		return filestate.Sum{}, errors.New("unsupported or damaged managed backup state")
	}
	if err = checkBindings(file.Version, file.Bindings); err != nil {
		return filestate.Sum{}, err
	}
	state := sharedState{Version: file.Version, Bindings: file.Bindings}
	drives, err := src.Read(drivesFile)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return filestate.Sum{}, err
	}
	if err == nil {
		var file drivesState
		if len(drives) > maxStateBytes || json.Unmarshal(drives, &file) != nil || file.Version != 1 || file.Drives == nil {
			return filestate.Sum{}, errors.New("unsupported or damaged drive backup state")
		}
		if err = checkDrives(file.Drives); err != nil {
			return filestate.Sum{}, err
		}
		if len(file.Drives) > 0 {
			state.Version, state.Drives = 3, file.Drives
		}
	}
	body, err := json.Marshal(state)
	if err != nil {
		return filestate.Sum{}, err
	}
	if len(body) > maxStateBytes {
		return filestate.Sum{}, errors.New("managed backup configuration exceeds the state size limit")
	}
	if _, err = db.WriteDocument(ctx, tx, stateKind, stateID, 0, body); err != nil {
		return filestate.Sum{}, err
	}
	for name, raw := range map[string][]byte{bindingsFile: raw, drivesFile: drives} {
		if raw == nil {
			continue
		}
		canonical, err := filestate.JSON(raw)
		if err != nil {
			return filestate.Sum{}, err
		}
		tally.Add(name, canonical)
	}
	return tally.Sum(), nil
}

// ExportFiles writes the shared bindings as bindings.json, in the version the
// file-based release reads, and any drive destinations as drives.json. Leases
// are left behind, as the file-based release's restart ended them: a worker
// leases again, and a token that no gateway revoked expires at its provider.
// The document's version only fences gateways' writes.
func ExportFiles(ctx context.Context, db *pg.DB, q pg.Querier, out *filestate.Target) (filestate.Sum, error) {
	var tally filestate.Tally
	version, body, err := db.ReadDocument(ctx, q, stateKind, stateID, -1)
	if err != nil || version == 0 {
		return tally.Sum(), err
	}
	var state sharedState
	if err = json.Unmarshal(body, &state); err != nil {
		return filestate.Sum{}, fmt.Errorf("%s state: %w", stateKind, err)
	}
	if err = checkState(state); err != nil {
		return filestate.Sum{}, err
	}
	files := map[string]any{bindingsFile: fileState{Version: min(state.Version, 2), Bindings: state.Bindings}}
	if len(state.Drives) > 0 {
		files[drivesFile] = drivesState{Version: 1, Drives: state.Drives}
	}
	for name, value := range files {
		raw, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			return filestate.Sum{}, err
		}
		if len(raw) > maxStateBytes {
			return filestate.Sum{}, errors.New("managed backup configuration exceeds the file-based release's state size limit")
		}
		if err = out.Write(name, raw); err != nil {
			return filestate.Sum{}, err
		}
		canonical, err := filestate.JSON(raw)
		if err != nil {
			return filestate.Sum{}, err
		}
		tally.Add(name, canonical)
	}
	return tally.Sum(), nil
}
