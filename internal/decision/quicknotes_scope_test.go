package decision

import (
	"context"
	"net/http"
	"testing"
)

func TestQuicknotesEvaluationPersistsSharedUsage(t *testing.T) {
	service, ledger := fixture(t, func(w http.ResponseWriter, r *http.Request) { goodResponse(w) })
	request := evaluation()
	request.Scope.Feature = "quicknotes"
	if _, err := service.Evaluate(context.Background(), request); err != nil {
		t.Fatalf("quicknotes evaluation rejected: %v", err)
	}
	records, err := ledger.Records()
	if err != nil || len(records) != 1 || records[0].Scope.Feature != "quicknotes" || records[0].Usage == nil {
		t.Fatalf("quicknotes usage absent: %+v %v", records, err)
	}
	ledger.Close()
	reopened, err := OpenLedger(ledger.db)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	records, err = reopened.Records()
	if err != nil || len(records) != 1 || records[0].Scope.Feature != "quicknotes" {
		t.Fatalf("quicknotes usage not recoverable: %+v %v", records, err)
	}
}
