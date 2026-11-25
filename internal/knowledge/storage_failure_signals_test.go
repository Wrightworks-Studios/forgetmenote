package knowledge

import (
	"strings"
	"testing"
)

func TestPersistentVectorFailureSignalsOnlyOnTransition(t *testing.T) {
	for _, scope := range []string{"current", "history"} {
		t.Run(scope, func(t *testing.T) {
			engine, _ := indexFixture(t)
			if _, err := engine.Configure(t.Context(), testConfig(), false); err != nil {
				t.Fatal(err)
			}
			pending, err := engine.PendingScope(t.Context(), 1, scope)
			if err != nil || len(pending.Passages) != 1 {
				t.Fatalf("fixture pending: %+v %v", pending, err)
			}
			p := pending.Passages[0]
			sub := Submission{Scope: scope, Revision: pending.Status.Revision, ConfigID: pending.Status.ConfigID, Vectors: []VectorSubmission{{ID: p.ID, Hash: p.Hash, NoteHash: p.NoteHash, Vector: []float32{1, 0, 0}}}}
			if _, err = engine.Submit(t.Context(), sub); err != nil {
				t.Fatal(err)
			}
			engine.Close()
			signals := 0
			state := NewProcessing(func(reset bool) {
				signals++
				if reset {
					t.Error("storage failure reset provider retry state")
				}
			})
			reopened, err := OpenWithProcessing(database(t, engine.Store), engine.Store, state)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			restore := failVectors(t, reopened)
			for i := 0; i < 4; i++ {
				// Each submission reaches Postgres and is refused there.
				if _, err = reopened.Submit(t.Context(), sub); err == nil || !strings.Contains(err.Error(), "injected write failure") {
					t.Fatal("fixture did not reject failed publication", err)
				}
			}
			if signals != 1 {
				t.Fatalf("persistent failure generated%d recovery wakeups, want1", signals)
			}
			restore()
			if _, err = reopened.Submit(t.Context(), sub); err != nil {
				t.Fatal(err)
			}
			failVectors(t, reopened)
			if _, err = reopened.Submit(t.Context(), sub); err == nil {
				t.Fatal("new failure was not reported")
			}
			if signals != 2 {
				t.Fatalf("recovered then failed storage generated%d wakeups, want2", signals)
			}
		})
	}
}
