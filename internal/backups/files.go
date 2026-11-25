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

type fileState struct {
	Version  int                `json:"version"`
	Bindings map[string]Binding `json:"bindings"`
}

// ImportFiles copies bindings.json into the shared backups document with no
// leases, checking it as the file-based release does when it opens.
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
	body, err := json.Marshal(sharedState{Version: file.Version, Bindings: file.Bindings})
	if err != nil {
		return filestate.Sum{}, err
	}
	if len(body) > maxStateBytes {
		return filestate.Sum{}, errors.New("managed backup configuration exceeds the state size limit")
	}
	if _, err = db.WriteDocument(ctx, tx, stateKind, stateID, 0, body); err != nil {
		return filestate.Sum{}, err
	}
	canonical, err := filestate.JSON(raw)
	if err != nil {
		return filestate.Sum{}, err
	}
	tally.Add(bindingsFile, canonical)
	return tally.Sum(), nil
}

// ExportFiles writes the shared bindings as bindings.json. Leases are left
// behind, as the file-based release's restart ended them: a worker leases
// again, and a token that no gateway revoked expires at GitHub within the hour
// it was minted for. The document's version only fences gateways' writes.
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
	if err = checkBindings(state.Version, state.Bindings); err != nil {
		return filestate.Sum{}, err
	}
	raw, err := json.MarshalIndent(fileState{Version: state.Version, Bindings: state.Bindings}, "", "  ")
	if err != nil {
		return filestate.Sum{}, err
	}
	if len(raw) > maxStateBytes {
		return filestate.Sum{}, errors.New("managed backup configuration exceeds the file-based release's state size limit")
	}
	if err = out.Write(bindingsFile, raw); err != nil {
		return filestate.Sum{}, err
	}
	canonical, err := filestate.JSON(raw)
	if err != nil {
		return filestate.Sum{}, err
	}
	tally.Add(bindingsFile, canonical)
	return tally.Sum(), nil
}
