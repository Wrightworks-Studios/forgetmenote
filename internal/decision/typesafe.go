package decision

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	"forgetmenote/internal/durable"
)

type typesafeClient struct {
	config *Config
	http   *http.Client
	secret string
}

func newTypeSafeClient(c *Config) (*typesafeClient, error) {
	secret, err := durable.ReadSecret(c.SecretFile, 4096)
	if err != nil || !identity(secret, 4096) {
		return nil, errorsSecret()
	}
	return &typesafeClient{config: c, secret: secret, http: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
func (c *typesafeClient) close() { c.http.CloseIdleConnections() }
func (c *typesafeClient) prepare(request Request) ([]byte, int64, error) {
	return prepare(*c.config, request)
}
func errorsSecret() error { return errors.New("TypeSafe credential is unavailable or invalid") }

func jsonValue(v any, nullable bool) bool {
	if v == nil {
		return nullable
	}
	switch x := v.(type) {
	case string:
		return x != ""
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return false
}
func prepare(c Config, r Request) ([]byte, int64, error) {
	questions := make(map[string]Question, len(r.Questions))
	for id, question := range r.Questions {
		switch question.Type {
		case Boolean:
			question.Type = "noul"
		case Choice, Score:
		default:
			return nil, 0, ErrInvalid
		}
		questions[id] = question
	}
	raw, err := json.Marshal(struct {
		Model     string              `json:"model"`
		State     any                 `json:"state"`
		Questions map[string]Question `json:"questions"`
	}{c.Model, r.State, questions})
	if err != nil {
		return nil, 0, ErrInvalid
	}
	if len(raw) > c.Limits.RequestBytes {
		return nil, 0, ErrLimit
	}
	var normalized struct {
		State     any                 `json:"state"`
		Questions map[string]Question `json:"questions"`
	}
	if json.Unmarshal(raw, &normalized) != nil || !jsonValue(normalized.State, false) || len(normalized.Questions) == 0 || len(normalized.Questions) > c.Limits.Questions {
		return nil, 0, ErrInvalid
	}
	state, _ := json.Marshal(normalized.State)
	output := int64(512)
	for id, q := range normalized.Questions {
		// Jev has separate combined-request and state-plus-one-question context
		// limits. Byte bounds are conservative; they are not token accounting.
		question, _ := json.Marshal(q)
		if len(state)+len(question) > 32768 {
			return nil, 0, ErrLimit
		}
		if !identity(id, 80) || !jsonValue(q.Instructions, false) {
			return nil, 0, ErrInvalid
		}
		switch q.Type {
		case "noul":
			if q.Criteria != nil {
				m, ok := q.Criteria.(map[string]any)
				if !ok || len(m) == 0 || len(m) > 2 {
					return nil, 0, ErrInvalid
				}
				for k, v := range m {
					if (k != "true" && k != "false") || !jsonValue(v, false) {
						return nil, 0, ErrInvalid
					}
				}
			}
			output += 64
		case "choice":
			m, ok := q.Criteria.(map[string]any)
			if !ok || len(m) < 2 || len(m) > 255 {
				return nil, 0, ErrInvalid
			}
			for k, v := range m {
				if !identity(k, 80) || !jsonValue(v, true) {
					return nil, 0, ErrInvalid
				}
			}
			output += int64(64 + 128*len(m))
		case "score":
			a, ok := q.Criteria.([]any)
			if !ok || len(a) < 2 || len(a) > 10 {
				return nil, 0, ErrInvalid
			}
			for _, v := range a {
				if !jsonValue(v, false) {
					return nil, 0, ErrInvalid
				}
			}
			output += int64(64 + 128*len(a))
		default:
			return nil, 0, ErrInvalid
		}
	}
	// The API has no max_tokens parameter. Byte-based input plus question overhead
	// and bounded typed output is a deliberately conservative estimate, not a bill.
	return raw, int64(len(raw)) + int64(len(r.Questions))*512 + output, nil
}
func (c *typesafeClient) call(ctx context.Context, raw []byte, r Request) (Result, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.config.Endpoint, bytes.NewReader(raw))
	if err != nil {
		return Result{}, false, ErrInvalid
	}
	req.Header.Set("Authorization", "Bearer "+c.secret)
	req.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return Result{}, false, ctx.Err()
		}
		return Result{}, false, ErrUnavailable
	}
	defer response.Body.Close()
	retry := response.StatusCode == 429 || response.StatusCode == 529
	failure := ErrInvalid
	if response.StatusCode != http.StatusOK {
		failure = ErrUnavailable
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, int64(c.config.Limits.ResponseBytes)+1))
	if err != nil || len(body) > c.config.Limits.ResponseBytes {
		if ctx.Err() != nil {
			return Result{}, false, ctx.Err()
		}
		return Result{}, retry, failure
	}
	accounted := responseAccounting(body)
	if response.StatusCode != http.StatusOK {
		return accounted, retry, &providerStatus{code: response.StatusCode, err: ErrUnavailable}
	}

	var wire struct {
		Model   string                  `json:"model"`
		Answers map[string]answerRecord `json:"answers"`
		Usage   *Usage                  `json:"usage"`
	}
	if !strictJSON(body, &wire) {
		return accounted, false, ErrInvalid
	}
	result := Result{Model: wire.Model, Answers: make(map[string]Answer, len(wire.Answers)), Usage: accounted.Usage}
	for id, answer := range wire.Answers {
		if answer.Type != "noul" && answer.Type != Choice && answer.Type != Score {
			return accounted, false, ErrInvalid
		}
		result.Answers[id] = answer.normalized()
	}

	if result.Usage == nil || !matchesModel(c.config.Model, result.Model) || validateAnswers(r, result.Answers) != nil {
		return result, false, ErrInvalid
	}
	return result, false, nil
}
func probability(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v <= 1 }
func validateAnswers(r Request, answers map[string]Answer) error {
	if len(r.Questions) != len(answers) {
		return ErrInvalid
	}
	// Normalize JSON-shaped criteria for callers using []string or typed maps.
	raw, _ := json.Marshal(r)
	var normalized Request
	if json.Unmarshal(raw, &normalized) != nil {
		return ErrInvalid
	}
	for id, q := range normalized.Questions {
		a, ok := answers[id]
		if !ok || a.Type != q.Type {
			return ErrInvalid
		}
		if q.Type == Boolean {
			if a.Probability == nil || !probability(*a.Probability) || a.Score != nil || a.Choice != "" || a.Confidence != nil || len(a.Probabilities) > 0 || len(a.Legend) > 0 {
				return ErrInvalid
			}
			continue
		}
		if a.Probability != nil || a.Confidence == nil || !probability(*a.Confidence) {
			return ErrInvalid
		}
		expected := map[string]bool{}
		if q.Type == "choice" {
			for k := range q.Criteria.(map[string]any) {
				expected[k] = true
			}
			if !expected[a.Choice] || a.Score != nil || len(a.Legend) > 0 {
				return ErrInvalid
			}
		} else {
			levels := q.Criteria.([]any)
			for i := range levels {
				expected[strconv.Itoa(i)] = true
			}
			if a.Choice != "" || a.Score == nil || math.IsNaN(*a.Score) || *a.Score < 0 || *a.Score > float64(len(levels)-1) || len(a.Legend) != len(expected) {
				return ErrInvalid
			}
			for k, v := range a.Legend {
				var description any
				index, err := strconv.Atoi(k)
				if !expected[k] || err != nil || json.Unmarshal(v, &description) != nil || !reflect.DeepEqual(description, levels[index]) {
					return ErrInvalid
				}
			}
		}
		if len(a.Probabilities) != len(expected) {
			return ErrInvalid
		}
		lower, upper := 0.0, 0.0
		for k, p := range a.Probabilities {
			if !expected[k] || !probability(p) {
				return ErrInvalid
			}
			lower += math.Max(0, p-0.005)
			upper += math.Min(1, p+0.005)
			if q.Type == "choice" && p > a.Probabilities[a.Choice]+0.011 {
				return ErrInvalid
			}
		}
		// Provider distributions are rounded to two decimal places on the wire.
		if lower > 1.00001 || upper < 0.99999 {
			return ErrInvalid
		}
	}
	return nil
}

var versionedModel = regexp.MustCompile(`^jev-[0-9]+\.[0-9]+\.[0-9]+$`)

func matchesModel(requested, returned string) bool {
	if requested == "jev-latest" || requested == "jev-preview" {
		return versionedModel.MatchString(returned)
	}
	return requested == returned
}

// Accounting is independent of HTTP status and malformed judgments. Decode the
// top-level object without resolving duplicates inside unrelated answer data.
func responseAccounting(raw []byte) Result {
	if !json.Valid(raw) {
		return Result{}
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return Result{}
	}
	values := map[string]json.RawMessage{}
	ambiguous := map[string]bool{}
	for d.More() {
		token, err = d.Token()
		if err != nil {
			return Result{}
		}
		key := token.(string)
		var value json.RawMessage
		if d.Decode(&value) != nil {
			return Result{}
		}
		canonical := strings.ToLower(key)
		if canonical != "model" && canonical != "usage" {
			continue
		}
		if _, exists := values[canonical]; exists || key != canonical {
			ambiguous[canonical] = true
		}
		values[canonical] = value
	}
	result := Result{}
	if !ambiguous["model"] && json.Unmarshal(values["model"], &result.Model) == nil && !identity(result.Model, 128) {
		result.Model = ""
	}
	var usage Usage
	if !ambiguous["usage"] && json.Unmarshal(values["usage"], &usage) == nil {
		result.Usage = &usage
	}
	return result
}

func validateTypeSafe(c *Config) error {
	if c.Endpoint == "" {
		c.Endpoint = "https://api.typesafe.ai/v1/systemone"
	}
	u, err := url.Parse(c.Endpoint)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Path != "/v1/systemone" || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.ContainsAny(c.Endpoint, "\r\n\t") {
		return errors.New("invalid TypeSafe HTTPS endpoint")
	}
	if c.SecretFile == "" {
		c.SecretFile = "/run/secrets/typesafe-api-key"
	}
	return nil
}
