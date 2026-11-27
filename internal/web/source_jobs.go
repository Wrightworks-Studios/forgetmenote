package web

import (
	"context"
	"encoding/json"
	"time"

	"forgetmenote/internal/automation"
	"forgetmenote/internal/sourcehttp"
	"forgetmenote/internal/sources"
	"forgetmenote/internal/workspace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type sourceRestrictions struct {
	Credential *credentialContext   `json:"credential,omitempty"`
	OAuthHash  string               `json:"oauthHash,omitempty"`
	Scope      *string              `json:"scope,omitempty"`
	Client     string               `json:"client,omitempty"`
	Automation *automation.Identity `json:"automation,omitempty"`
	Fence      *automationFence     `json:"fence,omitempty"`
}

func (s *Server) sourceScope(ctx context.Context, project string) sources.JobScope {
	r := sourceRestrictions{}
	if c, ok := credentialFrom(ctx); ok {
		r.Credential = &c
	}
	if value, ok := ctx.Value(scopeKey{}).(string); ok {
		r.Scope = &value
	}
	r.Client, _ = ctx.Value(clientKey{}).(string)
	if lease, ok := ctx.Value(leaseKey{}).(revisionLease); ok && lease.key != digest(s.token) && r.Credential == nil {
		r.OAuthHash = lease.key
	}
	if a, ok := automation.IdentityFrom(ctx); ok {
		r.Automation = &a
	}
	if f, ok := ctx.Value(automationFenceKey{}).(automationFence); ok {
		r.Fence = &f
	}
	raw, _ := json.Marshal(r)
	return sources.JobScope{Subject: subject(ctx), Notebook: project, Generation: s.catalog.Authority(project).Generation, Epoch: s.catalog.Epoch(subject(ctx)), Restrictions: raw}
}
func (s *Server) sourceContext(ctx context.Context, scope sources.JobScope) (context.Context, error) {
	if !s.catalog.ValidSession(scope.Subject, scope.Epoch) || s.catalog.Authority(scope.Notebook).Generation != scope.Generation {
		return ctx, workspace.ErrForbidden
	}
	var r sourceRestrictions
	if json.Unmarshal(scope.Restrictions, &r) != nil {
		return ctx, workspace.ErrForbidden
	}
	ctx = context.WithValue(ctx, identityKey{}, scope.Subject)
	ctx = context.WithValue(ctx, projectIDKey{}, scope.Notebook)
	ctx = workspace.WithAuthority(ctx, scope.Generation)
	if r.Scope != nil {
		ctx = context.WithValue(ctx, scopeKey{}, *r.Scope)
	}
	if r.Client != "" {
		ctx = context.WithValue(ctx, clientKey{}, r.Client)
	}
	if r.Credential != nil {
		ctx = context.WithValue(ctx, credentialContextKey{}, *r.Credential)
	}
	if r.Automation != nil {
		ctx = automation.WithIdentity(ctx, *r.Automation)
	}
	if r.Fence != nil {
		ctx = context.WithValue(ctx, automationFenceKey{}, *r.Fence)
	}
	if r.OAuthHash != "" {
		grant, exists := s.oauthAuthorization(r.OAuthHash)
		if !exists || grant.Subject != scope.Subject || grant.Expires <= time.Now().Unix() || grant.Epoch != scope.Epoch || (grant.Resource != s.origin+"/mcp" && grant.Resource != s.origin+apiPrefix) || r.Scope == nil || grant.Scope != *r.Scope || grant.Client != r.Client {
			return ctx, workspace.ErrForbidden
		}
	}
	store, err := s.toolStore(ctx, scope.Notebook, "edit")
	if err != nil {
		return ctx, err
	}
	return context.WithValue(ctx, projectKey{}, store), nil
}

// acquireSourceWork waits for one of the bounded local extraction slots.
func (s *Server) acquireSourceWork(ctx context.Context) (func(), error) {
	select {
	case s.sourceWork <- struct{}{}:
		return func() { <-s.sourceWork }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// initSourceJobs opens the source jobs. On an instance that turns the sources
// module off no source work runs, so nothing could resume a job: startup
// retires the jobs left from when the module ran and removes their files at
// once, rather than keep a private upload for the rest of its 24 hours.
func (s *Server) initSourceJobs() error {
	if !s.modules.Enabled("sources") {
		return sources.RetireJobs(s.db, s.blobs, s.data)
	}
	jobs, err := sources.OpenJobs(s.db, s.blobs, s.data, func(ctx context.Context, scope sources.JobScope) error {
		_, err := s.sourceContext(ctx, scope)
		return err
	})
	if err != nil {
		return err
	}
	s.sourceJobs = jobs
	s.sourceHTTP, err = sourcehttp.New(jobs, s.renderer, s.sourceWork, sourcehttp.Hooks{Scope: s.sourceScope, Access: s.toolStore, Sign: s.sign})
	return err
}

func (s *Server) sourceRoutes(r routeRegistrar) {
	s.sourceHTTP.Register(r.project)
}

type sourceJobOutput = sourcehttp.JobOutput

type sourceJobInput struct {
	projectArg
	Job string `json:"job"`
}

func (s *Server) sourceJobTools(server *mcp.Server) {
	addProjectTool(s, server, "prepare_source", "Queue a source upload or public URL for bounded server extraction. Returns a durable job ID; inspect source_job, then import_source_job. Local extraction does not upload to an AI provider. To upload your own extraction, use archive_source; jobs refuse text.", "edit", func(ctx context.Context, _ *workspace.Store, in sourceInput) (sourceJobOutput, error) {
		job, err := s.sourceHTTP.Submit(ctx, in.Project, sourcehttp.Input(in))
		return sourceJobOutput{Job: job}, err
	})
	addProjectTool(s, server, "source_job", "Read your source processing progress and completed preview in the original notebook.", "edit", func(ctx context.Context, _ *workspace.Store, in sourceJobInput) (sourceJobOutput, error) {
		return s.sourceHTTP.JobOutput(ctx, s.sourceScope(ctx, in.Project), in.Job)
	})
	addProjectTool(s, server, "cancel_source_job", "Cancel your source job and retain completed local pages for explicit resume.", "edit", func(ctx context.Context, _ *workspace.Store, in sourceJobInput) (sources.JobStatus, error) {
		return s.sourceJobs.Cancel(ctx, s.sourceScope(ctx, in.Project), in.Job)
	})
	addProjectTool(s, server, "resume_source_job", "Explicitly resume your interrupted local source job from its completed pages.", "edit", func(ctx context.Context, _ *workspace.Store, in sourceJobInput) (sources.JobStatus, error) {
		return s.sourceJobs.Resume(ctx, s.sourceScope(ctx, in.Project), in.Job)
	})
	addProjectTool(s, server, "import_source_job", "Commit your completed source job's original and extracted text, without repeating extraction. Refresh requires path and current revision.", "edit", func(ctx context.Context, store *workspace.Store, in struct {
		sourceJobInput
		Title    string  `json:"title,omitempty"`
		Path     string  `json:"path,omitempty"`
		Revision string  `json:"revision,omitempty"`
		Folder   *string `json:"folder,omitempty"`
	}) (noteOutput, error) {
		capture, err := s.sourceJobs.Result(ctx, s.sourceScope(ctx, in.Project), in.Job)
		if err != nil {
			return noteOutput{}, err
		}
		if _, err := s.toolStore(ctx, in.Project, "edit"); err != nil {
			return noteOutput{}, err
		}
		doc, err := store.ImportSource(ctx, capture, in.Path, in.Revision, in.Title, in.Folder)
		return s.output(doc), err
	})
}
