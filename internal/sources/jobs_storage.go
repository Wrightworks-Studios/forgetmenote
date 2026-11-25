package sources

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"forgetmenote/internal/blob"
	"forgetmenote/internal/capacity"
	"forgetmenote/internal/pg"
	"forgetmenote/internal/workspace"

	"github.com/jackc/pgx/v5"
)

// errJobChanged reports that another gateway changed a job after it was read.
var errJobChanged = errors.New("source job changed on another gateway")

// A job's objects are its original, page checkpoints and result, named as the
// file-based release named those files.
var jobObject = regexp.MustCompile(`^(original|result\.json|page-[0-9]{5}\.json)$`)

func pageFile(n int) string { return fmt.Sprintf("page-%05d.json", n) }

// objectKey is where run attempt of job id stores the object name. An upload,
// an import and the job's first run store at the paths the file-based release
// kept the files at, source-jobs/<id>/<name>; each later run stores under
// source-jobs/<id>/attempt-<n>/. No two runs store under one key, so a run
// that lost its job to another gateway and stores late never replaces an
// object the row names: the row names only its own runs' objects.
func objectKey(id string, attempt int, name string) string {
	if attempt <= 1 {
		return jobsDir + "/" + id + "/" + name
	}
	return fmt.Sprintf("%s/%s/attempt-%d/%s", jobsDir, id, attempt, name)
}

func (job *sourceJob) originalKey() string { return objectKey(job.ID, job.Fetched, "original") }

// resultKey is the result of the run that finished the job.
func (job *sourceJob) resultKey() string { return objectKey(job.ID, job.Attempt, "result.json") }

func (job *sourceJob) pageKey(n int) string {
	attempt := 1
	for _, run := range job.Runs {
		if run.First <= n {
			attempt = run.Attempt
		}
	}
	return objectKey(job.ID, attempt, pageFile(n))
}

// Reserve the private original and its conversion copy, bounded encoded page
// checkpoints/results (including atomic replacement), and rendering scratch.
// Queue slots alone cannot bound uploads retained after processing completes.
func jobReservation(original int) int64 {
	if original == 0 {
		original = workspace.MaxSourceBytes()
	}
	return 2*int64(original) + 18*int64(workspace.MaxSourceText()) + (64 << 20) + (10000 * 1024) + (1 << 20)
}

func storageContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 10*time.Second)
}

// blobContext bounds one blob storage call, such as an original of
// FMN_LIMIT_SOURCE_BYTES, made while the worker's lock may be held.
func blobContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, 5*time.Minute)
}

func (m *Jobs) putObject(ctx context.Context, key string, raw []byte) error {
	return blob.Write(ctx, m.blobs, key, raw)
}

func (m *Jobs) readObject(ctx context.Context, key string, limit int) ([]byte, error) {
	return blob.Read(ctx, m.blobs, key, int64(limit))
}

// locked runs fn in a transaction holding the instance's source job admission
// lock, so admission counts every gateway's jobs at once.
func (m *Jobs) locked(fn func(context.Context, pgx.Tx) error) error {
	ctx, cancel := storageContext()
	defer cancel()
	return pgx.BeginFunc(ctx, m.db.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('source-jobs ' || $1, 0))`, m.db.Org); err != nil {
			return err
		}
		return fn(ctx, tx)
	})
}

func decodeJob(id string, version, reserved int64, raw []byte) (*sourceJob, error) {
	if len(raw) > 256<<10 {
		return nil, errors.New("invalid or excessive source job record")
	}
	var job sourceJob
	if err := strictRecord(raw, &job); err != nil || job.Version != 1 || job.ID != id || job.Scope.Subject == "" || job.Scope.Notebook == "" || job.Pages < 0 || job.Pages > 10000 || job.TextBytes < 0 || job.TextBytes > workspace.MaxSourceText() || job.Stored < 0 {
		return nil, errors.New("invalid source job manifest")
	}
	switch job.State {
	// A staging job's row committed before its original was stored; no gateway
	// serves it until it is published as queued.
	case "staging", "queued", "running", "ready", "partial", "failed", "canceled", "interrupted":
	default:
		return nil, errors.New("invalid source job state")
	}
	if job.Attempt < 0 || job.Fetched < 0 || job.Fetched > job.Attempt || job.Fetched > 0 && !job.Downloaded {
		return nil, errors.New("invalid source job attempt")
	}
	last := pageRun{Attempt: 1}
	for _, run := range job.Runs {
		if run.Attempt <= last.Attempt || run.Attempt > job.Attempt || run.First <= last.First || run.First > job.Pages {
			return nil, errors.New("invalid source job checkpoints")
		}
		last = run
	}
	job.version, job.reserved = version, reserved
	return &job, nil
}

// load returns the job's row, or nil when there is none.
func (m *Jobs) load(id string) (*sourceJob, error) {
	ctx, cancel := storageContext()
	defer cancel()
	var version, reserved int64
	var raw []byte
	err := m.db.Pool.QueryRow(ctx, `SELECT version, reserved, manifest FROM source_jobs WHERE org = $1 AND id = $2`, m.db.Org, id).Scan(&version, &reserved, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeJob(id, version, reserved, raw)
}

func (m *Jobs) list(ctx context.Context, q pg.Querier) ([]*sourceJob, error) {
	rows, err := q.Query(ctx, `SELECT id, version, reserved, manifest FROM source_jobs WHERE org = $1 ORDER BY id`, m.db.Org)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (*sourceJob, error) {
		var id string
		var version, reserved int64
		var raw []byte
		if err := row.Scan(&id, &version, &reserved, &raw); err != nil {
			return nil, err
		}
		return decodeJob(id, version, reserved, raw)
	})
}

// save writes job over the version it was read at and advances it. A job
// changed or retired since then is errJobChanged.
func (m *Jobs) save(ctx context.Context, q pg.Querier, job *sourceJob) error {
	raw, err := marshalJob(job)
	if err != nil {
		return err
	}
	tag, err := q.Exec(ctx, `UPDATE source_jobs SET version = version + 1, reserved = $4, manifest = $5 WHERE org = $1 AND id = $2 AND version = $3`, m.db.Org, job.ID, job.version, job.reserved, raw)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errJobChanged
	}
	job.version++
	return nil
}
func (m *Jobs) persist(job *sourceJob) error {
	ctx, cancel := storageContext()
	defer cancel()
	return m.save(ctx, m.db.Pool, job)
}

// admitStorage runs under the admission lock. Reservations replace stored-byte
// accounting for staging, queued and draining jobs; other jobs charge the
// objects they stored and any scratch a failed cleanup left on this gateway.
func (m *Jobs) admitStorage(ctx context.Context, tx pgx.Tx, replace string, reservation int64) error {
	jobs, err := m.list(ctx, tx)
	if err != nil {
		return err
	}
	if replace == "" && len(jobs) >= capacity.SourceJobCount.Limit() {
		return fmt.Errorf("%w; %v", ErrJobCapacity, capacity.SourceJobCount.Error())
	}
	used := reservation
	for _, job := range jobs {
		if job.ID == replace {
			continue
		}
		if job.reserved > 0 {
			used += job.reserved
		} else {
			scratch, err := scratchBytes(filepath.Join(m.scratch, job.ID))
			if err != nil {
				return err
			}
			used += job.Stored + scratch
		}
		if used > int64(capacity.SourceJobBytes.Limit()) {
			break
		}
	}
	if used > int64(capacity.SourceJobBytes.Limit()) {
		return fmt.Errorf("%w; %v", ErrJobCapacity, capacity.SourceJobBytes.Error())
	}
	return nil
}

// scratchBytes measures the regular files under a job's scratch directory.
func scratchBytes(dir string) (int64, error) {
	var used int64
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if path == dir && errors.Is(err, fs.ErrNotExist) {
			return fs.SkipAll
		}
		if err != nil || entry.IsDir() {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return errors.New("invalid source job storage entry")
		}
		used += info.Size()
		return nil
	})
	return used, err
}

// Crash safety rests on one order. A job's objects are stored only while its
// row exists: creation commits the row, in the staging state no gateway
// serves, before storing the original, and every later object is stored by
// the run that holds the job, under its attempt, after the row charged it.
// Retirement deletes the row first and the objects after. Ids are random and
// never reused. So an object listed before the rows are read whose id then has
// no row belongs to a retired job: the sweep removes it, even while a worker
// that lost the job is still writing, and never removes the object of a job
// another gateway is creating. An interrupted creation leaves a staging row,
// which recovery retires once its gateway is gone, and an interrupted
// retirement leaves objects without a row, which the next sweep removes.
//
// A run that lost its job, because its gateway's presence lapsed and another
// gateway resumed the job, may still store the one object it was storing when
// it lost: its next write to the row is refused, and it stops. That object is
// under the lost run's attempt, which the row no longer names for that object,
// so it replaces nothing and is never read; the row charged it before it was
// stored, and the sweep removes it with the job.

// deleteRow retires a job; the janitor removes its objects.
func (m *Jobs) deleteRow(ctx context.Context, q pg.Querier, id string) error {
	_, err := q.Exec(ctx, `DELETE FROM source_jobs WHERE org = $1 AND id = $2`, m.db.Org, id)
	return err
}

// retire deletes a job whose creation failed. Should even that fail, the
// staging row it leaves is retired by recovery.
func (m *Jobs) retire(id string) {
	ctx, cancel := storageContext()
	defer cancel()
	if m.deleteRow(ctx, m.db.Pool, id) == nil {
		m.cleanSoon()
	}
}

// cleanSoon wakes the janitor after a retirement.
func (m *Jobs) cleanSoon() {
	select {
	case m.retired <- struct{}{}:
	default:
	}
}

// sweep removes every object of the jobs that have no row.
func (m *Jobs) sweep(ctx context.Context) error {
	keys, err := m.blobs.List(ctx, jobsDir+"/")
	if err != nil {
		return err
	}
	var ids []string
	for _, key := range keys {
		parts := strings.SplitN(key, "/", 3)
		if len(parts) == 3 && jobID.MatchString(parts[1]) && (len(ids) == 0 || ids[len(ids)-1] != parts[1]) {
			ids = append(ids, parts[1])
		}
	}
	if len(ids) == 0 {
		return nil
	}
	rows, err := m.db.Pool.Query(ctx, `SELECT id FROM source_jobs WHERE org = $1 AND id = ANY($2)`, m.db.Org, ids)
	if err != nil {
		return err
	}
	live, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	kept := map[string]bool{}
	for _, id := range live {
		kept[id] = true
	}
	for _, key := range keys {
		if parts := strings.SplitN(key, "/", 3); len(parts) == 3 && jobID.MatchString(parts[1]) && !kept[parts[1]] {
			if err := m.blobs.Delete(ctx, key); err != nil {
				return err
			}
		}
	}
	return nil
}

// janitor sweeps after each retirement and every hour, apart from the worker,
// so no admission waits on removing a job's objects.
func (m *Jobs) janitor(ctx context.Context) {
	defer close(m.janitorDone)
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.retired:
		case <-time.After(time.Hour):
		}
		sweep, cancel := blobContext(ctx)
		if err := m.sweep(sweep); err != nil && ctx.Err() == nil {
			slog.Warn("source job storage sweep", "error", err)
		}
		cancel()
	}
}
