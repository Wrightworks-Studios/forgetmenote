package webcore

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestMonitorStopsRevokedWorkAndJoins(t *testing.T) {
	revoked := errors.New("access revoked")
	var allowed atomic.Bool
	allowed.Store(true)
	checked := make(chan struct{}, 1)
	ctx, stop := MonitorAccess(t.Context(), func(context.Context) error {
		select {
		case checked <- struct{}{}:
		default:
		}
		if !allowed.Load() {
			return revoked
		}
		return nil
	})
	defer stop()
	select {
	case <-checked:
	case <-time.After(time.Second):
		t.Fatal("authorization was not checked")
	}
	allowed.Store(false)
	select {
	case <-ctx.Done():
	case <-time.After(2 * accessRecheck):
		t.Fatal("revoked work was not canceled within the recheck interval")
	}
	if !errors.Is(context.Cause(ctx), revoked) {
		t.Fatal("revocation cause lost", context.Cause(ctx))
	}
	// The cleanup is safe after cancellation and waits for the monitor to exit.
	stop()
}
