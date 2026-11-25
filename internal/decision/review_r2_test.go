package decision

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestTypedAliasesCannotOverrideAnswersOrAccounting(t *testing.T) {
	cases := []struct {
		name, answer, usage string
		known               bool
	}{
		{"type alias", `{"type":"choice","TYPE":"noul","noul":0.9}`, `{"input_tokens":876,"output_tokens":54}`, true},
		{"null overridden", `{"type":"noul","noul":null,"NOUL":0.9}`, `{"input_tokens":876,"output_tokens":54}`, true},
		{"usage alias", `{"type":"noul","noul":0.9}`, `{"input_tokens":0,"INPUT_TOKENS":876,"output_tokens":54}`, false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			s, l := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"model":"jev-1.13.0","answers":{"support":%s},"usage":%s}`, tt.answer, tt.usage)
			})
			defer s.Close()
			result, err := s.Evaluate(context.Background(), evaluation())
			if !errors.Is(err, ErrInvalid) || len(result.Answers) != 0 {
				t.Fatalf("aliased typed fields escaped: %+v %v", result, err)
			}
			records, _ := l.Records()
			if len(records) != 1 || (records[0].Usage != nil) != tt.known {
				t.Fatalf("accounting ambiguity mishandled: %+v", records)
			}
		})
	}
}
func TestLedgerRejectsCaseAliasesInTypedFields(t *testing.T) {
	for _, field := range []string{"actor", "attempt", "input_tokens"} {
		t.Run(field, func(t *testing.T) {
			s, l := fixture(t, func(w http.ResponseWriter, r *http.Request) { goodResponse(w) })
			if _, err := s.Evaluate(context.Background(), evaluation()); err != nil {
				t.Fatal(err)
			}
			s.Close()
			l.Close()
			if _, err := l.db.Pool.Exec(context.Background(), `UPDATE jev_usage SET record = replace(record::text, $2, $3)::jsonb WHERE org = $1`, l.db.Org, `"`+field+`"`, `"`+strings.ToUpper(field)+`"`); err != nil {
				t.Fatal(err)
			}
			restored, err := OpenLedger(l.db)
			if err != nil {
				t.Fatal(err)
			}
			defer restored.Close()
			if _, err := restored.Records(); !errors.Is(err, ErrStorage) {
				t.Fatalf("aliased persisted field accepted: %s %v", field, err)
			}
		})
	}
}
func TestIndependentAccountingSurvivesHTTPFailuresAndAnswerDuplicates(t *testing.T) {
	for _, kind := range []string{"http error", "duplicate answer"} {
		t.Run(kind, func(t *testing.T) {
			s, l := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				if kind == "http error" {
					w.WriteHeader(500)
					w.Write([]byte(`{"model":"jev-1.13.0","error":"private-marker","usage":{"input_tokens":876,"output_tokens":54}}`))
					return
				}
				w.Write([]byte(`{"model":"jev-1.13.0","answers":{"support":{"type":"noul","noul":0.9},"support":{"type":"noul","noul":0.1}},"usage":{"input_tokens":876,"output_tokens":54}}`))
			})
			defer s.Close()
			result, err := s.Evaluate(context.Background(), evaluation())
			expected := ErrInvalid
			if kind == "http error" {
				expected = ErrUnavailable
			}
			if !errors.Is(err, expected) || len(result.Answers) > 0 || strings.Contains(err.Error(), "private-marker") {
				t.Fatalf("unsafe provider failure: %+v %v", result, err)
			}
			records, _ := l.Records()
			if len(records) != 1 || records[0].Usage == nil || records[0].Tokens() != 930 {
				t.Fatalf("lost independent billed usage: %+v", records)
			}
		})
	}
}
func TestCancellationDuringFinalCheckReturnsNoAnswers(t *testing.T) {
	s, l := fixture(t, func(w http.ResponseWriter, r *http.Request) { goodResponse(w) })
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e := evaluation()
	checks := 0
	e.Check = func(context.Context) error {
		checks++
		if checks == 2 {
			cancel()
		}
		return nil
	}
	result, err := s.Evaluate(ctx, e)
	if !errors.Is(err, context.Canceled) || len(result.Answers) > 0 {
		t.Fatalf("cancelled result escaped final check: %+v %v", result, err)
	}
	records, _ := l.Records()
	if len(records) != 1 || records[0].Tokens() != 338 || records[0].Usage == nil {
		t.Fatal("cancelled handoff lost paid usage")
	}
}
func TestRoundedDistributionSupportsAllDeclaredChoiceSizes(t *testing.T) {
	for _, probability := range []float64{.06, .09} {
		t.Run(fmt.Sprint(probability), func(t *testing.T) {
			criteria := map[string]string{}
			probabilities := map[string]float64{}
			for i := 0; i < 18; i++ {
				key := fmt.Sprintf("option-%d", i)
				criteria[key] = key
				probabilities[key] = probability
			}
			body, _ := json.Marshal(map[string]any{"model": "jev-1.13.0", "answers": map[string]any{"support": map[string]any{"type": "choice", "choice": "option-0", "confidence": 0, "probabilities": probabilities}}, "usage": map[string]int{"input_tokens": 876, "output_tokens": 54}})
			s, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) { w.Write(body) })
			defer s.Close()
			e := evaluation()
			e.Request.Questions["support"] = Question{Type: "choice", Instructions: "Which option?", Criteria: criteria}
			_, err := s.Evaluate(context.Background(), e)
			if (err == nil) != (probability == .06) {
				t.Fatalf("rounded distribution %v: %v", probability, err)
			}
		})
	}
}

func TestAccountingAmbiguityAndResponseLimitsRemainUnknown(t *testing.T) {
	for _, kind := range []string{"duplicate usage", "counter alias", "truncated", "oversize"} {
		t.Run(kind, func(t *testing.T) {
			body := `{"model":"jev-1.13.0","usage":{"input_tokens":876,"output_tokens":54},"usage":{"input_tokens":1,"output_tokens":2}}`
			switch kind {
			case "counter alias":
				body = `{"model":"jev-1.13.0","usage":{"INPUT_TOKENS":876,"output_tokens":54}}`
			case "truncated":
				body = `{"model":"jev-1.13.0","usage":{"input_tokens":876,"output_tokens":54}`
			case "oversize":
				body = `{"model":"jev-1.13.0","usage":{"input_tokens":876,"output_tokens":54},"error":"` + strings.Repeat("x", 70000) + `"}`
			}
			s, l := fixture(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500); w.Write([]byte(body)) })
			defer s.Close()
			if _, err := s.Evaluate(context.Background(), evaluation()); !errors.Is(err, ErrUnavailable) {
				t.Fatal(err)
			}
			records, _ := l.Records()
			if len(records) != 1 || records[0].Usage != nil || records[0].Status != "unknown" {
				t.Fatalf("ambiguous usage became known: %+v", records)
			}
		})
	}
}
func TestUserDefinedQuestionAndOptionKeysRemainCaseSensitive(t *testing.T) {
	s, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"model":"jev-1.13.0","answers":{"support":{"type":"noul","noul":0.9},"SUPPORT":{"type":"choice","choice":"yes","confidence":0.8,"probabilities":{"yes":0.9,"YES":0.1}}},"usage":{"input_tokens":60,"output_tokens":20}}`))
	})
	defer s.Close()
	e := evaluation()
	e.Request.Questions["SUPPORT"] = Question{Type: "choice", Instructions: "Which?", Criteria: map[string]string{"yes": "first", "YES": "second"}}
	result, err := s.Evaluate(context.Background(), e)
	if err != nil || len(result.Answers) != 2 || result.Answers["SUPPORT"].Probabilities["YES"] != .1 {
		t.Fatalf("case-sensitive user keys collapsed: %+v %v", result, err)
	}
}
