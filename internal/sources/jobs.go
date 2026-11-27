package sources

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"forgetmenote/internal/blob"
	"forgetmenote/internal/durable"
	"forgetmenote/internal/pg"
	"forgetmenote/internal/workspace"

	"github.com/jackc/pgx/v5"
)

var jobID = regexp.MustCompile(`^[a-f0-9]{32}$`)
var intermediateJobID = regexp.MustCompile(`^\.(staging|retired)-[a-f0-9]{32}$`)
var ErrJobCapacity = errors.New("source processing capacity exceeded")

// rejectedWarning opens the warning of a failed job whose document or URL was
// refused, so that any gateway reports the failure as the caller's. It is not a manifest
// field because manifests decode strictly: the previous release could not open
// rows that had one, which rollback relies on.
const rejectedWarning = "Rejected: "

// JobScope contains identity and restrictions, never bearer tokens or provider
// keys. The application's authorization boundary rechecks it on each operation
// and completed page, including work continuing after its initiating request.
type JobScope struct {
	Subject      string          `json:"subject"`
	Notebook     string          `json:"notebook"`
	Generation   uint64          `json:"generation"`
	Epoch        uint64          `json:"epoch"`
	Restrictions json.RawMessage `json:"restrictions,omitempty"`
}
type JobInput struct {
	Filename string  `json:"filename"`
	Path     string  `json:"path,omitempty"`
	Folder   *string `json:"folder,omitempty"`
	URL      string  `json:"url,omitempty"`
	// Retired is empty for every job this release admits. A manifest an
	// earlier release wrote may name hosted processing there; such a job
	// opens with the rest but never runs again.
	Retired RetiredOptions `json:"options"`
}

// RetiredOptions are the hosted OCR, figure descriptions and transcription a
// source job could ask for before they were removed, and the pages they
// covered. Jobs expire within 24 hours, so a manifest naming them remains only
// briefly after an upgrade.
type RetiredOptions struct {
	DescribeImages bool `json:"describeImages,omitempty"`
	ExternalOCR    bool `json:"externalOCR,omitempty"`
	Transcribe     bool `json:"transcribe,omitempty"`
	FirstPage      int  `json:"firstPage,omitempty"`
	LastPage       int  `json:"lastPage,omitempty"`
}

var errRetiredHosted = errors.New("this job asked for hosted source processing, which was removed; archive the original again, and a connected agent can add its text")

type JobStatus struct {
	ID            string    `json:"id"`
	State         string    `json:"state"`
	Pages         int       `json:"pages"`
	Created       time.Time `json:"created"`
	Updated       time.Time `json:"updated"`
	Expires       time.Time `json:"expires"`
	Warning       string    `json:"warning,omitempty"`
	ResumeBlocked bool      `json:"resumeBlocked,omitempty"`
}
type sourceJob struct {
	Version int `json:"version"`
	JobStatus
	Scope       JobScope `json:"scope"`
	Input       JobInput `json:"input"`
	Hash        string   `json:"hash,omitempty"`
	FinalURL    string   `json:"finalURL,omitempty"`
	ContentType string   `json:"contentType,omitempty"`
	Downloaded  bool     `json:"downloaded,omitempty"`
	TextBytes   int      `json:"textBytes"`
	// Holder is the gateway that queued or runs the job; while it is present, no
	// other gateway interrupts it.
	Holder string `json:"holder,omitempty"`
	// Stored counts the bytes of the objects the job stored or is storing, each
	// charged before it is stored; admission charges them once no reservation
	// covers the job.
	Stored int64 `json:"stored,omitempty"`
	// Attempt numbers the job's runs: each start takes the next. A run changes
	// the row only while it still names this run's gateway and attempt, and
	// stores its objects under names of its own (see objectKey). Fetched is the
	// run that downloaded the original, and Runs each later run that stored
	// page checkpoints, with the first page it stored.
	Attempt int       `json:"attempt,omitempty"`
	Fetched int       `json:"fetched,omitempty"`
	Runs    []pageRun `json:"runs,omitempty"`

	version  int64
	reserved int64
}

type pageRun struct {
	Attempt int `json:"attempt"`
	First   int `json:"first"`
}

// Jobs owns this gateway's durable local source worker. Manifests are rows and
// each job's original, page checkpoints and result are objects in blob
// storage, both shared by the instance's gateways; conversion scratch is this
// gateway's own. No notebook write lock is held during conversion. Admission,
// checkpoints and interruption are independent of HTTP, MCP, chat and
// automation transports.
type Jobs struct {
	mu    sync.Mutex
	db    *pg.DB
	blobs blob.Store
	// scratch holds this gateway's conversions, one directory per job.
	scratch   string
	queue     []string
	active    string
	cancel    context.CancelFunc
	done      chan struct{}
	wake      chan struct{}
	closed    bool
	authorize func(context.Context, JobScope) error

	// The janitor removes retired jobs' objects apart from the worker.
	retired     chan struct{}
	stopJanitor context.CancelFunc
	janitorDone chan struct{}
}

// OpenJobs recovers the instance's jobs for a gateway whose data directory is
// data and whose blob storage is blobs. Its conversions run under
// data/cache/source-jobs, which only this process uses and opening empties.
// Until gateways lease source work, only one may run it: opening interrupts
// the queued and running jobs of this gateway's previous worker and of
// gateways that are gone, and removes the objects of jobs without a row.
func OpenJobs(db *pg.DB, blobs blob.Store, data string, authorize func(context.Context, JobScope) error) (*Jobs, error) {
	if authorize == nil || blobs == nil {
		return nil, errors.New("source jobs require an authorization boundary and blob storage")
	}
	scratch := filepath.Join(data, "cache", jobsDir)
	// Native conversion scratch is never a resumable checkpoint, and no
	// conversion runs here yet, so recovery removes it before releasing any
	// previous execution reservation.
	if err := os.RemoveAll(scratch); err != nil {
		return nil, err
	}
	if err := privateDirectory(scratch); err != nil {
		return nil, err
	}
	if err := removeReleaseLeftovers(filepath.Join(data, jobsDir)); err != nil {
		return nil, err
	}
	m := &Jobs{db: db, blobs: blobs, scratch: scratch, wake: make(chan struct{}, 1), done: make(chan struct{}), authorize: authorize, retired: make(chan struct{}, 1), janitorDone: make(chan struct{})}
	err := m.locked(func(ctx context.Context, tx pgx.Tx) error {
		jobs, err := m.list(ctx, tx)
		if err != nil {
			return err
		}
		for _, job := range jobs {
			if time.Now().After(job.Expires) {
				if err := m.deleteRow(ctx, tx, job.ID); err != nil {
					return err
				}
				continue
			}
			if err := m.interrupt(ctx, tx, job, true); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// The sweep removes the objects of the jobs retired here and of any
	// retirement interrupted earlier. Blob storage that is unreachable now
	// does not stop the gateway; the janitor sweeps again.
	ctx, cancel := blobContext(context.Background())
	if err = m.sweep(ctx); err != nil {
		slog.Warn("source job storage sweep", "error", err)
	}
	cancel()
	ctx, m.stopJanitor = context.WithCancel(context.Background())
	go m.janitor(ctx)
	go m.run()
	return m, nil
}

// removeReleaseLeftovers removes what an interrupted file-based release left in
// its source job directory, as that release did when it opened: admission and
// retirement intermediates, and conversion scratch. Nothing else there is
// touched; with blob storage elsewhere, the jobs' own files there are only the
// import's source.
func removeReleaseLeftovers(dir string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	removed := false
	for _, entry := range entries {
		var leftover string
		switch {
		case !entry.IsDir():
			continue
		case intermediateJobID.MatchString(entry.Name()):
			leftover = filepath.Join(dir, entry.Name())
		case jobID.MatchString(entry.Name()):
			leftover = filepath.Join(dir, entry.Name(), "scratch")
		default:
			continue
		}
		if _, err := os.Lstat(leftover); errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err := os.RemoveAll(leftover); err != nil {
			return err
		}
		removed = true
	}
	if removed {
		return durable.SyncDir(dir)
	}
	return nil
}

// interrupt releases a job whose worker or creation stopped: this gateway's
// previous worker when opening, or another gateway that is gone. A job that
// was never published is retired; its objects go with the next sweep. Outside
// opening, a caller holds m.mu, which Submit holds while it creates a job, so
// this gateway's own unpublished job is then abandoned too.
func (m *Jobs) interrupt(ctx context.Context, tx pgx.Tx, job *sourceJob, opening bool) error {
	if job.Holder == m.db.Holder {
		if !opening && job.State != "staging" {
			return nil
		}
	} else if gone, err := m.db.Gone(ctx, tx, job.Holder); err != nil || !gone {
		return err
	}
	switch job.State {
	case "staging":
		return m.deleteRow(ctx, tx, job.ID)
	case "queued", "running":
		job.State = "interrupted"
		job.Warning = "Processing was interrupted; explicitly resume to continue completed local pages."
	default:
		if job.reserved == 0 {
			return nil
		}
	}
	job.reserved = 0
	return m.save(ctx, tx, job)
}

// recoverGone interrupts the work of gateways that are gone, and retires
// uploads none is still creating. The caller holds m.mu.
func (m *Jobs) recoverGone() error {
	retired := false
	err := m.locked(func(ctx context.Context, tx pgx.Tx) error {
		jobs, err := m.list(ctx, tx)
		if err != nil {
			return err
		}
		for _, job := range jobs {
			if job.State == "staging" || (job.State == "queued" || job.State == "running" || job.reserved != 0) && job.Holder != m.db.Holder {
				retired = retired || job.State == "staging"
				if err = m.interrupt(ctx, tx, job, false); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err == nil && retired {
		m.cleanSoon()
	}
	return err
}

func privateDirectory(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("source job path is not a private directory")
	}
	return os.Chmod(dir, 0700)
}
func strictRecord(raw []byte, out any) error {
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("invalid trailing source job data")
	}
	return nil
}
func marshalJob(job *sourceJob) ([]byte, error) {
	raw, err := json.Marshal(job)
	if err != nil {
		return nil, err
	}
	if len(raw) > 256<<10 {
		return nil, errors.New("source job manifest exceeds 256 KiB")
	}
	return raw, nil
}
func (m *Jobs) signal() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}
func (m *Jobs) Submit(ctx context.Context, scope JobScope, input JobInput, data []byte) (JobStatus, error) {
	if err := m.authorize(ctx, scope); err != nil {
		return JobStatus{}, err
	}
	if scope.Subject == "" || scope.Notebook == "" || scope.Generation == 0 {
		return JobStatus{}, errors.New("invalid source job scope")
	}
	if len(input.Filename) > 1024 || len(data) > workspace.MaxSourceBytes() {
		return JobStatus{}, rejection("source filename or size exceeds its budget")
	}
	if !workspace.SourceURL(input.URL) {
		return JobStatus{}, ValidateURL(input.URL)
	}
	if input.Retired != (RetiredOptions{}) {
		return JobStatus{}, rejection(errRetiredHosted.Error())
	}
	if len(input.Path) > 512 {
		return JobStatus{}, rejection("source path exceeds its budget")
	}
	if len(data) == 0 {
		if err := ValidateURL(input.URL); err != nil {
			return JobStatus{}, err
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return JobStatus{}, errors.New("source processing is stopping")
	}
	if err := m.expire(); err != nil {
		return JobStatus{}, err
	}
	// Reserve the first job for the idle worker, with four further waiting jobs.
	queued, err := m.waiting()
	if err != nil {
		return JobStatus{}, err
	}
	if m.active == "" {
		queued--
	}
	if queued >= 4 {
		return JobStatus{}, fmt.Errorf("%w; at most four jobs may wait", ErrJobCapacity)
	}
	reservation := jobReservation(len(data))
	random := make([]byte, 16)
	rand.Read(random)
	id := hex.EncodeToString(random)
	now := time.Now().UTC()
	job := &sourceJob{Version: 1, JobStatus: JobStatus{ID: id, State: "queued", Created: now, Updated: now, Expires: now.Add(24 * time.Hour)}, Scope: scope, Input: input, Holder: m.db.Holder, Stored: int64(len(data)), reserved: reservation}
	if len(data) > 0 {
		// The row commits before its original is stored, in a state no gateway
		// serves, and is published once the original is.
		job.Hash = workspace.ContentHash(data)
		job.State = "staging"
	}
	raw, err := marshalJob(job)
	if err != nil {
		return JobStatus{}, err
	}
	err = m.locked(func(ctx context.Context, tx pgx.Tx) error {
		if err := m.admitStorage(ctx, tx, "", reservation); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO source_jobs (org, id, version, reserved, manifest) VALUES ($1, $2, 1, $3, $4)`, m.db.Org, id, reservation, raw)
		return err
	})
	if err != nil {
		return JobStatus{}, err
	}
	job.version = 1
	if len(data) > 0 {
		store, cancel := blobContext(ctx)
		err = m.putObject(store, job.originalKey(), data)
		cancel()
		if err == nil {
			job.State = "queued"
			err = m.persist(job)
		}
		if err != nil {
			m.retire(id)
			return JobStatus{}, fmt.Errorf("could not store the source original: %w", err)
		}
	}
	m.queue = append(m.queue, id)
	m.signal()
	return job.JobStatus, nil
}

// waiting counts this gateway's queued jobs.
func (m *Jobs) waiting() (int, error) {
	queued := 0
	for _, id := range m.queue {
		job, err := m.load(id)
		if err != nil {
			return 0, err
		}
		if job != nil && job.State == "queued" {
			queued++
		}
	}
	return queued, nil
}
func (m *Jobs) expire() error {
	ctx, cancel := storageContext()
	defer cancel()
	jobs, err := m.list(ctx, m.db.Pool)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		if time.Now().After(job.Expires) {
			if job.ID == m.active {
				if m.cancel != nil {
					m.cancel()
				}
				continue
			}
			if err := m.deleteRow(ctx, m.db.Pool, job.ID); err != nil {
				return err
			}
			m.cleanSoon()
		}
	}
	return nil
}
func (m *Jobs) authorized(ctx context.Context, scope JobScope, id string) (*sourceJob, error) {
	if !jobID.MatchString(id) {
		return nil, workspace.ErrForbidden
	}
	job, err := m.load(id)
	if err != nil {
		return nil, err
	}
	if job == nil || job.State == "staging" || job.Scope.Subject != scope.Subject || job.Scope.Notebook != scope.Notebook || job.Scope.Generation != scope.Generation || time.Now().After(job.Expires) && id != m.active {
		return nil, workspace.ErrForbidden
	}
	if err := m.authorize(ctx, scope); err != nil {
		return nil, err
	}
	if err := m.authorize(ctx, job.Scope); err != nil {
		return nil, err
	}
	return job, nil
}
func (m *Jobs) Status(ctx context.Context, scope JobScope, id string) (JobStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job, err := m.authorized(ctx, scope, id)
	if err != nil {
		return JobStatus{}, err
	}
	return job.JobStatus, nil
}

// Result reads the job's objects without holding the worker's lock.
func (m *Jobs) Result(ctx context.Context, scope JobScope, id string) (workspace.SourceCapture, error) {
	m.mu.Lock()
	job, err := m.authorized(ctx, scope, id)
	m.mu.Unlock()
	if err != nil {
		return workspace.SourceCapture{}, err
	}
	if job.State != "ready" && job.State != "partial" {
		return workspace.SourceCapture{}, errors.New("source result is not ready; check job progress")
	}
	if time.Now().After(job.Expires) {
		return workspace.SourceCapture{}, workspace.ErrForbidden
	}
	ctx, cancel := blobContext(ctx)
	defer cancel()
	raw, err := m.readObject(ctx, job.resultKey(), 6*workspace.MaxSourceText()+1<<20)
	if err != nil {
		return workspace.SourceCapture{}, err
	}
	var capture workspace.SourceCapture
	if err := strictRecord(raw, &capture); err != nil {
		return capture, err
	}
	original, err := m.readObject(ctx, job.originalKey(), workspace.MaxSourceBytes())
	if err != nil {
		return capture, err
	}
	if workspace.ContentHash(original) != job.Hash || capture.Record.SHA256 != job.Hash || workspace.ContentHash([]byte(capture.Text)) != capture.Record.TextSHA256 || len(capture.Text) > workspace.MaxSourceText() {
		return workspace.SourceCapture{}, errors.New("source result checksum or budget mismatch")
	}
	capture.Original = original
	return capture, nil
}
func (m *Jobs) Cancel(ctx context.Context, scope JobScope, id string) (JobStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job, err := m.authorized(ctx, scope, id)
	if err != nil {
		return JobStatus{}, err
	}
	if job.State != "queued" && job.State != "running" {
		return job.JobStatus, nil
	}
	next := *job
	next.State = "canceled"
	next.Updated = time.Now().UTC()
	next.Warning = "Canceled; completed local pages are retained for explicit resume."
	// A draining local worker keeps its reservation until it stops.
	if m.active != id {
		next.reserved = 0
	}
	if err := m.persist(&next); err != nil {
		return JobStatus{}, err
	}
	if m.active == id && m.cancel != nil {
		m.cancel()
	}
	return next.JobStatus, nil
}
func (m *Jobs) Resume(ctx context.Context, scope JobScope, id string) (JobStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job, err := m.authorized(ctx, scope, id)
	if err != nil {
		return JobStatus{}, err
	}
	if m.closed {
		return JobStatus{}, errors.New("source processing is stopping")
	}
	// ResumeBlocked marks a job an earlier release ran a paid hosted upload for.
	if job.ResumeBlocked || job.Input.Retired != (RetiredOptions{}) {
		return JobStatus{}, errRetiredHosted
	}
	if job.State != "canceled" && job.State != "interrupted" && job.State != "failed" {
		return JobStatus{}, errors.New("only interrupted, canceled or failed local jobs can be resumed")
	}
	if m.active == id {
		return JobStatus{}, errors.New("canceled processing is still draining; resume after it stops")
	}
	queued, err := m.waiting()
	if err != nil {
		return JobStatus{}, err
	}
	if queued >= 4 {
		return JobStatus{}, fmt.Errorf("%w; at most four jobs may wait", ErrJobCapacity)
	}
	if time.Now().After(job.Expires) {
		return JobStatus{}, workspace.ErrForbidden
	}
	// Old scratch cannot coexist with a fresh execution reservation. Preserve
	// the inactive job and its actual-byte accounting if cleanup fails.
	if err := os.RemoveAll(filepath.Join(m.scratch, id)); err != nil {
		return JobStatus{}, fmt.Errorf("could not clear previous source conversion scratch: %w", err)
	}
	reservation := jobReservation(0)
	next := *job
	next.State = "queued"
	next.Holder = m.db.Holder
	next.Warning = ""
	next.Updated = time.Now().UTC()
	next.reserved = reservation
	err = m.locked(func(ctx context.Context, tx pgx.Tx) error {
		if err := m.admitStorage(ctx, tx, id, reservation); err != nil {
			return err
		}
		return m.save(ctx, tx, &next)
	})
	if err != nil {
		return JobStatus{}, err
	}
	m.queue = append(m.queue, id)
	m.signal()
	return next.JobStatus, nil
}
func (m *Jobs) Wait(ctx context.Context, scope JobScope, id string) (workspace.SourceCapture, error) {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		status, err := m.Status(ctx, scope, id)
		if err != nil {
			return workspace.SourceCapture{}, err
		}
		switch status.State {
		case "ready", "partial":
			return m.Result(ctx, scope, id)
		case "failed", "interrupted", "canceled":
			reason, rejected := strings.CutPrefix(status.Warning, rejectedWarning)
			message := fmt.Sprintf("source processing %s: %s", status.State, reason)
			if rejected {
				return workspace.SourceCapture{}, rejection(message)
			}
			return workspace.SourceCapture{}, errors.New(message)
		}
		select {
		case <-ctx.Done():
			return workspace.SourceCapture{}, ctx.Err()
		case <-ticker.C:
		}
	}
}
func (m *Jobs) Close() {
	m.mu.Lock()
	if !m.closed {
		m.closed = true
		if m.cancel != nil {
			m.cancel()
		}
		m.signal()
	}
	m.mu.Unlock()
	<-m.done
	if m.stopJanitor != nil {
		m.stopJanitor()
		<-m.janitorDone
	}
}
func (m *Jobs) run() {
	defer close(m.done)
	for {
		m.mu.Lock()
		if m.closed {
			for _, id := range m.queue {
				if job, err := m.load(id); err == nil && job != nil && job.State == "queued" && job.Holder == m.db.Holder {
					job.State = "interrupted"
					job.Warning = "Processing stopped; explicitly resume completed local work."
					job.Updated = time.Now().UTC()
					job.reserved = 0
					_ = m.persist(job)
				}
			}
			m.mu.Unlock()
			return
		}
		var job *sourceJob
		for len(m.queue) > 0 {
			id := m.queue[0]
			m.queue = m.queue[1:]
			// A job another gateway resumed since it was queued here is that
			// gateway's to run.
			if j, err := m.load(id); err == nil && j != nil && j.State == "queued" && j.Holder == m.db.Holder {
				job = j
				break
			}
		}
		if job == nil {
			m.mu.Unlock()
			select {
			case <-m.wake:
			case <-time.After(time.Minute):
				m.mu.Lock()
				_ = m.expire()
				_ = m.recoverGone()
				m.mu.Unlock()
			}
			continue
		}
		queued := *job
		job.State = "running"
		job.Attempt++
		job.Updated = time.Now().UTC()
		if err := m.persist(job); err != nil {
			if !errors.Is(err, errJobChanged) {
				queued.State = "failed"
				queued.Warning = "Could not persist processing state."
				queued.reserved = 0
				_ = m.persist(&queued)
			}
			m.mu.Unlock()
			continue
		}
		ctx, cancel := context.WithDeadline(context.Background(), job.Expires)
		m.active = job.ID
		m.cancel = cancel
		snapshot := *job
		m.mu.Unlock()
		capture, err := m.process(ctx, snapshot)
		cancel()
		m.mu.Lock()
		m.active = ""
		m.cancel = nil
		// Only the run the row still names finishes the job: one that lost it
		// to another gateway writes nothing more. Cancellation here or on
		// another gateway changed the row; the job then keeps that state and
		// only releases its reservation.
		live, loadErr := m.load(job.ID)
		if loadErr != nil || live == nil || live.Holder != m.db.Holder || live.Attempt != job.Attempt {
			m.mu.Unlock()
			continue
		}
		next := *live
		next.reserved = 0
		if live.State == "running" {
			next.Updated = time.Now().UTC()
			if m.closed || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				next.State = "interrupted"
				next.Warning = "Processing interrupted; explicitly resume completed local pages."
			} else if err != nil {
				next.State = "failed"
				next.Warning = err.Error()
				if errors.Is(err, ErrRejected) {
					next.Warning = rejectedWarning + next.Warning
				}
			} else {
				original := capture.Original
				capture.Original = nil
				raw, storeErr := json.Marshal(capture)
				capture.Original = original
				if storeErr == nil {
					// The row charges the result before it is stored, so one
					// this run stores after losing the job is still charged.
					reserve := *live
					reserve.Stored += int64(len(raw))
					if storeErr = m.persist(&reserve); storeErr == nil {
						next.version, next.Stored = reserve.version, reserve.Stored
						store, cancel := blobContext(context.Background())
						storeErr = m.putObject(store, live.resultKey(), raw)
						cancel()
					}
				}
				if storeErr != nil {
					next.State = "failed"
					next.Warning = "Could not persist extracted result."
				} else {
					next.State = "ready"
					if capture.Record.Extraction == "partial" || capture.Record.Extraction == "failed" || capture.Record.Extraction == "unavailable" {
						next.State = "partial"
					}
					next.Warning = capture.Record.Warning
				}
			}
		}
		if live.State == "running" || live.reserved != 0 {
			failed := next
			if e := m.persist(&next); e != nil && live.State == "running" && !errors.Is(e, errJobChanged) {
				failed.State = "failed"
				failed.Warning = "Could not persist final job state."
				_ = m.persist(&failed)
			}
		}
		m.mu.Unlock()
	}
}

// running reloads the job a run holds. The run may change the row only while
// it still names this gateway and the run's attempt, and only over the version
// read here: a job canceled, interrupted, retired or resumed by another
// gateway meanwhile stops its work.
func (m *Jobs) running(job *sourceJob) (*sourceJob, error) {
	live, err := m.load(job.ID)
	if err != nil {
		return nil, err
	}
	if live == nil || live.State != "running" || live.Holder != m.db.Holder || live.Attempt != job.Attempt {
		return nil, context.Canceled
	}
	return live, nil
}
func (m *Jobs) process(ctx context.Context, job sourceJob) (capture workspace.SourceCapture, err error) {
	if err := m.authorize(ctx, job.Scope); err != nil {
		return workspace.SourceCapture{}, err
	}
	scratch := filepath.Join(m.scratch, job.ID)
	if err := privateDirectory(scratch); err != nil {
		return workspace.SourceCapture{}, err
	}
	ctx = context.WithValue(ctx, jobScratchKey{}, scratch)
	defer func() {
		err = errors.Join(err, os.RemoveAll(scratch))
	}()
	if job.Hash == "" {
		raw, contentType, final, err := Fetch(ctx, job.Input.URL)
		if err != nil {
			return workspace.SourceCapture{}, err
		}
		if err := m.authorize(ctx, job.Scope); err != nil {
			return workspace.SourceCapture{}, err
		}
		// The row charges the original before it is stored, and names it once
		// it is, over the version that charged it.
		m.mu.Lock()
		live, err := m.running(&job)
		if err == nil {
			live.Stored += int64(len(raw))
			err = m.persist(live)
		}
		m.mu.Unlock()
		if err != nil {
			return workspace.SourceCapture{}, err
		}
		live.Hash = workspace.ContentHash(raw)
		live.FinalURL = final
		live.ContentType = contentType
		live.Downloaded = true
		live.Fetched = job.Attempt
		store, cancel := blobContext(ctx)
		err = m.putObject(store, live.originalKey(), raw)
		cancel()
		if err == nil {
			m.mu.Lock()
			err = m.persist(live)
			m.mu.Unlock()
		}
		if err != nil {
			return workspace.SourceCapture{}, err
		}
		job = *live
	}
	raw, err := m.readObject(ctx, job.originalKey(), workspace.MaxSourceBytes())
	if err != nil {
		return workspace.SourceCapture{}, err
	}
	if workspace.ContentHash(raw) != job.Hash {
		return workspace.SourceCapture{}, errors.New("source original checksum mismatch")
	}
	if job.Input.Retired != (RetiredOptions{}) {
		return workspace.SourceCapture{}, errRetiredHosted
	}
	var options Options
	options.pageStart = func(number int) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return m.authorize(ctx, job.Scope)
	}
	for n := 1; n <= job.Pages; n++ {
		raw, err := m.readObject(ctx, job.pageKey(n), 6*workspace.MaxSourceText()+1024)
		if err != nil {
			return workspace.SourceCapture{}, err
		}
		var page PDFPage
		if strictRecord(raw, &page) != nil || page.Number != n {
			return workspace.SourceCapture{}, errors.New("invalid source page checkpoint")
		}
		options.completedPages = append(options.completedPages, page)
	}
	options.pageDone = func(page PDFPage) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := m.authorize(ctx, job.Scope); err != nil {
			return err
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		live, err := m.running(&job)
		if err != nil {
			return err
		}
		if page.Number != live.Pages+1 {
			return errors.New("source page checkpoints out of order")
		}
		if len(page.Text) > workspace.MaxSourceText()-live.TextBytes {
			return errors.New("page text exceeds FMN_LIMIT_SOURCE_TEXT_BYTES")
		}
		raw, err := json.Marshal(page)
		if err != nil {
			return err
		}
		// The row charges the checkpoint before it is stored, and names it once
		// it is, over the version that charged it.
		live.Stored += int64(len(raw))
		if err := m.persist(live); err != nil {
			return err
		}
		store, cancel := blobContext(ctx)
		defer cancel()
		if err := m.putObject(store, objectKey(job.ID, job.Attempt, pageFile(page.Number)), raw); err != nil {
			return err
		}
		if job.Attempt > 1 && (len(live.Runs) == 0 || live.Runs[len(live.Runs)-1].Attempt != job.Attempt) {
			live.Runs = append(live.Runs, pageRun{Attempt: job.Attempt, First: page.Number})
		}
		live.Pages = page.Number
		live.TextBytes += len(page.Text)
		live.Updated = time.Now().UTC()
		return m.persist(live)
	}
	filename := job.Input.Filename
	if filename == "" && job.Downloaded {
		if u, err := url.Parse(job.FinalURL); err == nil {
			filename = u.Path
		}
	}
	if job.Downloaded {
		if format := workspace.SourceFormatForMIME(strings.TrimSpace(strings.Split(job.ContentType, ";")[0])); format.MIME != "" {
			filename = "download" + format.Extensions[0]
		}
	}
	capture, err = PrepareWithOptions(ctx, filename, job.Input.URL, raw, options)
	if err != nil {
		return capture, err
	}
	if err := m.authorize(ctx, job.Scope); err != nil {
		return workspace.SourceCapture{}, err
	}

	if job.Downloaded {
		capture.Record.Method = "server-http"
		capture.Record.FinalURL = job.FinalURL
	}
	return capture, nil
}
