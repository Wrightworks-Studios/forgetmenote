package automation

import (
	"context"
	"slices"
)

// Identity is the authority an automation run acts with: its definition, the
// account it runs as, and the account, notebook access and notebook authority
// generations checked when the run started. Each check of the run compares
// them with the current catalog.
type Identity struct {
	Spec                            Spec
	Account                         string
	Epoch, ProjectEpoch, Generation uint64
}

type identityKey struct{}

// WithIdentity binds the run's identity. Besides restricting the notebooks the
// run reaches, it is the authority for the run's own writes and admits its
// code-only data connections, so only a run start that checked it may bind it.
func WithIdentity(ctx context.Context, a Identity) context.Context {
	return context.WithValue(ctx, identityKey{}, a)
}

// IdentityFrom returns the automation run's identity, if the invocation is one.
func IdentityFrom(ctx context.Context) (Identity, bool) {
	a, ok := ctx.Value(identityKey{}).(Identity)
	return a, ok
}

type scriptHostKey struct{}

// WithScriptHost marks the invocation as an installed package's host call.
// Installed Python runs have an explicit host boundary: inside a chat scope,
// which a script run keeps for notebook freshness checks, the mark admits only
// the run's own code-only data connections, and it does not turn catalog reads
// into AI tools. Only the script runner may set it, for the run it started.
func WithScriptHost(ctx context.Context) context.Context {
	return context.WithValue(ctx, scriptHostKey{}, true)
}

// ScriptHost reports whether the invocation is an installed package's host
// call.
func ScriptHost(ctx context.Context) bool {
	host, _ := ctx.Value(scriptHostKey{}).(bool)
	return host
}

// Reads reports whether the run may read project: its own notebook or one it
// was given to read.
func (a Identity) Reads(project string) bool {
	return project == a.Spec.Project || slices.Contains(a.Spec.ReadProjects, project)
}

// Writes reports whether the run may change project: only its own notebook.
func (a Identity) Writes(project string) bool { return project == a.Spec.Project }
