package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"forgetmenote/internal/pg"

	"github.com/jackc/pgx/v5"
)

// The storage import (`forgetmenote admin storage import`) records that it
// began importing a notebook before it stores anything, and removes the
// record once the notebook verified, or once it removed what it stored. A
// record without a verified notebook is an import that stopped part way or
// before it verified: a process killed, out of memory or cut off from the
// database, which never ran its own clean-up. The next import resumes it,
// and until then no gateway serves the notebook (checkImported); with no
// record, a stored notebook is never imported over.
const storageImportKind = "storage-import"

// BeginStorageImport records that the storage import of notebook id from
// the Git commit source began.
func BeginStorageImport(ctx context.Context, db *pg.DB, id, source string) error {
	record, err := json.Marshal(map[string]string{"source": source})
	if err != nil {
		return err
	}
	_, err = db.Pool.Exec(ctx, `INSERT INTO documents (org, kind, id, version, body) VALUES ($1, $2, $3, 1, $4::jsonb)
		ON CONFLICT (org, kind, id) DO UPDATE SET version = documents.version + 1, body = EXCLUDED.body, updated_at = now()`, db.Org, storageImportKind, id, record)
	return err
}

// Once it verified a notebook, the storage import also records the Git
// commit it imported, which check (`forgetmenote admin storage check`)
// compares with the repository's HEAD: a repository older than the one the
// import read, from an older copy of the data directory or a HEAD moved
// back, holds only commits the store holds, as does the repository of a
// notebook that took revisions after the start.
const storageImportedKind = "storage-imported"

// FinishStorageImport removes the record of notebook id's storage import
// and, in the same transaction, records imported, the Git commit whose
// history the import verified the notes store holds (StorageImported). An
// empty imported, after an import that removed what it stored, removes that
// record too.
func FinishStorageImport(ctx context.Context, db *pg.DB, id, imported string) error {
	return pgx.BeginFunc(ctx, db.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM documents WHERE org = $1 AND kind = $2 AND id = $3`, db.Org, storageImportKind, id); err != nil {
			return err
		}
		if imported == "" {
			_, err := tx.Exec(ctx, `DELETE FROM documents WHERE org = $1 AND kind = $2 AND id = $3`, db.Org, storageImportedKind, id)
			return err
		}
		record, err := json.Marshal(map[string]string{"head": imported})
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO documents (org, kind, id, version, body) VALUES ($1, $2, $3, 1, $4::jsonb)
			ON CONFLICT (org, kind, id) DO UPDATE SET version = documents.version + 1, body = EXCLUDED.body, updated_at = now()`, db.Org, storageImportedKind, id, record)
		return err
	})
}

// StorageImported returns the Git commit the storage import last imported
// notebook id at, or "" if no import recorded one: the notebook was never
// imported, or an earlier build imported it.
func StorageImported(ctx context.Context, db *pg.DB, id string) (string, error) {
	var head string
	err := db.Pool.QueryRow(ctx, `SELECT coalesce(body->>'head', '') FROM documents WHERE org = $1 AND kind = $2 AND id = $3`, db.Org, storageImportedKind, id).Scan(&head)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return head, err
}

// StorageImportUnfinished reports whether a storage import of notebook id
// began and did not finish.
func StorageImportUnfinished(ctx context.Context, db *pg.DB, id string) (bool, error) {
	var unfinished bool
	err := db.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT FROM documents WHERE org = $1 AND kind = $2 AND id = $3)`, db.Org, storageImportKind, id).Scan(&unfinished)
	return unfinished, err
}

// Retire (`forgetmenote admin storage retire -yes`) records, before it
// removes a notebook's previous release files, that it removed them, so
// check tells a notebook whose repository retire removed from one whose
// repository went missing.
const storageRetiredKind = "storage-retired"

// RecordStorageRetired records that retire removes notebook id's previous
// release files, once it verified that the notes store holds the history of
// their repository's HEAD, head, if they had one.
func RecordStorageRetired(ctx context.Context, db *pg.DB, id, head string) error {
	record, err := json.Marshal(map[string]string{"head": head})
	if err != nil {
		return err
	}
	_, err = db.Pool.Exec(ctx, `INSERT INTO documents (org, kind, id, version, body) VALUES ($1, $2, $3, 1, $4::jsonb)
		ON CONFLICT (org, kind, id) DO UPDATE SET version = documents.version + 1, body = EXCLUDED.body, updated_at = now()`, db.Org, storageRetiredKind, id, record)
	return err
}

// StorageRetired returns when retire last removed notebook id's previous
// release files, or the zero time if it never did.
func StorageRetired(ctx context.Context, db *pg.DB, id string) (time.Time, error) {
	var at time.Time
	err := db.Pool.QueryRow(ctx, `SELECT updated_at FROM documents WHERE org = $1 AND kind = $2 AND id = $3`, db.Org, storageRetiredKind, id).Scan(&at)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, nil
	}
	return at, err
}

// StoredNotebooks lists the IDs of the notebooks the stored catalog has,
// sorted, reading the document without the catalog's directory lock, which
// a serving gateway holds: the cutover's check (`forgetmenote admin storage
// check`) runs beside the new release.
func StoredNotebooks(ctx context.Context, db *pg.DB) ([]string, error) {
	version, body, err := db.ReadDocument(ctx, db.Pool, catalogKind, catalogID, -1)
	if err != nil || version == 0 {
		return nil, err
	}
	var envelope catalogEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, err
	}
	if envelope.Version != catalogStorageVersion {
		return nil, fmt.Errorf("unsupported catalog version %d", envelope.Version)
	}
	return slices.Sorted(maps.Keys(envelope.Data.Projects)), nil
}
