package jev

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

var errJevAuditCapacity = errors.New("audit metadata exceeds durable storage capacity")

// errJevJobIdle keeps a job that is not waiting to run as it is stored.
var errJevJobIdle = errors.New("job is not waiting to run")

type jevSourceSelection struct {
	Path     string `json:"path"`
	Revision string `json:"revision"`
}

// JobInput is the request for a review or organization job.
type JobInput struct {
	Organization *organizationInput   `json:"organization,omitempty"`
	Kind         string               `json:"kind"`
	OperationID  string               `json:"operationId"`
	Sources      []jevSourceSelection `json:"sources,omitempty"`
	Destinations []string             `json:"destinations,omitempty"`
	Tags         []string             `json:"tags,omitempty"`
}
type jevJobNote struct {
	Path        string `json:"path"`
	Revision    string `json:"revision"`
	Hash        string `json:"hash"`
	CurrentHash string `json:"currentHash"`
	Source      bool   `json:"source"`
	Destination bool   `json:"destination"`
	Historical  bool   `json:"historical"`
	Container   string `json:"container"`
}
type jevJobWork struct {
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
type jevJobResult struct {
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
type jevJob struct {
	Organization *organizationRun `json:"organization,omitempty"`
	Hidden       bool             `json:"-"`
	Version      int              `json:"version"`
	ID           string           `json:"id"`
	RequestHash  string           `json:"requestHash"`
	Project      string           `json:"project"`
	Owner        string           `json:"owner"`
	AccountID    string           `json:"accountId"`
	Kind         string           `json:"kind"`
	State        string           `json:"state"`
	Examined     int              `json:"examined"`
	Total        int              `json:"total"`
	Remaining    int              `json:"remaining"`
	UpdatedAt    string           `json:"updatedAt"`
	CreatedAt    string           `json:"createdAt"`
	Message      string           `json:"message"`
	Results      []jevJobResult   `json:"-"`
	Automatic    bool             `json:"automatic"`
	Cancelled    bool             `json:"cancelled"`
	Inventory    string           `json:"inventory"`
	Notes        []jevJobNote     `json:"notes"`
	Work         []jevJobWork     `json:"work"`
	Cursor       int              `json:"cursor"`
	Model        string           `json:"model"`
}

func jobName(id string) string { return "jobs/" + id + ".json" }

func jevJobID(id string) bool { return len(id) == 64 && !strings.ContainsAny(id, "/\\.") }

func validJevJob(id string, job jevJob) error {
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

func (j *jevState) readJob(id string) (jevJob, error) {
	var job jevJob
	if !jevJobID(id) {
		return job, chat.ErrNotFound
	}
	if err := j.read(jobName(id), &job); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return job, chat.ErrNotFound
		}
		return job, err
	}
	return job, validJevJob(id, job)
}

// updateJob changes a stored job while holding its record, so a cancellation
// or checkpoint committed through another gateway is never overwritten, and
// returns the job as stored.
func (j *jevState) updateJob(id string, change func(*jevJob) error) (jevJob, error) {
	var job jevJob
	if !jevJobID(id) {
		return job, chat.ErrNotFound
	}
	err := j.update(jobName(id), &job, func(found bool) error {
		if !found {
			return chat.ErrNotFound
		}
		if err := validJevJob(id, job); err != nil {
			return err
		}
		return change(&job)
	})
	return job, err
}

// insertJob stores a new job unless its operation already has one, which it
// returns instead, refusing a different request under the same operation.
// admit runs only for a new job, while its record is held.
func (j *jevState) insertJob(job jevJob, admit func() error) (jevJob, error) {
	var stored jevJob
	err := j.update(jobName(job.ID), &stored, func(found bool) error {
		if !found {
			if err := admit(); err != nil {
				return err
			}
			stored = job
			return nil
		}
		if err := validJevJob(job.ID, stored); err != nil {
			return err
		}
		if stored.RequestHash != job.RequestHash {
			return workspace.ErrConflict
		}
		return nil
	})
	return stored, err
}

func (j *jevState) jobIDs() ([]string, error) {
	names, err := j.records.List("jobs/", jevJobLimit)
	if err != nil {
		return nil, err
	}
	if len(names) > jevJobLimit {
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

// saveJevJob checkpoints the runner's copy of its job unless the job was
// cancelled, through any gateway, since the runner read it.
func (s *Service) saveJevJob(job *jevJob) error {
	s.jevState.mu.Lock()
	defer s.jevState.mu.Unlock()
	_, err := s.jevState.updateJob(job.ID, func(current *jevJob) error {
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

// settleJevJob records where a runner stopped, in the state and message the
// runner set. A cancellation through any gateway has settled the job already.
// When the save fails the job is paused with a message instead, so a run
// never stays running; a job that cannot be saved at all is logged.
func (s *Service) settleJevJob(job *jevJob) {
	err := s.saveJevJob(job)
	if err == nil || errors.Is(err, context.Canceled) {
		return
	}
	slog.Warn("jev job state not saved", "job", job.ID, "state", job.State, "error", err)
	if job.State == "paused" {
		return
	}
	job.State = "paused"
	job.Message = "The run's outcome could not be saved. Continue resumes it from its last checkpoint."
	if err = s.saveJevJob(job); err != nil && !errors.Is(err, context.Canceled) {
		slog.Warn("jev job state not saved", "job", job.ID, "state", job.State, "error", err)
	}
}

// checkpointJevJob saves a runner's progress and reports whether the run may
// go on. When it may not, the job is settled: cancelled through any gateway,
// or paused because the checkpoint failed.
func (s *Service) checkpointJevJob(job *jevJob) bool {
	err := s.saveJevJob(job)
	if err == nil {
		return true
	}
	if !errors.Is(err, context.Canceled) {
		slog.Warn("jev job checkpoint not saved", "job", job.ID, "error", err)
		job.State = "paused"
		job.Message = "A checkpoint could not be saved. Continue resumes the run from its last saved checkpoint."
		s.settleJevJob(job)
	}
	return false
}

func (s *Service) jevJobRoutes(project func(pattern, role string, handler http.HandlerFunc), global func(pattern string, handler func(http.ResponseWriter, *http.Request))) {
	project("POST /api/jev/jobs", "comment", s.jevJobCreate)
	project("GET /api/jev/jobs", "read", s.jevJobList)
	project("GET /api/jev/jobs/{job}", "read", s.jevJobGet)
	project("POST /api/jev/jobs/{job}/continue", "comment", s.jevJobControl)
	project("POST /api/jev/jobs/{job}/cancel", "comment", s.jevJobControl)
	global("GET /api/notifications", s.jevNotifications)
	global("POST /api/notifications/read", s.jevNotificationsRead)
}

func (s *Service) jevJobCreate(w http.ResponseWriter, r *http.Request) {
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
		s.jevState.organizationMu.Lock()
		defer s.jevState.organizationMu.Unlock()
	}
	job, err := s.createJevJob(r.Context(), user, webcore.NotebookID(r.Context()), input, false, nil, nil)
	if errors.Is(err, errJevAuditCapacity) {
		http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
		return
	}
	if err != nil {
		webcore.Fail(w, err)
		return
	}
	s.launchJevJob(job.ID, false)
	webcore.JSON(w, publicJevJob(job))
}

// createJevJob prepares a review. changed, when set, limits it to those notes,
// and known names the text hashes of passages already reviewed there.
func (s *Service) createJevJob(ctx context.Context, user, project string, input JobInput, automatic bool, changed, known map[string]bool) (jevJob, error) {
	var empty jevJob
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
	s.jevState.mu.Lock()
	prior, err := s.jevState.readJob(id)
	s.jevState.mu.Unlock()
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
	case s.jevState.prepare <- struct{}{}:
	case <-ctx.Done():
		return empty, ctx.Err()
	}
	defer func() { <-s.jevState.prepare }()
	job, err := buildJevJob(ctx, store, input, changed, known)
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
	job.Results = []jevJobResult{}
	job.Total = len(job.Work)
	job.Remaining = job.Total
	job.Message = "Checking selected relevant candidates and original passages; this is not exhaustive all-pairs verification."
	if err = s.checkJevJob(ctx, job, judge); err != nil {
		return empty, err
	}
	if err = s.roomForJevJob(); err != nil {
		return empty, err
	}
	s.jevState.mu.Lock()
	defer s.jevState.mu.Unlock()
	if s.jevState.closed {
		return empty, decision.ErrDisabled
	}
	job, err = s.jevState.insertJob(job, func() error {
		ids, err := s.jevState.jobIDs()
		if err != nil {
			return err
		}
		if len(ids) >= jevJobLimit {
			return decision.ErrLimit
		}
		return nil
	})
	if err != nil {
		return empty, err
	}
	return job, nil
}

func publicJevJob(job jevJob) map[string]any {
	return map[string]any{"organization": job.Organization, "automatic": job.Automatic, "owner": job.Owner, "id": job.ID, "project": job.Project, "kind": job.Kind, "state": job.State, "examined": job.Examined, "total": job.Total, "remaining": job.Remaining, "updatedAt": job.UpdatedAt, "message": job.Message, "results": []jevJobResult{}}
}

// visibleJevJob shows a job as stored to user; active reports that a runner
// on this gateway holds it.
func (s *Service) visibleJevJob(ctx context.Context, user string, job jevJob, active bool) (jevJob, error) {
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
	if err = validateJevSources(ctx, store, job); err != nil {
		job.Hidden = true
		job.Results = []jevJobResult{}
		job.State = "paused"
		job.Message = "Sources or visibility changed. Start a new review for current evidence."
	}
	if !active && job.State == "running" {
		job.State = "paused"
		job.Message = "Review interrupted; continue its durable checkpoint."
	}
	return job, nil
}

func (s *Service) jevJobGet(w http.ResponseWriter, r *http.Request) {
	user, err := s.hooks.Identity(r)
	if err != nil {
		webcore.Fail(w, err)
		return
	}
	job, active, err := s.jevState.readJobAndRunner(r.PathValue("job"))
	if err != nil || job.Project != webcore.NotebookID(r.Context()) {
		webcore.Fail(w, chat.ErrNotFound)
		return
	}
	job, err = s.visibleJevJob(r.Context(), user, job, active)
	if err != nil {
		webcore.Fail(w, err)
		return
	}
	offset, limit, err := jevPage(r)
	if err != nil {
		webcore.Fail(w, err)
		return
	}
	result, err := s.jevJobPage(job, offset, limit)
	if err != nil {
		webcore.Fail(w, err)
		return
	}
	webcore.JSON(w, result)
}

func (s *Service) jevJobList(w http.ResponseWriter, r *http.Request) {
	user, err := s.hooks.Identity(r)
	if err != nil {
		webcore.Fail(w, err)
		return
	}
	jobs, active, err := s.jevState.jobsAndRunners(map[string]any{"project": webcore.NotebookID(r.Context())})
	if err != nil {
		webcore.Fail(w, err)
		return
	}
	result := []map[string]any{}
	for _, job := range jobs {
		job, err = s.visibleJevJob(r.Context(), user, job, active[job.ID])
		if err != nil {
			webcore.Fail(w, err)
			return
		}
		result = append(result, publicJevJob(job))
	}
	sort.Slice(result, func(i, j int) bool { return result[i]["updatedAt"].(string) > result[j]["updatedAt"].(string) })
	webcore.JSON(w, map[string]any{"jobs": result})
}

func (s *Service) jevJobControl(w http.ResponseWriter, r *http.Request) {
	s.jevState.organizationMu.Lock()
	defer s.jevState.organizationMu.Unlock()
	user, err := s.hooks.Identity(r)
	if err != nil {
		webcore.Fail(w, err)
		return
	}
	id := r.PathValue("job")
	s.jevState.mu.Lock()
	job, err := s.jevState.readJob(id)
	if err != nil || job.Project != webcore.NotebookID(r.Context()) {
		s.jevState.mu.Unlock()
		webcore.Fail(w, chat.ErrNotFound)
		return
	}
	if job.Owner != user {
		if _, err = s.catalog.Access(user, job.Project, "owner"); err != nil {
			s.jevState.mu.Unlock()
			webcore.Fail(w, workspace.ErrForbidden)
			return
		}
	}
	cancel := strings.HasSuffix(r.URL.Path, "/cancel")
	if cancel {
		job, err = s.jevState.updateJob(id, func(job *jevJob) error {
			job.Cancelled = true
			job.State = "cancelled"
			job.UpdatedAt = chat.Now()
			return nil
		})
		if stop := s.jevState.active["job:"+id]; stop != nil {
			stop()
		}
	}
	s.jevState.mu.Unlock()
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
		if err = s.checkJevJob(r.Context(), job, judge); err != nil {
			webcore.Fail(w, err)
			return
		}
		s.launchJevJob(id, true)
	}
	webcore.JSON(w, publicJevJob(job))
}

// One durable review advances at a time. Provider concurrency alone would still
// allow every queued review to load its full pinned source set into memory.
func (s *Service) launchJevJob(id string, explicit bool) {
	s.jevState.mu.Lock()
	defer s.jevState.mu.Unlock()
	if s.jevState.closed || s.jevState.active["job:"+id] != nil || s.outgoing.Paused() {
		return
	}
	busy := false
	for key := range s.jevState.active {
		busy = busy || strings.HasPrefix(key, "job:")
	}
	job, err := s.jevState.updateJob(id, func(job *jevJob) error {
		if job.Cancelled || job.State == "completed" || job.State == "failed" || !explicit && job.State != "queued" {
			return errJevJobIdle
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
	ctx, cancel := context.WithTimeout(s.jevState.ctx, 2*time.Minute)
	key := "job:" + id
	s.jevState.active[key] = cancel
	s.jevState.wg.Add(1)
	go func() {
		defer s.jevState.wg.Done()
		defer cancel()
		defer func() {
			s.jevState.mu.Lock()
			delete(s.jevState.active, key)
			s.jevState.mu.Unlock()
			s.launchNextJevJob()
		}()
		s.runJevJob(ctx, job)
	}()
}

// launchNextJevJob starts the longest-waiting queued job unless one runs on
// this gateway. It reads only queued jobs, so finished ones never hide them.
func (s *Service) launchNextJevJob() {
	s.jevState.mu.Lock()
	if s.jevState.closed {
		s.jevState.mu.Unlock()
		return
	}
	for key := range s.jevState.active {
		if strings.HasPrefix(key, "job:") {
			s.jevState.mu.Unlock()
			return
		}
	}
	s.jevState.mu.Unlock()
	queued, invalid, more, err := s.jevState.scanJobs(map[string]any{"state": "queued", "cancelled": false})
	if err != nil || invalid > 0 || more {
		slog.Warn("jev queue not fully read", "invalid", invalid, "more", more, "error", err)
	}
	next := jevJob{}
	for _, job := range queued {
		if next.ID == "" || jevJobTime(job).Before(jevJobTime(next)) {
			next = job
		}
	}
	if next.ID != "" {
		s.launchJevJob(next.ID, false)
	}
}
