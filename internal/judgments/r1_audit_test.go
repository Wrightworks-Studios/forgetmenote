package judgments

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
)

func TestJudgmentR1AuditPreparationRejectsBeforeCartesianAllocation(t *testing.T) {
	app := reviewFixture(t, syntheticReviewAnswer)
	store, _ := app.catalog.Access("alice", "default", "edit")
	var source, destination strings.Builder
	source.WriteString("---\nfmn-container: archive\n---\n")
	for i := 0; i < 500; i++ {
		fmt.Fprintf(&source, "# Source %d\nPressure criterion %d.\n\n", i, i)
		fmt.Fprintf(&destination, "# Destination %d\nPressure criterion %d.\n\n", i, i)
	}
	src, err := store.Save(context.Background(), "source.md", []byte(source.String()), "", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Save(context.Background(), "destination.md", []byte(destination.String()), "", "alice"); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(JobInput{Kind: "audit", OperationID: "large", Sources: []judgmentSourceSelection{{Path: src.Path, Revision: src.Revision}}, Destinations: []string{"destination.md"}})
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	w := proxyCall(app, "alice", "POST", "/p/default/api/judgments/jobs", string(body))
	runtime.ReadMemStats(&after)
	if w.Code == 200 {
		t.Fatal("oversized audit was admitted")
	}
	t.Logf("bounded audit preparation allocated %d bytes", after.TotalAlloc-before.TotalAlloc)
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 64<<20 {
		t.Fatalf("audit expanded before storage preflight: allocated %d bytes", allocated)
	}
	if !strings.Contains(w.Body.String(), "smaller source or destination selection") {
		t.Fatalf("oversized audit did not explain storage boundary: %d %s", w.Code, w.Body.String())
	}
	records, _ := usageLedger(t, app).Records()
	if len(records) != 0 {
		t.Fatal("preflight spent provider usage")
	}
}

func TestJudgmentR1AuditDuplicateSelectionsDoNotMultiplyCoverage(t *testing.T) {
	app := reviewFixture(t, syntheticReviewAnswer)
	store, _ := app.catalog.Access("alice", "default", "edit")
	src, err := store.Read(context.Background(), "a.md", "")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(JobInput{Kind: "audit", OperationID: "duplicates", Sources: []judgmentSourceSelection{{Path: src.Path, Revision: src.Revision}, {Path: src.Path}, {Path: src.Path, Revision: src.Revision}}, Destinations: []string{"b.md", "b.md"}})
	w := proxyCall(app, "alice", "POST", "/p/default/api/judgments/jobs", string(body))
	var job map[string]any
	json.Unmarshal(w.Body.Bytes(), &job)
	if w.Code != 200 || job["total"] != float64(1) {
		t.Fatal("repeated selection duplicated work", w.Code, w.Body.String())
	}
}

func TestJudgmentR1CancelledAuditPreparationDoesNotCreateCheckpoint(t *testing.T) {
	app := reviewFixture(t, syntheticReviewAnswer)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request := httptest.NewRequest("POST", "http://localhost/p/default/api/judgments/jobs", strings.NewReader(`{"kind":"audit","operationId":"cancel-prepare","sources":[{"path":"a.md"}],"destinations":["b.md"]}`)).WithContext(ctx)
	request.Header.Set(testUserHeader, "alice")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	instanceOf(t, app).mux.ServeHTTP(response, request)
	if response.Code == 200 {
		t.Fatal("cancelled preparation created durable work", response.Body.String())
	}
	app.judgmentState.mu.Lock()
	ids, err := app.judgmentState.jobIDs()
	app.judgmentState.mu.Unlock()
	if err != nil || len(ids) != 0 {
		t.Fatal(ids, err)
	}
	records, err := usageLedger(t, app).Records()
	if err != nil || len(records) != 0 {
		t.Fatal(records, err)
	}
}
