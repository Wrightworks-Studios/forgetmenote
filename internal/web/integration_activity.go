package web

import "errors"

// The catalog cannot transact another owner's state. Record intent before that
// boundary and its outcome afterward; an interrupted operation remains visible.
func integrationActivity[T any](s *Server, actor, action, project string, run func() (T, error)) (T, error) {
	details := map[string]string{"operation": RandomToken()}
	if err := s.catalog.RecordActivity(actor, action, project, "pending", details); err != nil {
		var zero T
		return zero, err
	}
	result, err := run()
	outcome := "succeeded"
	if err != nil {
		outcome = "failed"
	}
	if receipt := s.catalog.RecordActivity(actor, action, project, outcome, details); receipt != nil {
		return result, errors.Join(err, errors.New("operation completed without a final activity receipt; inspect current state before retrying"), receipt)
	}
	return result, err
}
