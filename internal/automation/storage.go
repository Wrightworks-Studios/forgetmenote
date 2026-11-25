package automation

import (
	"context"
	"errors"
	"forgetmenote/internal/pg"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// records is the automation state an instance's gateways share: private
// records by name in Postgres, as the files of the automations directory were.
// Every write names the version its writer read, so a write that overlaps
// another gateway's, as one can when the queue lock's connection drops, is
// refused with pg.ErrConflict instead of replacing it.
type records struct {
	db *pg.DB
	// blocked is the failure to take the queue's cross-gateway lock, which every
	// access in that critical section then reports instead of running unserialized.
	blocked error
}

var automationRows = pg.NewVersionedRows("automation_records", "body", "org", "name")

type storedName struct {
	name    string
	version int64
}

func storageContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 10*time.Second)
}

func validRecordName(name string) bool {
	return name != "" && len(name) <= 256 && !strings.ContainsRune(name, 0) && !strings.Contains(name, "..")
}

// read returns a record's bytes and the version they were read at,
// os.ErrNotExist when there is none, or an error when it exceeds limit.
func (s records) read(name string, limit int) ([]byte, int64, error) {
	if s.blocked != nil {
		return nil, 0, s.blocked
	}
	if !validRecordName(name) {
		return nil, 0, os.ErrNotExist
	}
	ctx, cancel := storageContext()
	defer cancel()
	version, raw, err := automationRows.Read(ctx, s.db.Pool, s.db.Org, name)
	if err != nil {
		return nil, 0, err
	}
	if version == 0 {
		return nil, 0, os.ErrNotExist
	}
	if len(raw) > limit {
		return nil, 0, errors.New("invalid private automation state")
	}
	return raw, version, nil
}

// version returns a record's version without its bytes, 0 when there is none.
func (s records) version(name string) (int64, error) {
	if s.blocked != nil {
		return 0, s.blocked
	}
	if !validRecordName(name) {
		return 0, nil
	}
	ctx, cancel := storageContext()
	defer cancel()
	return automationRows.Version(ctx, s.db.Pool, s.db.Org, name)
}

// write stores raw over the version its writer read, 0 for a record that must
// not exist yet, and returns the new version. It reports pg.ErrConflict when
// another writer changed or created the record first.
func (s records) write(name string, raw []byte, version int64) (int64, error) {
	if s.blocked != nil {
		return 0, s.blocked
	}
	if !validRecordName(name) {
		return 0, errors.New("invalid private automation record name")
	}
	ctx, cancel := storageContext()
	defer cancel()
	return automationRows.Write(ctx, s.db.Pool, version, raw, s.db.Org, name)
}

// replace stores raw over whatever version the record has now, for a record a
// critical section saves whole without reading it first. A writer between the
// two statements still makes it fail with pg.ErrConflict.
func (s records) replace(name string, raw []byte) error {
	version, err := s.version(name)
	if err == nil {
		_, err = s.write(name, raw, version)
	}
	return err
}

// remove deletes the record read at version, and reports pg.ErrConflict when
// another writer changed or removed it first.
func (s records) remove(name string, version int64) error {
	if s.blocked != nil {
		return s.blocked
	}
	ctx, cancel := storageContext()
	defer cancel()
	return automationRows.Delete(ctx, s.db.Pool, version, s.db.Org, name)
}

// list returns the top-level records whose names end in suffix, in name order.
func (s records) list(suffix string) ([]storedName, error) {
	if s.blocked != nil {
		return nil, s.blocked
	}
	ctx, cancel := storageContext()
	defer cancel()
	rows, err := s.db.Pool.Query(ctx, `SELECT name, version FROM automation_records
		WHERE org = $1 AND right(name, length($2)) = $2 AND strpos(name, '/') = 0 ORDER BY name`, s.db.Org, suffix)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (storedName, error) {
		var n storedName
		return n, row.Scan(&n.name, &n.version)
	})
}
