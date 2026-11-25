package webcore

import (
	"context"
	"errors"
)

// A serving process drains when it begins to shut down: it accepts no more
// connections and lets the requests it serves finish, up to its shutdown
// timeout. A request that waits for changes, such as an open page's live
// poll or chat events or an MCP subscription stream, would wait out that
// whole timeout for changes that no longer come. It watches the drain
// instead (UntilDrain) and ends at once, and its client reconnects to the
// next process. Every other request keeps the whole timeout.

type drainKey struct{}

// ErrDraining is the cause of a context UntilDrain ended.
var ErrDraining = errors.New("the server is shutting down")

// WithDrain returns ctx carrying drain, which ends when the serving process
// begins to drain.
func WithDrain(ctx, drain context.Context) context.Context {
	return context.WithValue(ctx, drainKey{}, drain)
}

// UntilDrain returns ctx, also ended, with the cause ErrDraining, when the
// serving process begins to drain. Only requests that wait for changes use
// it. stop releases it.
func UntilDrain(ctx context.Context) (waiting context.Context, stop context.CancelFunc) {
	waiting, cancel := context.WithCancelCause(ctx)
	drain, _ := ctx.Value(drainKey{}).(context.Context)
	if drain == nil {
		return waiting, func() { cancel(context.Canceled) }
	}
	ending := context.AfterFunc(drain, func() { cancel(ErrDraining) })
	return waiting, func() { ending(); cancel(context.Canceled) }
}

// Drained reports whether ctx, from UntilDrain, ended because the serving
// process began to drain.
func Drained(ctx context.Context) bool { return errors.Is(context.Cause(ctx), ErrDraining) }
