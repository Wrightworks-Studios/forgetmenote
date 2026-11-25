package quicknotes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"time"

	"github.com/jackc/pgx/v5"

	"forgetmenote/internal/workspace"
)

// ErrUnavailable reports capture state that Postgres cannot serve right now.
var ErrUnavailable = errors.New("Quicknotes state unavailable; retry shortly")

const (
	recordLimit    = 65536
	captureLimit   = 10000
	storageTimeout = 10 * time.Second
)

func storageContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), storageTimeout)
}

func unavailable(err error) error { return errors.Join(ErrUnavailable, err) }

// decode accepts exactly one stored value of known fields, as the record
// files did.
func decode(raw []byte, v any) error {
	if len(raw) > recordLimit {
		return errors.New("invalid Quicknotes state")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(v) != nil || decoder.Decode(new(any)) != io.EOF {
		return errors.New("invalid Quicknotes state")
	}
	return nil
}

func (s *Service) settings(account string) (Settings, error) {
	if !validID(account) {
		return Settings{}, workspace.ErrForbidden
	}
	ctx, cancel := storageContext()
	defer cancel()
	var raw []byte
	err := s.db.Pool.QueryRow(ctx, `SELECT settings::text FROM capture_settings WHERE org = $1 AND account = $2`, s.db.Org, account).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return defaultSettings(), nil
	}
	if err != nil {
		return Settings{}, unavailable(err)
	}
	return parseSettings(raw)
}

func parseSettings(raw []byte) (Settings, error) {
	var value Settings
	if err := decode(raw, &value); err != nil {
		return Settings{}, err
	}
	if value.Generation == 0 || (value.Mode != "manual" && value.Mode != "suggest" && value.Mode != "automatic") || len(value.ApprovedDestinations) > 128 {
		return Settings{}, errors.New("invalid Quicknotes settings")
	}
	return value, nil
}

// writeSettings stores next only over the generation it replaces, so of two
// gateways updating one account's settings the later one conflicts.
func (s *Service) writeSettings(account string, previous uint64, next Settings) error {
	raw, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if len(raw) > recordLimit {
		return errors.New("Quicknotes record exceeds capacity")
	}
	ctx, cancel := storageContext()
	defer cancel()
	query := `UPDATE capture_settings SET generation = $3, settings = $4::jsonb WHERE org = $1 AND account = $2 AND generation = $5`
	if previous == defaultSettings().Generation {
		// An account keeps no settings row until its first save.
		query = `INSERT INTO capture_settings (org, account, generation, settings) VALUES ($1, $2, $3, $4::jsonb)
			ON CONFLICT (org, account) DO UPDATE SET generation = EXCLUDED.generation, settings = EXCLUDED.settings
			WHERE capture_settings.generation = $5`
	}
	tag, err := s.db.Pool.Exec(ctx, query, s.db.Org, account, int64(next.Generation), string(raw), int64(previous))
	if err != nil {
		return unavailable(err)
	}
	if tag.RowsAffected() != 1 {
		return workspace.ErrConflict
	}
	return nil
}

// readRecord returns a capture with the version it was read at, or
// os.ErrNotExist when the account has no such capture.
func (s *Service) readRecord(account, id string) (record, error) {
	if !validID(account) || !validID(id) {
		return record{}, workspace.ErrForbidden
	}
	ctx, cancel := storageContext()
	defer cancel()
	var version int64
	var raw []byte
	err := s.db.Pool.QueryRow(ctx, `SELECT version, record::text FROM captures WHERE org = $1 AND account = $2 AND id = $3`, s.db.Org, account, id).Scan(&version, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return record{}, os.ErrNotExist
	}
	if err != nil {
		return record{}, unavailable(err)
	}
	return parseRecord(account, id, version, raw)
}

func parseRecord(account, id string, version int64, raw []byte) (record, error) {
	var r record
	if err := decode(raw, &r); err != nil {
		return r, err
	}
	if r.Schema != 1 || r.ID != id || r.Actor != account || r.InboxProject != "inbox-"+account || r.InboxPath != "quicknote-"+id+".md" || r.SaveHash == "" {
		return r, errors.New("invalid capture identity")
	}
	r.version = version
	return r, nil
}

// writeRecord stores r over the version it was read at, or creates it, and
// advances r to the stored version. A capture another gateway changed or
// created first conflicts.
func (s *Service) writeRecord(r *record) error {
	if !validID(r.Actor) || !validID(r.ID) {
		return workspace.ErrForbidden
	}
	stored := *r
	stored.Title = ""
	stored.Excerpt = ""
	stored.CanUndo = false
	raw, err := json.Marshal(stored)
	if err != nil {
		return err
	}
	if len(raw) > recordLimit {
		return errors.New("Quicknotes record exceeds capacity")
	}
	ctx, cancel := storageContext()
	defer cancel()
	var next int64
	if r.version == 0 {
		err = s.db.Pool.QueryRow(ctx, `INSERT INTO captures (org, account, id, version, created_at, record) VALUES ($1, $2, $3, 1, $4, $5::jsonb)
			ON CONFLICT DO NOTHING RETURNING version`, s.db.Org, r.Actor, r.ID, r.CreatedAt, string(raw)).Scan(&next)
	} else {
		err = s.db.Pool.QueryRow(ctx, `UPDATE captures SET version = version + 1, record = $5::jsonb
			WHERE org = $1 AND account = $2 AND id = $3 AND version = $4 RETURNING version`, s.db.Org, r.Actor, r.ID, r.version, string(raw)).Scan(&next)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return workspace.ErrConflict
	}
	if err != nil {
		return unavailable(err)
	}
	r.version = next
	return nil
}

func (s *Service) captureCount(account string) (int, error) {
	ctx, cancel := storageContext()
	defer cancel()
	var n int
	if err := s.db.Pool.QueryRow(ctx, `SELECT count(*) FROM captures WHERE org = $1 AND account = $2`, s.db.Org, account).Scan(&n); err != nil {
		return 0, unavailable(err)
	}
	return n, nil
}

// recentRecords returns an account's newest captures, newest first.
func (s *Service) recentRecords(account string, limit int) ([]record, error) {
	ctx, cancel := storageContext()
	defer cancel()
	rows, err := s.db.Pool.Query(ctx, `SELECT id, version, record::text FROM captures WHERE org = $1 AND account = $2
		ORDER BY created_at DESC, id LIMIT $3`, s.db.Org, account, limit)
	if err != nil {
		return nil, unavailable(err)
	}
	defer rows.Close()
	records := []record{}
	for rows.Next() {
		var id string
		var version int64
		var raw []byte
		if err = rows.Scan(&id, &version, &raw); err != nil {
			return nil, unavailable(err)
		}
		r, err := parseRecord(account, id, version, raw)
		if err != nil {
			return nil, err
		}
		records = append(records, r)
	}
	if err = rows.Err(); err != nil {
		return nil, unavailable(err)
	}
	return records, nil
}

// unfinished returns the account/id keys, in order, of captures the background
// scan may need to recover or sort; it rechecks each one as it reads it.
func (s *Service) unfinished() ([]string, error) {
	ctx, cancel := storageContext()
	defer cancel()
	rows, err := s.db.Pool.Query(ctx, `SELECT account || '/' || id FROM captures
		WHERE org = $1 AND (record->>'status' IN ('saving', 'inbox', 'suggested') OR record->'pending' IS NOT NULL)
		ORDER BY account, id`, s.db.Org)
	if err != nil {
		return nil, unavailable(err)
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}
