package messaginghttp

import (
	"context"
	"errors"
	"strings"
	"time"

	"forgetmenote/internal/automation"
	"forgetmenote/internal/messaging"
	"forgetmenote/internal/workspace"
)

// Progress is best effort and never changes whether the answer succeeds. The
// refresh belongs to this run and is joined before its final clear.
func (s *Service) messageProgress(ctx context.Context, transport messaging.Transport, address messaging.Address) func() {
	t, ok := transport.(messaging.StatusTransport)
	if !ok {
		return func() {}
	}
	update := func(text string) {
		statusCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		if err := t.Status(statusCtx, address, text); err != nil {
			s.automations.Report("chat progress", err)
		}
	}
	update("is working on your request…")
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(time.Minute)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-tick.C:
				update("is working on your request…")
			}
		}
	}()
	return func() { close(stop); <-done; update("") }
}

func (s *Service) messageFailure(ctx context.Context, transport messaging.Transport, address messaging.Address, run *automation.Run, save func() error, cause error) error {
	if errors.Is(cause, context.Canceled) || errors.Is(cause, workspace.ErrForbidden) || strings.HasPrefix(run.DeliveryState, "failure-") {
		return cause
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	if s.CheckMessageSpec(run.Spec) != nil || s.VerifyMessageMember(ctx, address) != nil {
		return cause
	}
	// Fence the notification before sending. An ambiguous delivery is never
	// repeated automatically, just like successful answer delivery.
	run.DeliveryState = "failure-sending"
	if err := save(); err != nil {
		run.DeliveryState = ""
		return errors.Join(cause, err)
	}
	text := "I couldn't finish this request."
	if errors.Is(cause, context.DeadlineExceeded) {
		text += " The request timed out."
	}
	text += " An administrator can inspect run " + run.ID + "."
	if err := transport.Send(ctx, address, messaging.Reply{Text: text}); err != nil {
		return errors.Join(cause, err)
	}
	run.DeliveryState = "failure-sent"
	return errors.Join(cause, save())
}
