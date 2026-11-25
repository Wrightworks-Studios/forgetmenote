package webcore

import (
	"context"
	"fmt"
	"testing"
)

// A request the client abandoned (a superseded search, a closed live poll) is
// not a server failure; logging it as 500 hides real errors.
func TestCanceledRequestsAreNotServerErrors(t *testing.T) {
	for _, err := range []error{context.Canceled, fmt.Errorf("search: %w", context.Canceled)} {
		if got := ErrorOf(err, 500); got.Status != 499 || got.Code != "canceled" {
			t.Fatalf("%v mapped to %+v", err, got)
		}
	}
	if got := ErrorOf(fmt.Errorf("plain failure"), 500); got.Status != 500 {
		t.Fatalf("unclassified error changed: %+v", got)
	}
}
