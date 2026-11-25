package decision

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// RedactNotebook gives fn every record that belongs to notebook, as a
// notebook purge cleans what quoted the purged content outside it: a record
// that names the notebook as a project or notebook anywhere in its body, such
// as its jobs, settings and citation checks, and the results of its jobs. fn
// returns the record's new body and whether it changed. With write, each
// changed record is written back under its lock, after fn runs again on the
// body the lock found; otherwise nothing changes. It returns how many records
// fn changed. A record fn refuses is left as it was; the others are still
// redacted, and the error says how many were refused. No record's content is
// returned or logged.
func (j *Records) RedactNotebook(ctx context.Context, notebook string, fn func([]byte) ([]byte, bool, error), write bool) (int, error) {
	if notebook == "" || fn == nil {
		return 0, ErrStorage
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	rows, err := j.db.Pool.Query(ctx, `SELECT name FROM jev_records WHERE org = $1
		AND jsonb_path_exists(body, '$.** ? (@.project == $p || @.notebook == $p)', jsonb_build_object('p', $2::text))
		ORDER BY name`, j.db.Org, notebook)
	if err != nil {
		return 0, ErrStorage
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, ErrStorage
	}
	// A job's results name the job, not the notebook.
	var prefixes []string
	for _, name := range names {
		if id, ok := strings.CutPrefix(name, "jobs/"); ok {
			prefixes = append(prefixes, "results/"+strings.TrimSuffix(id, ".json")+"-")
		}
	}
	if len(prefixes) > 0 {
		rows, err := j.db.Pool.Query(ctx, `SELECT name FROM jev_records WHERE org = $1 AND name ^@ ANY($2::text[]) ORDER BY name`, j.db.Org, prefixes)
		if err != nil {
			return 0, ErrStorage
		}
		results, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return 0, ErrStorage
		}
		names = append(names, results...)
	}
	slices.Sort(names)
	names = slices.Compact(names)
	changed, skipped := 0, 0
	var refusal error
	for _, name := range names {
		if !recordName(name) {
			continue
		}
		raw, err := j.read(ctx, j.db.Pool, name)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return changed, ErrStorage
		}
		_, redacted, err := fn(raw)
		if err != nil {
			skipped, refusal = skipped+1, err
			continue
		}
		if !redacted {
			continue
		}
		if !write {
			changed++
			continue
		}
		var refused error
		err = j.locked(name, func(ctx context.Context, tx pgx.Tx) error {
			raw, err := j.read(ctx, tx, name)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			next, redacted, err := fn(raw)
			if err != nil {
				refused = err
				return err
			}
			if !redacted || bytes.Equal(next, raw) {
				return nil
			}
			return j.put(ctx, tx, name, next)
		})
		if refused != nil {
			skipped, refusal = skipped+1, refused
			continue
		}
		if err != nil {
			return changed, err
		}
		changed++
	}
	if skipped > 0 {
		return changed, errors.Join(fmt.Errorf("%d Jev records cannot be redacted and are left as they are", skipped), refusal)
	}
	return changed, nil
}
