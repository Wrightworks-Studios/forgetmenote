package decision

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

func TestLedgerRejectsIncompleteSettledUsage(t *testing.T) {
	for _, usage := range []string{`{}`, `null`, `{"input_tokens":null,"output_tokens":3}`, `{"input_tokens":3,"output_tokens":null}`, `{"input_tokens":3}`, `{"output_tokens":3}`} {
		t.Run(usage, func(t *testing.T) {
			s, l := fixture(t, func(w http.ResponseWriter, r *http.Request) { goodResponse(w) })
			if _, err := s.Evaluate(context.Background(), evaluation()); err != nil {
				t.Fatal(err)
			}
			s.Close()
			l.Close()
			if _, err := l.db.Pool.Exec(context.Background(), `UPDATE jev_usage SET record = jsonb_set(record, '{usage}', $2::jsonb) WHERE org = $1`, l.db.Org, usage); err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenLedger(l.db)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if _, err := reopened.Records(); !errors.Is(err, ErrStorage) {
				t.Fatalf("lost settled counters on restore: %s: %v", usage, err)
			}
		})
	}
}
func TestMalformedTypedAnswersRetainIndependentUsage(t *testing.T) {
	for _, answer := range []string{`{"type":"noul","noul":"bad"}`, `{"type":"noul","noul":0.9,"unexpected":"bad"}`} {
		t.Run(answer, func(t *testing.T) {
			s, l := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"model":"jev-1.13.0","answers":{"support":%s},"usage":{"input_tokens":876,"output_tokens":54}}`, answer)
			})
			defer s.Close()
			result, err := s.Evaluate(context.Background(), evaluation())
			if !errors.Is(err, ErrInvalid) || len(result.Answers) > 0 {
				t.Fatalf("malformed answer exposed: %+v %v", result, err)
			}
			records, _ := l.Records()
			if len(records) != 1 || records[0].Usage == nil || records[0].Usage.InputTokens != 876 || records[0].Usage.OutputTokens != 54 || records[0].ReturnedModel != "jev-1.13.0" {
				t.Fatalf("discarded valid paid usage: %+v", records)
			}
		})
	}
}
func TestNullProbabilitiesAreNotNumbers(t *testing.T) {
	for _, kind := range []string{"choice", "score"} {
		t.Run(kind, func(t *testing.T) {
			answer := `{"type":"choice","choice":"yes","confidence":1,"probabilities":{"yes":1,"no":null}}`
			q := Question{Type: "choice", Instructions: "Supported?", Criteria: map[string]string{"yes": "Supported", "no": "Unsupported"}}
			if kind == "score" {
				answer = `{"type":"score","score":0,"confidence":1,"probabilities":{"0":1,"1":null},"legend":{"0":"unsupported","1":"supported"}}`
				q = Question{Type: "score", Instructions: "Supported?", Criteria: []string{"unsupported", "supported"}}
			}
			s, l := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"model":"jev-1.13.0","answers":{"support":%s},"usage":{"input_tokens":777,"output_tokens":33}}`, answer)
			})
			defer s.Close()
			e := evaluation()
			e.Request.Questions["support"] = q
			result, err := s.Evaluate(context.Background(), e)
			if !errors.Is(err, ErrInvalid) || len(result.Answers) > 0 {
				t.Fatalf("null accepted as numeric probability: %+v %v", result, err)
			}
			records, _ := l.Records()
			if len(records) != 1 || records[0].Usage == nil || records[0].Tokens() != 810 {
				t.Fatalf("lost usage for invalid distribution: %+v", records)
			}
		})
	}
}
func TestUsageNeverBecomesAnAdmissionQuota(t *testing.T) {
	var calls atomic.Int32
	s, l := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(500)
			return
		}
		goodResponse(w)
	})
	defer s.Close()
	// Legacy operator quota settings remain readable but cannot limit work.
	raw := `{"requestBytes":16384,"dailyTokens":1,"userDailyTokens":1,"runTokens":1,"runRequests":1}`
	if err := json.Unmarshal([]byte(raw), &s.config.Limits); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		e := evaluation()
		e.Scope.Operation = fmt.Sprintf("independent-%d", i)
		_, err := s.Evaluate(context.Background(), e)
		if i == 0 && !errors.Is(err, ErrUnavailable) || i > 0 && err != nil {
			t.Fatalf("usage prevented operation %d: %v", i, err)
		}
	}
	records, _ := l.Records()
	if len(records) != 3 || records[0].Status != "unknown" || records[0].Tokens() <= 1 || records[1].Tokens() != 338 {
		t.Fatalf("usage quota removal lost accounting: %+v", records)
	}
}
func TestAmbiguousUsageRemainsUnknownWhenTypedAnswersAreInvalid(t *testing.T) {
	for _, usage := range []string{`{"input_tokens":1,"input_tokens":999,"output_tokens":3}`, `{"input_tokens":"bad","output_tokens":3}`, `{"input_tokens":null,"output_tokens":3}`, `{"input_tokens":1}`, `null`} {
		t.Run(strings.ReplaceAll(usage, "\"", ""), func(t *testing.T) {
			s, l := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"model":"jev-1.13.0","answers":{"support":{"type":"noul","noul":"bad"}},"usage":%s}`, usage)
			})
			defer s.Close()
			if _, err := s.Evaluate(context.Background(), evaluation()); !errors.Is(err, ErrInvalid) {
				t.Fatal(err)
			}
			records, _ := l.Records()
			if len(records) != 1 || records[0].Usage != nil || records[0].Status != "unknown" {
				t.Fatalf("ambiguous usage considered known: %+v", records)
			}
		})
	}
}
