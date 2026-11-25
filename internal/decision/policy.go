package decision

import (
	"context"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/workspace"
)

type Preferences struct {
	Consent   bool `json:"consent"`
	Rerank    bool `json:"rerank"`
	Citations bool `json:"citations"`
}

// Policy joins live instance settings with existing per-user consent. Once an
// instance adopts policy, private legacy settings cannot override it.
type Policy struct {
	catalog    *catalog.Catalog
	configured func() (*Service, bool)
	legacy     func(string) (Preferences, error)
}

func NewPolicy(c *catalog.Catalog, current func() (*Service, bool), legacy func(string) (Preferences, error)) *Policy {
	return &Policy{c, current, legacy}
}
func (s *Policy) Preferences(user string) (Preferences, error) {
	if settings, managed := s.catalog.AISettings(); managed {
		return Preferences{Consent: settings.Rerank || settings.MapClassification || settings.Citations || settings.Reviews || settings.Writing, Rerank: settings.Rerank, Citations: settings.Citations}, nil
	}
	return s.legacy(user)
}
func (s *Policy) Authorize(ctx context.Context, user, project, feature string, expected *Service) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if !s.catalog.Enabled(user) {
		return workspace.ErrForbidden
	}
	if _, err := s.catalog.Access(user, project, "read"); err != nil {
		return err
	}
	judge, localOnly := s.configured()
	if localOnly {
		return ErrLocalOnly
	}
	if judge == nil || judge != expected {
		return ErrDisabled
	}
	if policy, managed := s.catalog.AISettings(); managed {
		allowed := false
		switch feature {
		case "rerank":
			allowed = policy.Rerank
		case "citations":
			allowed = policy.Citations
		case "repository-map":
			allowed = policy.MapClassification
		case "organization":
			allowed = policy.Writing
		default:
			allowed = policy.Reviews
		}
		if !allowed {
			return ErrDisabled
		}
	}
	prefs, err := s.Preferences(user)
	if err != nil {
		return err
	}
	if !prefs.Consent || feature == "rerank" && !prefs.Rerank || feature == "citations" && !prefs.Citations {
		return ErrDisabled
	}
	return nil
}
