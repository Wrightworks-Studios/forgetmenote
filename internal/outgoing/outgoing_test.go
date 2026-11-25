package outgoing

import (
	"errors"
	"testing"
)

func TestOnlyAnExplicitSettingPausesOutgoingActions(t *testing.T) {
	for value, want := range map[string]bool{"": false, "enabled": false, "paused": true} {
		paused, err := Parse(value)
		if err != nil || paused != want {
			t.Fatalf("%q: paused=%v err=%v", value, paused, err)
		}
	}
	for _, value := range []string{"pause", "Paused", "true", "off"} {
		if _, err := Parse(value); err == nil {
			t.Fatalf("%q was accepted; a mistyped setting must not run unpaused", value)
		}
	}
	var unset *Mode
	if unset.Check() != nil || (&Mode{}).Check() != nil {
		t.Fatal("an unconfigured mode paused outgoing actions")
	}
	m := &Mode{}
	m.Pause()
	if !m.Paused() || !errors.Is(m.Check(), ErrPaused) {
		t.Fatal("a paused mode allowed outgoing actions")
	}
}
