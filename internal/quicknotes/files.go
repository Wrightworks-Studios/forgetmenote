package quicknotes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"forgetmenote/internal/filestate"
	"forgetmenote/internal/pg"

	"github.com/jackc/pgx/v5"
)

// The file-based release kept each account's captures as
// quicknotes/<account>/<id>.json beside the account's settings.json.
// quicknotes.lock holds no state, and a file with the temporary prefix is a
// write that release never renamed into place and never reads.
const (
	filesDir     = "quicknotes"
	filesLock    = "quicknotes.lock"
	settingsFile = "settings.json"
	tempPrefix   = ".quicknotes-"
)

// ImportFiles copies every account's captures and settings into their rows,
// checking each file as the file-based release does when it reads it. Any
// other file is refused, as that release would never read it.
func ImportFiles(ctx context.Context, db *pg.DB, tx pgx.Tx, src *filestate.Source) (filestate.Sum, error) {
	var tally filestate.Tally
	accounts, err := src.Entries(filesDir)
	if err != nil {
		return filestate.Sum{}, err
	}
	var captures, settings [][]any
	for _, account := range accounts {
		dir := filesDir + "/" + account.Name()
		if account.Name() == filesLock && account.Type().IsRegular() {
			continue
		}
		if !account.IsDir() || !validID(account.Name()) {
			return filestate.Sum{}, fmt.Errorf("%s is not an account's Quicknotes state", dir)
		}
		files, err := src.Entries(dir)
		if err != nil {
			return filestate.Sum{}, err
		}
		// The file-based release refuses to list more.
		if len(files) > captureLimit {
			return filestate.Sum{}, fmt.Errorf("%s exceeds the capture state capacity", dir)
		}
		for _, file := range files {
			if strings.HasPrefix(file.Name(), tempPrefix) {
				continue
			}
			rel := dir + "/" + file.Name()
			id, ok := strings.CutSuffix(file.Name(), ".json")
			if file.Name() != settingsFile && (!ok || !validID(id)) {
				return filestate.Sum{}, fmt.Errorf("%s is not Quicknotes state the file-based release reads", rel)
			}
			raw, err := src.Read(rel)
			if err != nil {
				return filestate.Sum{}, err
			}
			if file.Name() == settingsFile {
				row, err := importSettings(db, account.Name(), raw)
				if err != nil {
					return filestate.Sum{}, fmt.Errorf("%s: %w", rel, err)
				}
				settings = append(settings, row)
			} else {
				row, err := importCapture(db, account.Name(), id, raw)
				if err != nil {
					return filestate.Sum{}, fmt.Errorf("%s: %w", rel, err)
				}
				captures = append(captures, row)
			}
			canonical, err := filestate.JSON(raw)
			if err != nil {
				return filestate.Sum{}, err
			}
			tally.Add(rel, canonical)
		}
	}
	if _, err = tx.CopyFrom(ctx, pgx.Identifier{"captures"}, []string{"org", "account", "id", "version", "created_at", "record"}, pgx.CopyFromRows(captures)); err != nil {
		return filestate.Sum{}, err
	}
	if _, err = tx.CopyFrom(ctx, pgx.Identifier{"capture_settings"}, []string{"org", "account", "generation", "settings"}, pgx.CopyFromRows(settings)); err != nil {
		return filestate.Sum{}, err
	}
	return tally.Sum(), nil
}

// importCapture returns a capture's first row, stored as writeRecord stores it.
func importCapture(db *pg.DB, account, id string, raw []byte) ([]any, error) {
	r, err := parseRecord(account, id, 1, raw)
	if err != nil {
		return nil, err
	}
	// The file-based release has no holder field and refuses one.
	if r.Holder != "" {
		return nil, errors.New("invalid Quicknotes state")
	}
	r.Title, r.Excerpt, r.CanUndo = "", "", false
	body, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	return []any{db.Org, account, id, int64(1), r.CreatedAt, body}, nil
}

func importSettings(db *pg.DB, account string, raw []byte) ([]any, error) {
	value, err := parseSettings(raw)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return []any{db.Org, account, int64(value.Generation), body}, nil
}

// ExportFiles writes every capture and every account's settings as the
// file-based release's files. Row versions only fence gateways' writes and
// are left behind.
func ExportFiles(ctx context.Context, db *pg.DB, q pg.Querier, out *filestate.Target) (filestate.Sum, error) {
	var tally filestate.Tally
	write := func(rel string, value any) error {
		raw, err := json.Marshal(value)
		if err != nil {
			return err
		}
		if len(raw) > recordLimit {
			return fmt.Errorf("%s exceeds the file-based release's record capacity", rel)
		}
		if err = out.Write(rel, raw); err != nil {
			return err
		}
		canonical, err := filestate.JSON(raw)
		if err != nil {
			return err
		}
		tally.Add(rel, canonical)
		return nil
	}
	rows, err := q.Query(ctx, `SELECT account, id, record::text FROM captures WHERE org = $1 ORDER BY account, id`, db.Org)
	if err != nil {
		return filestate.Sum{}, err
	}
	captures, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct{ Account, ID, Record string }])
	if err != nil {
		return filestate.Sum{}, err
	}
	for _, c := range captures {
		if !validID(c.Account) || !validID(c.ID) {
			return filestate.Sum{}, errors.New("invalid capture identity")
		}
		r, err := parseRecord(c.Account, c.ID, 1, []byte(c.Record))
		if err != nil {
			return filestate.Sum{}, err
		}
		// The holder names a gateway, which the file-based release has no
		// field for: it recovers every unfinished capture itself once it
		// starts, as a gateway does for a holder that is gone.
		r.Holder = ""
		if err = write(filesDir+"/"+c.Account+"/"+c.ID+".json", r); err != nil {
			return filestate.Sum{}, err
		}
	}
	if rows, err = q.Query(ctx, `SELECT account, settings::text FROM capture_settings WHERE org = $1 ORDER BY account`, db.Org); err != nil {
		return filestate.Sum{}, err
	}
	settings, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct{ Account, Settings string }])
	if err != nil {
		return filestate.Sum{}, err
	}
	for _, s := range settings {
		if !validID(s.Account) {
			return filestate.Sum{}, errors.New("invalid capture identity")
		}
		value, err := parseSettings([]byte(s.Settings))
		if err != nil {
			return filestate.Sum{}, err
		}
		if err = write(filesDir+"/"+s.Account+"/"+settingsFile, value); err != nil {
			return filestate.Sum{}, err
		}
	}
	return tally.Sum(), nil
}
