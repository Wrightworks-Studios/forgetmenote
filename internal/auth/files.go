package auth

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"

	"forgetmenote/internal/filestate"
	"forgetmenote/internal/pg"

	"github.com/jackc/pgx/v5"
)

// stateFile is where the file-based release kept sign-in state. It never held
// authorization codes or registration windows, which ended with the process.
const stateFile = "oauth.json"

type fileState struct {
	Credentials map[string]APICredential `json:"credentials,omitempty"`
	Clients     map[string]Client        `json:"clients"`
	Access      map[string]Grant         `json:"access"`
	Refresh     map[string]Grant         `json:"refresh"`
	Sessions    map[string]Grant         `json:"sessions,omitempty"`
	Used        map[string]Grant         `json:"usedRefresh,omitempty"`
}

// ImportFiles copies oauth.json into the sign-in document, validating it as the
// document is validated on every read.
func ImportFiles(ctx context.Context, db *pg.DB, tx pgx.Tx, src *filestate.Source) (filestate.Sum, error) {
	var tally filestate.Tally
	raw, err := src.Read(stateFile)
	if errors.Is(err, fs.ErrNotExist) {
		return tally.Sum(), nil
	}
	if err != nil {
		return filestate.Sum{}, err
	}
	var file fileState
	if err = json.Unmarshal(raw, &file); err != nil {
		return filestate.Sum{}, err
	}
	body, err := json.Marshal(file)
	if err != nil {
		return filestate.Sum{}, err
	}
	if err = (&State{}).decode(1, body); err != nil {
		return filestate.Sum{}, err
	}
	if _, err = db.WriteDocument(ctx, tx, stateKind, stateID, 0, body); err != nil {
		return filestate.Sum{}, err
	}
	canonical, err := filestate.JSON(raw)
	if err != nil {
		return filestate.Sum{}, err
	}
	tally.Add(stateFile, canonical)
	return tally.Sum(), nil
}

// ExportFiles writes the sign-in document as oauth.json. Pending authorization
// codes and registration windows are left behind, as a restart dropped them.
func ExportFiles(ctx context.Context, db *pg.DB, q pg.Querier, out *filestate.Target) (filestate.Sum, error) {
	var tally filestate.Tally
	version, body, err := db.ReadDocument(ctx, q, stateKind, stateID, -1)
	if err != nil || version == 0 {
		return tally.Sum(), err
	}
	stored := &State{}
	if err = stored.decode(version, body); err != nil {
		return filestate.Sum{}, err
	}
	raw, err := json.Marshal(fileState{Credentials: stored.Credentials, Clients: stored.Clients, Access: stored.Access, Refresh: stored.Refresh, Sessions: stored.Sessions, Used: stored.Used})
	if err != nil {
		return filestate.Sum{}, err
	}
	if err = out.Write(stateFile, raw); err != nil {
		return filestate.Sum{}, err
	}
	canonical, err := filestate.JSON(raw)
	if err != nil {
		return filestate.Sum{}, err
	}
	tally.Add(stateFile, canonical)
	return tally.Sum(), nil
}
