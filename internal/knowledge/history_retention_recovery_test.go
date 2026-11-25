package knowledge

import (
	"errors"
	"testing"
)

func TestHistoryDirectorySyncRecoverySurvivesEviction(t *testing.T) {
	for _, reopen := range []bool{false, true} {
		name := "resident"
		if reopen {
			name = "reopened"
		}
		t.Run(name, func(t *testing.T) {
			engine, _ := indexFixture(t)
			engine.historySync = func(string) error { return errors.New("synthetic final directory-sync failure") }
			pending, err := engine.PendingScope(t.Context(), 1, "history")
			if err != nil {
				t.Fatal(err)
			}
			if pending.Status.History == nil || pending.Status.History.State != "unavailable" {
				t.Fatalf("fault did not reach checkpoint final-sync boundary: %+v", pending.Status.History)
			}
			engine.historySync = syncKnowledgeDirectory
			if reopen {
				processing := engine.Processing
				engine.Close()
				engine, err = OpenWithProcessing(database(t, engine.Store), engine.Store, processing)
				if err != nil {
					t.Fatal(err)
				}
			}
			defer engine.Close()
			for i := 0; i < 3 && engine.ProcessBatch(t.Context(), i == 0); i++ {
			}
			pending, err = engine.PendingScope(t.Context(), 1, "history")
			if err != nil {
				t.Fatal(err)
			}
			if pending.Status.History == nil || pending.Status.History.State != "ready" {
				t.Fatalf("successful persistence recovery did not clear the checkpoint failure after reopen=%v: %+v", reopen, pending.Status.History)
			}
		})
	}
}
