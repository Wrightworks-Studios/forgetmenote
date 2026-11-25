package knowledge

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestKnowledgeOpenRejectsStateBeyondItsBudget(t *testing.T) {
	engine, _ := indexFixture(t)
	engine.Close()
	raw, err := json.Marshal(state{Checkpoints: map[string]Checkpoint{"review": {Summary: strings.Repeat("x", 2048)}}})
	if err != nil {
		t.Fatal(err)
	}
	writeDocument(t, engine, stateKind, string(raw))
	t.Setenv("FMN_LIMIT_STATE_BYTES", "1024")
	reopened, err := Open(database(t, engine.Store), engine.Store)
	if err == nil {
		reopened.Close()
		t.Fatal("oversized private knowledge state was accepted")
	}
}

func TestKnowledgeCheckpointBudgetPreservesPriorState(t *testing.T) {
	engine, doc := indexFixture(t)
	prior, err := engine.Complete(t.Context(), "agent", "review", "", doc.Revision, "Reviewed", false)
	if err != nil {
		t.Fatal(err)
	}
	version, before := storedDocument(t, engine, stateKind)
	t.Setenv("FMN_LIMIT_STATE_BYTES", "512")
	if _, err = engine.Complete(t.Context(), "agent", "review", prior.Revision, doc.Revision, strings.Repeat("x", 800), false); err == nil {
		t.Fatal("successful checkpoint cannot be reopened within the state budget")
	}
	if after, body := storedDocument(t, engine, stateKind); after != version || body != before {
		t.Fatal("rejected checkpoint changed durable state")
	}
	current, err := engine.Checkpoint(t.Context(), "agent", "review")
	if err != nil || current != prior {
		t.Fatal("rejected checkpoint changed live state", err)
	}
}
