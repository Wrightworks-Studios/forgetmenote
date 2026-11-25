package chat

import (
	"context"
	"errors"

	"forgetmenote/internal/actions"
	"forgetmenote/internal/workspace"
)

// ErrOutsideCorpus refuses a source or action that the run's saved corpus does
// not include.
var ErrOutsideCorpus = errors.New("the requested source or action is outside this run's saved corpus")

// RunScope is what one chat run may read and change: its conversation, the
// run with its saved corpus, the user turn that started it, and the content
// scope that corpus allows. It rides on the invocation's context.
type RunScope struct {
	Owner, Project, Chat string
	Run                  Run
	Origin               Turn
	Content              workspace.ContentScope
	// Personal reports that this conversation has used personal assistant
	// context; outside the person's own assistant notebook its changes need
	// the person's approval.
	Personal bool
	// approved marks the operation the person approved. Only WithApproval sets
	// it, so a scope built or copied elsewhere cannot carry that approval.
	approved bool
}

// Approved reports whether the person approved the operation about to run.
func (s *RunScope) Approved() bool { return s.approved }

type runScopeKey struct{}

// ContentScope is the notebook content a run's saved corpus allows.
func ContentScope(run Run) (workspace.ContentScope, error) {
	if err := run.Options.Corpus.Validate(); err != nil {
		return workspace.ContentScope{}, err
	}
	return workspace.ContentScope{NotesOnly: true, Policy: workspace.ContainerPolicy{Version: 1, Archives: run.Options.Corpus.Archives, Drafts: run.Options.Corpus.Drafts}, Collection: "all"}, nil
}

// PersonalContext reports whether any run in the conversation carried the
// person's assistant memory or agent memory, or read their connected accounts.
// Retained answers and transcript excerpts carry that context into later runs.
func PersonalContext(c Conversation) bool {
	for _, run := range c.Runs {
		if run.PersonalConsent != "" {
			return true
		}
		for _, op := range run.Operations {
			if name := actions.LegacyName(op.Name); name == "read_personal_source" || name == "assistant_context" || name == "recall_memory" || name == "remember" {
				return true
			}
		}
	}
	return false
}

// WithRunScope binds the scope of run, started by its user turn in c. Like the
// other functions that store a scope, it may be used only where web's boundary
// test lists it, after the run's own access check.
func WithRunScope(ctx context.Context, c Conversation, run Run) (context.Context, error) {
	if run.Options.Corpus != nil && len(run.Options.Corpus.SourceProjects) > 0 && len(run.Options.Corpus.SourceBindings) != len(run.Options.Corpus.SourceProjects) {
		return nil, ErrCorpus
	}
	scope, err := ContentScope(run)
	if err != nil {
		return nil, err
	}
	for _, turn := range c.Turns {
		if turn.Role == "user" && turn.RunID == run.ID {
			return context.WithValue(ctx, runScopeKey{}, &RunScope{Owner: c.Owner, Project: c.Project, Chat: c.ID, Run: run, Origin: turn, Content: scope, Personal: PersonalContext(c)}), nil
		}
	}
	return nil, errors.New("run is missing its original user turn")
}

// WithScope binds scope in place of the current one, which can widen what the
// run may reach. Its listed callers build scope from a run or narrow the
// current one.
func WithScope(ctx context.Context, scope *RunScope) context.Context {
	return context.WithValue(ctx, runScopeKey{}, scope)
}

// WithoutRunScope lifts the chat run's scope, for work that runs under an
// automation's own identity and restrictions instead.
func WithoutRunScope(ctx context.Context) context.Context {
	return context.WithValue(ctx, runScopeKey{}, (*RunScope)(nil))
}

// WithApproval binds the person's approval of the operation about to run.
func WithApproval(ctx context.Context) context.Context {
	scope := RunScopeFrom(ctx)
	if scope == nil {
		return ctx
	}
	approved := *scope
	approved.approved = true
	return context.WithValue(ctx, runScopeKey{}, &approved)
}

// RunScopeFrom returns the chat run's scope, or nil outside a chat run.
func RunScopeFrom(ctx context.Context) *RunScope {
	scope, _ := ctx.Value(runScopeKey{}).(*RunScope)
	return scope
}

// RunAuth is the authorization a chat run started with: its owner, notebook,
// browser session and account epoch. Each check of the run compares it with
// the current catalog and sessions.
type RunAuth struct {
	User, Project, Session string
	Epoch                  uint64
}

type runAuthKey struct{}

// WithRunAuth binds the run's authorization, so a check made during the run
// also confirms it.
func WithRunAuth(ctx context.Context, a RunAuth) context.Context {
	return context.WithValue(ctx, runAuthKey{}, a)
}

// RunAuthFrom returns the run's authorization, if the invocation has one.
func RunAuthFrom(ctx context.Context) (RunAuth, bool) {
	a, ok := ctx.Value(runAuthKey{}).(RunAuth)
	return a, ok
}
