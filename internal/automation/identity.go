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

// Reads reports whether the run may read project: its own notebook or one it
// was given to read.
func (a Identity) Reads(project string) bool {
	return project == a.Spec.Project || slices.Contains(a.Spec.ReadProjects, project)
}

// Writes reports whether the run may change project: only its own notebook.
func (a Identity) Writes(project string) bool { return project == a.Spec.Project }
