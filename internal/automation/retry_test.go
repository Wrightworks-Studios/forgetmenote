package automation

import (
	"context"
	"encoding/json"
	"forgetmenote/internal/connectors"
	"forgetmenote/internal/pg/pgtest"
	"testing"
	"time"
)

func TestDeterministicRetryIsExplicitBoundedAndDelayed(t *testing.T) {
	q, err := Open(pgtest.Org(t))
	if err != nil {
		t.Fatal(err)
	}
	var spec Spec
	json.Unmarshal([]byte(`{"id":"health","kind":"native","actor":"robot","project":"default","native":{"task":"storage-health"},"retry":{"maxAttempts":2,"delaySeconds":60}}`), &spec)
	queued, err := q.Enqueue(spec, "one")
	if err != nil {
		t.Fatal(err)
	}
	q.RunNext(t.Context(), func(context.Context, *Run, func() error) error { return connectors.ErrUnavailable })
	r, err := q.Get(queued.ID)
	if err != nil || r.State != "queued" {
		t.Fatal("transient read was not scheduled for retry", err, r.State)
	}
	if ran, _ := q.RunNext(t.Context(), func(context.Context, *Run, func() error) error { t.Fatal("retry ignored delay"); return nil }); ran {
		t.Fatal("immediate retry")
	}
	raw, _ := json.Marshal(r)
	var fields map[string]any
	json.Unmarshal(raw, &fields)
	fields["retryAt"] = time.Now().Add(-time.Second)
	raw, _ = json.Marshal(fields)
	json.Unmarshal(raw, &r)
	q.write(&r)
	q.RunNext(t.Context(), func(context.Context, *Run, func() error) error { return connectors.ErrUnavailable })
	r, err = q.Get(queued.ID)
	if err != nil || r.State != "failed" || r.Attempts != 2 {
		t.Fatal("retry cap ignored", err, r)
	}
}
