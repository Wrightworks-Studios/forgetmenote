package decision

import (
	"context"
	"net/http"
	"testing"
)

func TestOperatorAliasRecordsResolvedVersion(t *testing.T) {
	s, l := fixture(t, func(w http.ResponseWriter, r *http.Request) { goodResponse(w) })
	defer s.Close()
	s.config.Model = "jev-latest"
	result, err := s.Evaluate(context.Background(), evaluation())
	if err != nil || result.Model != "jev-1.13.0" {
		t.Fatalf("documented operator alias rejected: %+v %v", result, err)
	}
	records, _ := l.Records()
	if len(records) != 1 || records[0].RequestedModel != "jev-latest" || records[0].ReturnedModel != "jev-1.13.0" {
		t.Fatal("lost resolved model identity")
	}
}
