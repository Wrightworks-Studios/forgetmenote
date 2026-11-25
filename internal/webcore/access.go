package webcore

import (
	"context"
	"time"
)

// accessRecheck bounds how long in-flight work outlives a revocation. Work also
// checks access once when it starts and again before it discloses a result.
const accessRecheck = time.Second

// One request lifetime owns its revocation monitor, including cleanup on cancel.
func MonitorAccess(parent context.Context, authorize func(context.Context) error) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(parent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(accessRecheck)
		defer ticker.Stop()
		for {
			if err := authorize(ctx); err != nil {
				cancel(err)
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return ctx, func() { cancel(context.Canceled); <-done }
}
