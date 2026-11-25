package web

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"forgetmenote/internal/githubapp"
	"forgetmenote/internal/pg"
	"forgetmenote/internal/webcore"
)

// Pending provider flows live in Postgres between their start and their
// callback, so the provider may return the browser to any gateway. A flow's
// key is the digest of its state or binding; its body is the flow's own JSON.
var (
	errFlowsFull       = webcore.Error{Status: 429, Code: "capacity", Message: "Too many pending connections; try again later"}
	errFlowUnavailable = webcore.Error{Status: 503, Code: "unavailable", Message: "Connection state is unavailable; try again shortly"}
)

// flowLimit bounds each flow group's live flows.
const flowLimit = 256

func flowError(err error) error {
	switch {
	case err == nil, errors.Is(err, pg.ErrNoFlow):
		return err
	case errors.Is(err, pg.ErrFlowsFull):
		return errFlowsFull
	}
	return errors.Join(errFlowUnavailable, err)
}

func (s *Server) beginFlow(ctx context.Context, kind, key, subject string, value any, ttl time.Duration, bound pg.FlowBound) error {
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return flowError(s.db.BeginFlow(ctx, kind, key, subject, body, ttl, bound))
}

func (s *Server) putFlow(ctx context.Context, kind, key, subject string, value any, expires time.Time) error {
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return flowError(s.db.PutFlow(ctx, kind, key, subject, body, expires))
}

// endFlow removes a flow its last step finished.
func (s *Server) endFlow(ctx context.Context, kind, key string) error {
	_, err := s.db.TakeFlow(ctx, kind, key)
	if errors.Is(err, pg.ErrNoFlow) {
		return nil
	}
	return flowError(err)
}

// readFlow returns a live flow; ok is false when there is none.
func readFlow[T any](s *Server, ctx context.Context, kind, key string) (value T, ok bool, err error) {
	body, err := s.db.ReadFlow(ctx, kind, key)
	if errors.Is(err, pg.ErrNoFlow) {
		return value, false, nil
	}
	if err != nil {
		return value, false, flowError(err)
	}
	if err = json.Unmarshal(body, &value); err != nil {
		return value, false, flowError(err)
	}
	return value, true, nil
}

// claimFlow completes a live flow once when valid accepts it. A flow valid
// refuses stays pending, so a request with the wrong binding cannot consume
// another browser's flow.
func claimFlow[T any](s *Server, ctx context.Context, kind, key string, valid func(T) bool) (value T, ok bool, err error) {
	if value, ok, err = readFlow[T](s, ctx, kind, key); err != nil || !ok || !valid(value) {
		return value, false, err
	}
	if _, err = s.db.TakeFlow(ctx, kind, key); errors.Is(err, pg.ErrNoFlow) {
		return value, false, nil
	}
	return value, err == nil, flowError(err)
}

// updateFlow applies change to a live flow while holding it, so steps on two
// gateways apply one at a time. It returns pg.ErrNoFlow when there is none,
// and change's error leaves the flow unchanged.
func updateFlow[T any](s *Server, ctx context.Context, kind, key string, change func(*T) error) error {
	var refused error
	err := s.db.UpdateFlow(ctx, kind, key, func(body []byte) ([]byte, error) {
		var value T
		if err := json.Unmarshal(body, &value); err != nil {
			return nil, err
		}
		if refused = change(&value); refused != nil {
			return nil, refused
		}
		return json.Marshal(value)
	})
	if refused != nil {
		return refused
	}
	return flowError(err)
}

// A GitHub App grant waits between its authorization callback and the
// repository selection. Its user token is sealed under the App key, which only
// gateways hold, before the grant is stored.
func (s *Server) saveGrant(ctx context.Context, kind string, app *githubapp.Client, p repositoryGrant) error {
	sealed, err := app.SealToken(p.Token)
	if err != nil {
		return err
	}
	p.Token = sealed
	return s.putFlow(ctx, kind, p.Binding, p.Subject, p, p.Expires)
}

// grant returns the live grant stored under binding with its token opened. A
// grant sealed under another App key is no grant: its user authorizes again.
func (s *Server) grant(ctx context.Context, kind, binding string, app *githubapp.Client) (repositoryGrant, bool, error) {
	p, ok, err := readFlow[repositoryGrant](s, ctx, kind, binding)
	if err != nil || !ok {
		return p, false, err
	}
	if p.Token, err = app.OpenToken(p.Token); err != nil {
		return repositoryGrant{}, false, nil
	}
	return p, true, nil
}
