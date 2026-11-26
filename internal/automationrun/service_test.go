package automationrun

import (
	"reflect"
	"testing"

	"forgetmenote/internal/agentmemory"
	"forgetmenote/internal/automation"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chatrun"
	"forgetmenote/internal/outgoing"
)

// TestNewRefusesAMissingHook starts the engine only with every hook bound, so
// a check web forgot to bind cannot pass as allowed.
func TestNewRefusesAMissingHook(t *testing.T) {
	var all Hooks
	fields := reflect.ValueOf(&all).Elem()
	for i := range fields.NumField() {
		f := fields.Field(i)
		f.Set(reflect.MakeFunc(f.Type(), func([]reflect.Value) []reflect.Value {
			t.Fatal("New called a hook")
			return nil
		}))
	}
	start := func(h Hooks) error {
		_, err := New(&catalog.Catalog{}, &automation.Runtime{}, &chatrun.Service{}, &agentmemory.Service{}, &outgoing.Mode{}, t.TempDir(), h)
		return err
	}
	if err := start(all); err != nil {
		t.Fatal(err)
	}
	for i := range fields.NumField() {
		missing := all
		reflect.ValueOf(&missing).Elem().Field(i).SetZero()
		if start(missing) == nil {
			t.Errorf("New accepted hooks without %s", fields.Type().Field(i).Name)
		}
	}
}
