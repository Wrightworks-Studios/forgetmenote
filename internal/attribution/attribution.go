// Package attribution carries server-resolved identity to Git transactions.
package attribution

import "context"

type Identity struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Email  string `json:"email"`
	Source string `json:"source,omitempty"`
	Client string `json:"client,omitempty"`
}
type key struct{}

func With(ctx context.Context, identity Identity) context.Context {
	return context.WithValue(ctx, key{}, identity)
}
func From(ctx context.Context) Identity {
	if identity, ok := ctx.Value(key{}).(Identity); ok {
		return identity
	}
	return Identity{Name: "ForgetMeNote", Email: "forgetmenote@localhost", Source: "system"}
}

// WithSource binds server-resolved attribution to one concrete invocation.
func WithSource(ctx context.Context, identity Identity, source, client string) context.Context {
	identity.Source, identity.Client = source, client
	return With(ctx, identity)
}
