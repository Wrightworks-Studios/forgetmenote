package judgments

import (
	"context"
	"encoding/json"
	"errors"
	"forgetmenote/internal/auth"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/decision"
	"forgetmenote/internal/knowledge"
	"forgetmenote/internal/webcore"
	"forgetmenote/internal/workspace"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

var errJudgmentAuditCapacity = errors.New("audit metadata exceeds durable storage capacity")

// errJudgmentJobIdle keeps a job that is not waiting to run as it is stored.
var errJudgmentJobIdle = errors.New("job is not waiting to run")

type judgmentSourceSelection struct {
	Path     string `json:"path"`
	Revision string `json:"revision"`
}

// JobInput is the request for a review or organization job.
type JobInput struct {
	Organization *organizationInput        `json:"organization,omitempty"`
	Kind         string                    `json:"kind"`
	OperationID  string                    `json:"operationId"`
	Sources      []judgmentSourceSelection `json:"sources,omitempty"`
	Destinations []string                  `json:"destinations,omitempty"`
	Tags         []string                  `json:"tags,omitempty"`
}
type judgmentJobNote struct {
	Path        string `json:"path"`
	Revision    string `json:"revision"`
	Hash        string `json:"hash"`
	CurrentHash string `json:"currentHash"`
	Source      bool   `json:"source"`
	Destination bool   `json:"destination"`
	Historical  bool   `json:"historical"`
	Container   string `json:"container"`
}
type judgmentJobWork struct {
	Organization *organizationAction     `json:"organization,omitempty"`
	Kind         string                  `json:"kind"`
	Passages     []string                `json:"passages"`
	Tags         []string                `json:"tags,omitempty"`
	Attempt      int                     `json:"attempt"`
	Verdict      string                  `json:"verdict,omitempty"`
	Scores       map[string]float64      `json:"scores,omitempty"`
	Proposal     *workspace.EditGroup    `json:"proposal,omitempty"`
	Finding      *workspace.FindingInput `json:"finding,omitempty"`
}
type judgmentJobResult struct {
	SourceID    string                       `json:"sourceId,omitempty"`
	Complete    bool                         `json:"complete"`
	Compared    int                          `json:"compared,omitempty"`
	ID          string                       `json:"id"`
	Kind        string                       `json:"kind"`
	Title       string                       `json:"title"`
	Summary     string                       `json:"summary"`
	Status      string                       `json:"status"`
	FindingID   string                       `json:"findingId,omitempty"`
	FindingPath string                       `json:"findingPath,omitempty"`
	ProposalID  string                       `json:"proposalId,omitempty"`
	Evidence    []knowledge.JudgmentEvidence `json:"evidence"`
	Details     any                          `json:"details,omitempty"`
}
type judgmentJob struct {
	Organization *organizationRun    `json:"organization,omitempty"`
	Hidden       bool                `json:"-"`
	Version      int                 `json:"version"`
	ID           string              `json:"id"`
	RequestHash  string              `json:"requestHash"`
	Project      string              `json:"project"`
	Owner        string              `json:"owner"`
	AccountID    string              `json:"accountId"`
	Kind         string              `json:"kind"`
	State        string              `json:"state"`
	Examined     int                 `json:"examined"`
	Total        int                 `json:"total"`
	Remaining    int                 `json:"remaining"`
	UpdatedAt    string              `json:"updatedAt"`
	CreatedAt    string              `json:"createdAt"`
	Message      string              `json:"message"`
	Results      []judgmentJobResult `json:"-"`
	Automatic    bool                `json:"automatic"`
	Cancelled    bool                `json:"cancelled"`
	Inventory    string              `json:"inventory"`
	Notes        []judgmentJobNote   `json:"notes"`
	Work         []judgmentJobWork   `json:"work"`
	Cursor       int                 `json:"cursor"`
	Model        string              `json:"model"`
}

func jobName(id string) string { return "jobs/" + id + ".json" }

func judgmentJobID(id string) bool { return len(id) == 64 && !strings.ContainsAny(id, "/\\.") }

func validJudgmentJob(id string, job judgmentJob) error {
	if job.Version != 2 || job.ID != id || job.Owner == "" || job.AccountID == "" || job.Project == "" || job.RequestHash == "" || job.Inventory == "" || job.Model == "" || job.Notes == nil || job.Work == nil || job.Total != len(job.Work) || job.Cursor < 0 || job.Cursor > job.Total || job.Examined != job.Cursor {
		return decision.ErrStorage
	}
	switch job.State {
	case "queued", "running", "paused", "completed", "cancelled", "failed":
	default:
		return decision.ErrStorage
	}
	return nil
}

func (j *judgmentState) readJob(id string) (judgmentJob, error) {
	var job judgmentJob
	if !judgmentJobID(id) {
		return job, chat.ErrNotFound
	}
	if err := j.read(jobName(id), &job); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return job, chat.ErrNotFound
		}
		return job, err
	}
	return job, validJudgmentJob(id, job)
}

// updateJob changes a stored job while holding its record, so a cancellation
// or checkpoint committed through another gateway is never overwritten, and
// returns the job as stored.
func (j *judgmentState) updateJob(id string, change func(*judgmentJob) error) (judgmentJob, error) {
	var job judgmentJob
	if !judgmentJobID(id) {
		return job, chat.ErrNotFound
	}
	err := j.update(jobName(id), &job, func(found bool) error {
		if !found {
			return chat.ErrNotFound
		}
		if err := validJudgmentJob(id, job); err != nil {
			return err
		}
		return change(&job)
	})
	return job, err
}

// insertJob stores a new job unless its operation already has one, which it
// returns instead, refusing a different request under the same operation.
// admit runs only for a new job, while its record is held.
func (j *judgmentState) insertJob(job judgmentJob, admit func() error) (judgmentJob, error) {
	var stored judgmentJob
	err := j.update(jobName(job.ID), &stored, func(found bool) error {
		if !found {
			if err := admit(); err != nil {
				return err
			}
			stored = job
			return nil
		}
		if err := validJudgmentJob(job.ID, stored); err != nil {
			return err
		}
		if stored.RequestHash != job.RequestHash {
			return workspace.ErrConflict
		}
		return nil
	})
	return stored, err
}

func (j *judgmentState) jobIDs() ([]string, error) {
	names, err := j.records.List("jobs/", judgmentJobLimit)
	if err != nil {
		return nil, err
	}
	if len(names) > judgmentJobLimit {
		return nil, decision.ErrLimit
	}
	ids := []string{}
	for _, name := range names {
		if id, ok := strings.CutSuffix(strings.TrimPrefix(name, "jobs/"), ".json"); ok && !strings.Contains(id, "/") {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// saveJudgmentJob checkpoints the runner's copy of its job unless the job was
// cancelled, through any gateway, since the runner read it.
func (s *Service) saveJudgmentJob(job *judgmentJob) error {
	s.judgmentState.mu.Lock()
	defer s.judgmentState.mu.Unlock()
	_, err := s.judgmentState.updateJob(job.ID, func(current *judgmentJob) error {
		if current.Cancelled {
			return context.Canceled
		}
		job.Total = len(job.Work)
		job.Examined = job.Cursor
		job.Remaining = job.Total - job.Cursor
		job.UpdatedAt = chat.Now()
		*current = *job
		return nil
	})
	return err
}

// settleJudgmentJob records where a runner stopped, in the state and message the
// runner set. A cancellation through any gateway has settled the job already.
// When the save fails the job is paused with a message instead, so a run
// never stays running; a job that cannot be saved at all is logged.
func (s *Service) settleJudgmentJob(job *judgmentJob) {
	err := s.saveJudgmentJob(job)
	if err == nil || errors.Is(err, context.Canceled) {
		return
	}
	slog.Warn("judgment job state not saved", "job", job.ID, "state", job.State, "error", err)
	if job.State == "paused" {
		return
	}
	job.State = "paused"
	job.Message = "The run's outcome could not be saved. Continue resumes it from its last checkpoint."
	if err = s.saveJudgmentJob(job); err != nil && !errors.Is(err, context.Canceled) {
		slog.Warn("judgment job state not saved", "job", job.ID, "state", job.State, "error", err)
	}
}

// checkpointJudgmentJob saves a runner's progress and reports whether the run may
// go on. When it may not, the job is settled: cancelled through any gateway,
// or paused because the checkpoint failed.
func (s *Service) checkpointJudgmentJob(job *judgmentJob) bool {
	err := s.saveJudgmentJob(job)
	if err == nil {
		return true
	}
	if !errors.Is(err, context.Canceled) {
		slog.Warn("judgment job checkpoint not saved", "job", job.ID, "error", err)
		job.State = "paused"
		job.Message = "A checkpoint could not be saved. Continue resumes the run from its last saved checkpoint."
		s.settleJudgmentJob(job)
	}
	return false
}

func (s *Service) judgmentJobRoutes(project func(pattern, role string, handler http.HandlerFunc), global func(pattern string, handler func(http.ResponseWriter, *http.Request))) {
	project("POST /api/judgments/jobs", "comment", s.judgmentJobCreate)
	project("GET /api/judgments/jobs", "read", s.judgmentJobList)
	project("GET /api/judgments/jobs/{job}", "read", s.judgmentJobGet)
	project("POST /api/judgments/jobs/{job}/continue", "comment", s.judgmentJobControl)
	project("POST /api/judgments/jobs/{job}/cancel", "comment", s.judgmentJobControl)
	global("GET /api/notifications", s.judgmentNotifications)
	global("POST /api/notifications/read", s.judgmentNotificationsRead)
}

func (s *Service) judgmentJobCreate(w http.ResponseWriter, r *http.Request) {
	user, err := s.hooks.Identity(r)
	if err != nil {
		webcore.Fail(w, err)
		return
	}
	var input JobInput
	if !webcore.Decode(w, r, &input) {
		return
	}
	if input.Kind == "organize" {
		s.judgmentState.organizationMu.Lock()
		defer s.judgmentState.organizationMu.Unlock()
	}
	job, err := s.createJudgmentJob(r.Context(), user, webcore.NotebookID(r.Context()), input, false, nil, nil)
	if errors.Is(err, errJudgmentAuditCapacity) {
		http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
		return
	}
	if err != nil {
		webcore.Fail(w, err)
		return
	}
	s.launchJudgmentJob(job.ID, false)
	webcore.JSON(w, publicJudgmentJob(job))
}

// createJudgmentJob prepares a review. changed, when set, limits it to those notes,
// and known names the text hashes of passages already reviewed there.
func (s *Service) createJudgmentJob(ctx context.Context, user, project string, input JobInput, automatic bool, changed, known map[string]bool) (judgmentJob, error) {
	var empty judgmentJob
	// Reviews are paid background work, as the instance.
	if err := s.outgoing.Check(); err != nil {
		return empty, err
	}
	if input.Kind == "organize" {
		return s.createOrganizationJob(ctx, user, project, input, automatic, changed)
	}
	if input.Organization != nil {
		return empty, decision.ErrInvalid
	}
	if input.Kind != "consistency" && input.Kind != "audit" && input.Kind != "organization" || strings.TrimSpace(input.OperationID) == "" || len(input.OperationID) > 128 || len(input.Sources) > 100 || len(input.Destinations) > 100 || len(input.Tags) > 100 {
		return empty, decision.ErrInvalid
	}
	judge, _ := s.hooks.Judge()
	if err := s.hooks.Authorize(ctx, user, project, input.Kind, judge); err != nil {
		return empty, err
	}
	store, err := s.catalog.Access(user, project, "comment")
	if err != nil {
		return empty, err
	}
	account, err := s.catalog.Account(user)
	if err != nil {
		return empty, err
	}
	raw, _ := json.Marshal(input)
	requestHash := auth.Digest(string(raw))
	id := auth.Digest(account.ID + "\x00" + project + "\x00" + input.OperationID)
	s.judgmentState.mu.Lock()
	prior, err := s.judgmentState.readJob(id)
	s.judgmentState.mu.Unlock()
	if err == nil {
		if prior.RequestHash != requestHash {
			return empty, workspace.ErrConflict
		}
		return prior, nil
	}
	if !errors.Is(err, chat.ErrNotFound) {
		return empty, err
	}
	select {
	case s.judgmentState.prepare <- struct{}{}:
	case <-ctx.Done():
		return empty, ctx.Err()
	}
	defer func() { <-s.judgmentState.prepare }()
	job, err := buildJudgmentJob(ctx, store, input, changed, known)
	if err != nil {
		return empty, err
	}
	job.Version = 2
	job.ID = id
	job.RequestHash = requestHash
	job.Project = project
	job.Owner = user
	job.AccountID = account.ID
	job.Kind = input.Kind
	job.State = "queued"
	job.Automatic = automatic
	job.Model = judge.Model()
	job.CreatedAt = chat.Now()
	job.UpdatedAt = job.CreatedAt
	job.Results = []judgmentJobResult{}
	job.Total = len(job.Work)
	job.Remaining = job.Total
	job.Message = "Checking selected relevant candidates and original passages; this is not exhaustive all-pairs verification."
	if err = s.checkJudgmentJob(ctx, job, judge); err != nil {
		return empty, err
	}
	if err = s.roomForJudgmentJob(); err != nil {
		return empty, err
	}
	s.judgmentState.mu.Lock()
	defer s.judgmentState.mu.Unlock()
	if s.judgmentState.closed {
		return empty, decision.ErrDisabled
	}
	job, err = s.judgmentState.insertJob(job, func() error {
		ids, err := s.judgmentState.jobIDs()
		if err != nil {
			return err
		}
		if len(ids) >= judgmentJobLimit {
			return decision.ErrLimit
		}
		return nil
	})
	if err != nil {
		return empty, err
	}
	return job, nil
}

func publicJudgmentJob(job judgmentJob) map[string]any {
	return map[string]any{"organization": job.Organization, "automatic": job.Automatic, "owner": job.Owner, "id": job.ID, "project": job.Project, "kind": job.Kind, "state": job.State, "examined": job.Examined, "total": job.Total, "remaining": job.Remaining, "updatedAt": job.UpdatedAt, "message": job.Message, "results": []judgmentJobResult{}}
}

// visibleJudgmentJob shows a job as stored to user; active reports that a runner
// on this gateway holds it.
func (s *Service) visibleJudgmentJob(ctx context.Context, user string, job judgmentJob, active bool) (judgmentJob, error) {
	if !s.catalog.Enabled(user) {
		return job, workspace.ErrForbidden
	}
	store, err := s.catalog.Access(user, job.Project, "read")
	if err != nil {
		return job, err
	}
	if job.Organization != nil {
		if !active {
			job, err = s.reconcileOrganizationReceipt(ctx, store, job.ID)
			if err != nil {
				return job, err
			}
		}
		paths := []string{}
		for _, n := range job.Notes {
			paths = append(paths, n.Path)
		}
		for original, current := range job.Organization.Paths {
			paths = append(paths, original)
			if current != "" {
				paths = append(paths, current)
			}
		}
		allowed, err := store.VisibleHistoryPaths(ctx, paths)
		if err != nil {
			return job, err
		}
		for _, p := range paths {
			if !allowed[p] {
				job.Hidden = true
				job.Organization = nil
				job.Message = "Organization evidence is hidden because notebook visibility changed."
				break
			}
		}
		if !active && job.State == "running" {
			job.State = "paused"
			job.Message = "Organization interrupted; continue its durable checkpoint."
		}
		return job, nil
	}
	if err = validateJudgmentSources(ctx, store, job); err != nil {
		job.Hidden = true
		job.Results = []judgmentJobResult{}
		job.State = "paused"
		job.Message = "Sources or visibility changed. Start a new review for current evidence."
	}
	if !active && job.State == "running" {
		job.State = "paused"
		job.Message = "Review interrupted; continue its durable checkpoint."
	}
	return job, nil
}

func (s *Service) judgmentJobGet(w http.ResponseWriter, r *http.Request) {
	user, err := s.hooks.Identity(r)
	if err != nil {
		webcore.Fail(w, err)
		return
	}
	job, active, err := s.judgmentState.readJobAndRunner(r.PathValue("job"))
	if err != nil || job.Project != webcore.NotebookID(r.Context()) {
		webcore.Fail(w, chat.ErrNotFound)
		return
	}
	job, err = s.visibleJudgmentJob(r.Context(), user, job, active)
	if err != nil {
		webcore.Fail(w, err)
		return
	}
	offset, limit, err := judgmentPage(r)
	if err != nil {
		webcore.Fail(w, err)
		return
	}
	result, err := s.judgmentJobPage(job, offset, limit)
	if err != nil {
		webcore.Fail(w, err)
		return
	}
	webcore.JSON(w, result)
}

func (s *Service) judgmentJobList(w http.ResponseWriter, r *http.Request) {
	user, err := s.hooks.Identity(r)
	if err != nil {
		webcore.Fail(w, err)
		return
	}
	jobs, active, err := s.judgmentState.jobsAndRunners(map[string]any{"project": webcore.NotebookID(r.Context())})
	if err != nil {
		webcore.Fail(w, err)
		return
	}
	result := []map[string]any{}
	for _, job := range jobs {
		job, err = s.visibleJudgmentJob(r.Context(), user, job, active[job.ID])
		if err != nil {
			webcore.Fail(w, err)
			return
		}
		result = append(result, publicJudgmentJob(job))
	}
	sort.Slice(result, func(i, j int) bool { return result[i]["updatedAt"].(string) > result[j]["updatedAt"].(string) })
	webcore.JSON(w, map[string]any{"jobs": result})
}

func (s *Service) judgmentJobControl(w http.ResponseWriter, r *http.Request) {
	s.judgmentState.organizationMu.Lock()
	defer s.judgmentState.organizationMu.Unlock()
	user, err := s.hooks.Identity(r)
	if err != nil {
		webcore.Fail(w, err)
		return
	}
	id := r.PathValue("job")
	s.judgmentState.mu.Lock()
	job, err := s.judgmentState.readJob(id)
	if err != nil || job.Project != webcore.NotebookID(r.Context()) {
		s.judgmentState.mu.Unlock()
		webcore.Fail(w, chat.ErrNotFound)
		return
	}
	if job.Owner != user {
		if _, err = s.catalog.Access(user, job.Project, "owner"); err != nil {
			s.judgmentState.mu.Unlock()
			webcore.Fail(w, workspace.ErrForbidden)
			return
		}
	}
	cancel := strings.HasSuffix(r.URL.Path, "/cancel")
	if cancel {
		job, err = s.judgmentState.updateJob(id, func(job *judgmentJob) error {
			job.Cancelled = true
			job.State = "cancelled"
			job.UpdatedAt = chat.Now()
			return nil
		})
		if stop := s.judgmentState.active["job:"+id]; stop != nil {
			stop()
		}
	}
	s.judgmentState.mu.Unlock()
	if err != nil {
		webcore.Fail(w, err)
		return
	}
	if !cancel {
		if job.Cancelled {
			webcore.Fail(w, workspace.ErrConflict)
			return
		}
		if err = s.outgoing.Check(); err != nil {
			webcore.Fail(w, err)
			return
		}
		judge, _ := s.hooks.Judge()
		if err = s.checkJudgmentJob(r.Context(), job, judge); err != nil {
			webcore.Fail(w, err)
			return
		}
		s.launchJudgmentJob(id, true)
	}
	webcore.JSON(w, publicJudgmentJob(job))
}

// One durable review advances at a time. Provider concurrency alone would still
// allow every queued review to load its full pinned source set into memory.
func (s *Service) launchJudgmentJob(id string, explicit bool) {
	s.judgmentState.mu.Lock()
	defer s.judgmentState.mu.Unlock()
	if s.judgmentState.closed || s.judgmentState.active["job:"+id] != nil || s.outgoing.Paused() {
		return
	}
	busy := false
	for key := range s.judgmentState.active {
		busy = busy || strings.HasPrefix(key, "job:")
	}
	job, err := s.judgmentState.updateJob(id, func(job *judgmentJob) error {
		if job.Cancelled || job.State == "completed" || job.State == "failed" || !explicit && job.State != "queued" {
			return errJudgmentJobIdle
		}
		if explicit && job.State != "queued" && job.Cursor < len(job.Work) && job.Work[job.Cursor].Verdict == "" {
			job.Work[job.Cursor].Attempt++
			if a := job.Work[job.Cursor].Organization; a != nil && a.Answers == nil {
				a.Attempted = false
			}
		}
		job.State = "running"
		if busy {
			job.State = "queued"
		}
		job.UpdatedAt = chat.Now()
		return nil
	})
	if err != nil || busy {
		return
	}
	ctx, cancel := context.WithTimeout(s.judgmentState.ctx, 2*time.Minute)
	key := "job:" + id
	s.judgmentState.active[key] = cancel
	s.judgmentState.wg.Add(1)
	go func() {
		defer s.judgmentState.wg.Done()
		defer cancel()
		defer func() {
			s.judgmentState.mu.Lock()
			delete(s.judgmentState.active, key)
			s.judgmentState.mu.Unlock()
			s.launchNextJudgmentJob()
		}()
		s.runJudgmentJob(ctx, job)
	}()
}

// launchNextJudgmentJob starts the longest-waiting queued job unless one runs on
// this gateway. It reads only queued jobs, so finished ones never hide them.
func (s *Service) launchNextJudgmentJob() {
	s.judgmentState.mu.Lock()
	if s.judgmentState.closed {
		s.judgmentState.mu.Unlock()
		return
	}
	for key := range s.judgmentState.active {
		if strings.HasPrefix(key, "job:") {
			s.judgmentState.mu.Unlock()
			return
		}
	}
	s.judgmentState.mu.Unlock()
	queued, invalid, more, err := s.judgmentState.scanJobs(map[string]any{"state": "queued", "cancelled": false})
	if err != nil || invalid > 0 || more {
		slog.Warn("judgment queue not fully read", "invalid", invalid, "more", more, "error", err)
	}
	next := judgmentJob{}
	for _, job := range queued {
		if next.ID == "" || judgmentJobTime(job).Before(judgmentJobTime(next)) {
			next = job
		}
	}
	if next.ID != "" {
		s.launchJudgmentJob(next.ID, false)
	}
}
