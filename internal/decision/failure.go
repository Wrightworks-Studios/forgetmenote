package decision

import "errors"

// Failure says where Evaluate failed, for a caller's log: the step, and the
// provider's HTTP status when it answered with one. It holds no request or
// answer content.
type Failure struct {
	// Step is scope, prepare, queue, check, reserve, provider, settle or deadline.
	Step   string `json:"step"`
	Status int    `json:"status,omitempty"`
}

// failed wraps an Evaluate error with its Failure. Its text and errors.Is
// behave as the error it wraps, so callers that show or match the error see
// no difference.
type failed struct {
	Failure
	err error
}

func (f *failed) Error() string { return f.err.Error() }
func (f *failed) Unwrap() error { return f.err }

func failure(step string, err error) error {
	f := &failed{Failure: Failure{Step: step}, err: err}
	var status *providerStatus
	if errors.As(err, &status) {
		f.Status = status.code
	}
	return f
}

// Failed reports where an error returned by Evaluate arose; ok is false for
// any other error.
func Failed(err error) (Failure, bool) {
	var f *failed
	if errors.As(err, &f) {
		return f.Failure, true
	}
	return Failure{}, false
}

// providerStatus is a provider's non-success HTTP response. Its text is the
// error it wraps; the response body is never kept.
type providerStatus struct {
	code int
	err  error
}

func (p *providerStatus) Error() string { return p.err.Error() }
func (p *providerStatus) Unwrap() error { return p.err }
