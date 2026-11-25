// Package outgoing owns an instance's outgoing mode. A copy restored beside the
// live instance pauses outgoing actions so it cannot act on the outside world as
// that instance: no messages, automation effects, background paid AI, backup
// leases or personal token refreshes. Each owner that acts outward consults the
// shared Mode at its own boundary; reads that change nothing outside continue.
package outgoing

import (
	"errors"
	"sync/atomic"
)

// ErrPaused refuses a request that would act outside this instance.
var ErrPaused = errors.New("outgoing actions are paused on this instance")

// Parse reads the operator setting. Only "paused" pauses; an empty value or
// "enabled" keeps outgoing actions. Anything else fails startup rather than
// running a copy unpaused because of a typing mistake.
func Parse(value string) (paused bool, err error) {
	switch value {
	case "", "enabled":
		return false, nil
	case "paused":
		return true, nil
	}
	return false, errors.New(`outgoing must be "paused" or "enabled"`)
}

// Mode is shared by the owners that act outward. Startup configuration pauses
// it before any background duty starts or request is admitted. A nil or zero
// Mode allows outgoing actions.
type Mode struct{ paused atomic.Bool }

func (m *Mode) Pause()       { m.paused.Store(true) }
func (m *Mode) Paused() bool { return m != nil && m.paused.Load() }

// Check returns ErrPaused while outgoing actions are paused.
func (m *Mode) Check() error {
	if m.Paused() {
		return ErrPaused
	}
	return nil
}
