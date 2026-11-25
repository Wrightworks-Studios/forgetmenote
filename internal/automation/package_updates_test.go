package automation

import (
	"context"
	"errors"
	"forgetmenote/internal/pg/pgtest"
	"testing"
)

func TestPackageUpdateWaitsForActiveRunAndFencesQueuedSnapshot(t *testing.T) {
	runtime, err := OpenRuntime(pgtest.Org(t))
	if err != nil {
		t.Fatal(err)
	}
	spec := Spec{ID: "update", Kind: "native", Actor: "robot", Project: "default", Native: &NativeTask{Task: "storage-health"}}
	definition, err := runtime.Definitions.Save(spec, "")
	if err != nil {
		t.Fatal(err)
	}
	run, err := runtime.Queue.Enqueue(definition.Spec, "old")
	if err != nil {
		t.Fatal(err)
	}
	started, finish, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		_, err := runtime.Queue.RunNext(t.Context(), func(_ context.Context, r *Run, _ func() error) error {
			close(started)
			<-finish
			if Hash(r.Spec) != definition.Revision {
				t.Error("running snapshot changed")
			}
			return nil
		})
		done <- err
	}()
	<-started
	spec = definition.Spec
	spec.Name = "Updated package"
	changed, err := runtime.Queue.UpdateIdleDefinition(runtime.Definitions, spec, definition.Revision)
	if err != nil || changed {
		t.Error("update interrupted active work", changed, err)
	}
	close(finish)
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	queued, err := runtime.Queue.Enqueue(definition.Spec, "waiting")
	if err != nil {
		t.Fatal(err)
	}
	changed, err = runtime.Queue.UpdateIdleDefinition(runtime.Definitions, spec, definition.Revision)
	if err != nil || !changed {
		t.Fatal("idle update did not commit", changed, err)
	}
	old, _ := runtime.Queue.Get(run.ID)
	if old.State != "completed" || Hash(old.Spec) != definition.Revision {
		t.Fatal("completed run history changed", old)
	}
	waiting, _ := runtime.Queue.Get(queued.ID)
	if waiting.State != "cancelled" {
		t.Fatal("old queued snapshot could still execute", waiting)
	}
	if _, err = runtime.Definitions.Save(spec, definition.Revision); err == nil {
		t.Fatal("automatic update ignored definition revision checks")
	}
	current, _ := runtime.Definitions.Get(spec.ID)
	if _, err = runtime.Queue.Enqueue(current.Spec, "unknown-effect"); err != nil {
		t.Fatal(err)
	}
	if _, err = runtime.Queue.RunNext(t.Context(), func(_ context.Context, r *Run, save func() error) error {
		r.Script = &ScriptCheckpoint{Steps: map[string]*ScriptStep{"paid": {Effect: true, State: "running"}}}
		if err := save(); err != nil {
			return err
		}
		return errors.New("provider outcome unknown")
	}); err != nil {
		t.Fatal(err)
	}
	spec = current.Spec
	spec.Name = "Another update"
	if changed, err = runtime.Queue.UpdateIdleDefinition(runtime.Definitions, spec, current.Revision); err != nil || changed {
		t.Fatal("unresolved effects did not block package update", changed, err)
	}
}
