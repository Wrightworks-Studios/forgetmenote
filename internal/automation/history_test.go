package automation

import (
	"bytes"
	"context"
	"encoding/json"
	"forgetmenote/internal/pg/pgtest"
	"testing"
	"time"
)

func TestQueueRetainsHistoryButRejectsObsoleteResumableDefinitions(t *testing.T) {
	for _, tc := range []struct {
		state   string
		history bool
	}{
		{"completed", true}, {"failed", true}, {"cancelled", true},
		{"queued", false}, {"running", false}, {"action-required", false},
	} {
		t.Run(tc.state, func(t *testing.T) {
			dir := t.TempDir()
			spec := Spec{ID: "old-monitor", Kind: "monitor", Actor: "robot", Project: "default", Calls: []Call{
				{Connection: "metrics", Operation: "read"},
				{Connection: "metrics", Operation: "read"},
				{Connection: "metrics", Operation: "read"},
			}}
			r := Run{Version: 5, ID: Hash(tc.state), Spec: spec, State: tc.state, Result: "retained diagnostic", Created: time.Now(), Updated: time.Now()}
			before, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := (records{db: pgtest.Dir(t, dir)}).write(r.ID+".json", before, 0); err != nil {
				t.Fatal(err)
			}
			q, err := Open(pgtest.Dir(t, dir))
			if tc.history {
				if err != nil {
					t.Fatal("historical definition blocked startup", err)
				}
				got, err := q.Get(r.ID)
				if err != nil || got.State != tc.state || got.Result != r.Result || len(got.Spec.Calls) != 3 {
					t.Fatal("historical run lost", got, err)
				}
				if _, err := q.Enqueue(spec, "new"); err == nil {
					t.Fatal("obsolete definition accepted for new work")
				}
				ran, err := q.RunNext(t.Context(), func(context.Context, *Run, func() error) error {
					t.Fatal("terminal history executed")
					return nil
				})
				if err != nil || ran {
					t.Fatal("historical run changed scheduling", ran, err)
				}
			} else if err == nil {
				t.Fatal("obsolete definition accepted for recovery")
			}
			after, _, err := (records{db: pgtest.Dir(t, dir)}).read(r.ID+".json", 1<<20)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("reading saved history changed its bytes", err)
			}
		})
	}
}
