package web

import (
	"context"
	"testing"
)

func TestJudgmentConsistencyRepeatedPairReconcilesExistingFinding(t *testing.T) {
	app := reviewFixture(t, syntheticReviewAnswer)
	for _, body := range []string{`{"kind":"consistency","operationId":"first-pair","sources":[{"path":"a.md"}]}`, `{"kind":"consistency","operationId":"reverse-pair","sources":[{"path":"b.md"}]}`} {
		id := startReview(t, app, body)
		if job := waitReview(t, app, id); job["state"] != "completed" {
			t.Fatal(job)
		}
	}
	store, _ := app.catalog.Access("alice", "default", "read")
	findings, err := store.Findings(context.Background())
	if err != nil || len(findings) != 1 {
		t.Fatalf("repeated reversed evidence duplicated shared observation: %d %v", len(findings), err)
	}
}
