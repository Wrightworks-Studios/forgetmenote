package knowledge

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// A history scan whose request is canceled, or runs out of time, while the
// notes store's history reads wait reports the request's own error and
// leaves no failure behind, so the next scan resumes to complete coverage.
func TestHistoryScanCancellationDoesNotBecomeCatalogueFailure(t *testing.T) {
	for _, resumed := range []bool{false, true} {
		for _, timeout := range []bool{false, true} {
			t.Run(fmt.Sprintf("resumed=%v/deadline=%v", resumed, timeout), func(t *testing.T) {
				f := evidenceNotebook(t)
				ctx := context.Background()
				if resumed {
					// Leave a real persisted scan cursor beyond the first batch.
					for i := 0; i < 17; i++ {
						note, err := f.store.Read(ctx, "public.md", "")
						if err != nil {
							t.Fatal(err)
						}
						if _, err = f.store.Save(ctx, note.Path, []byte(fmt.Sprintf("# Public version %d\n", i)), note.Revision, ""); err != nil {
							t.Fatal(err)
						}
					}
				}
				e := f.engine
				if _, err := e.Configure(ctx, testConfig(), false); err != nil {
					t.Fatal(err)
				}
				if resumed {
					pending, err := e.PendingScope(ctx, 32, "history")
					if err != nil || pending.Status.History.State != "scanning" {
						t.Fatalf("expected unfinished first scan batch: %+v %v", pending, err)
					}
				}
				waiting, release := holdRevisionReads(t, e.db)
				requestCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
				defer cancel()
				result := make(chan error, 1)
				go func() { _, err := e.PendingScope(requestCtx, 32, "history"); result <- err }()
				waiting()
				want := context.DeadlineExceeded
				if !timeout {
					want = context.Canceled
					cancel()
				}
				if err := <-result; !errors.Is(err, want) {
					t.Fatalf("the scan lost its request's error: %v", err)
				}
				release()
				status, err := e.Pending(ctx, 32)
				if err != nil || status.Status.History == nil || status.Status.History.State != "scanning" || status.Status.History.Message != "" {
					t.Fatalf("a canceled scan became a catalogue failure: %+v %v", status.Status.History, err)
				}
				fillIndex(t, e, "history")
				pending, err := e.PendingScope(ctx, 32, "history")
				if err != nil || len(pending.Passages) != 0 || pending.Status.History.State != "ready" || pending.Status.History.Message != "" {
					t.Fatalf("canceled scan did not resume to complete coverage: %+v %v", pending, err)
				}
			})
		}
	}
}
