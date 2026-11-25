package automation

import (
	"forgetmenote/internal/pg/pgtest"
	"strings"
	"testing"
)

// A connector scope is shared by every writer. Reusing someone else's action
// ID with the same input must not replay their retained result.
func TestRetainedEffectReplaysOnlyForItsActor(t *testing.T) {
	q, err := Open(pgtest.Org(t))
	if err != nil {
		t.Fatal(err)
	}
	begin := func(actor string) (Effect, bool, error) {
		return q.BeginEffect(Effect{Scope: "connector:mail", Action: "reply-one", InputHash: Hash("payload"), Actor: actor, Connection: "mail", Operation: "draft-create"})
	}
	e, created, err := begin("alice")
	if err != nil || !created {
		t.Fatal(err)
	}
	if _, err = q.FinishEffect(e.ID, "accepted", map[string]string{"draft": "ALICE_DRAFT"}); err != nil {
		t.Fatal(err)
	}
	other, created, err := begin("bob")
	if err == nil || created || strings.Contains(string(other.Result), "ALICE_DRAFT") || other.Actor == "alice" {
		t.Fatalf("another actor replayed the retained effect: created=%v err=%v effect=%+v", created, err, other)
	}
	replay, created, err := begin("alice")
	if err != nil || created || !strings.Contains(string(replay.Result), "ALICE_DRAFT") {
		t.Fatalf("same-actor replay: created=%v err=%v effect=%+v", created, err, replay)
	}
}
