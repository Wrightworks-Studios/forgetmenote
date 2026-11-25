package connectors

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var parameter = regexp.MustCompile(`\{([A-Za-z][A-Za-z0-9_]*)\}`)
var clusterService = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?\.[a-z0-9](?:[a-z0-9-]*[a-z0-9])?\.svc\.cluster\.local$`)
var httpHost = regexp.MustCompile(`(?i)^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)*$`)

var submittedFrame = regexp.MustCompile(`File "<shell-ro>", line ([1-9][0-9]{0,6})(?:,|\r?\n|$)`)
var executionException = regexp.MustCompile(`(?m)^(TypeError|ValueError|AttributeError|NameError|ImportError|ModuleNotFoundError|SyntaxError|KeyError|IndexError|ZeroDivisionError|RuntimeError|AssertionError):`)

type HTTPRange struct {
	Start      string `json:"start"`
	End        string `json:"end"`
	Format     string `json:"format"`
	MaxSeconds int    `json:"maxSeconds"`
}

func validateHTTPQuery(op *Operation) error {
	if op.ExecutionDiagnostics && (!op.ReadOnly || op.Method != "POST" || !op.Body || op.Response != "") {
		return errors.New("execution diagnostics require a read-only JSON POST operation")
	}
	if op.BodyArgument != "" && (!identifier.MatchString(op.BodyArgument) || op.Body || op.Method == "GET" || op.Method == "HEAD") {
		return errors.New("bodyArgument requires a body-capable method and cannot accompany body")
	}
	if op.Response != "" && (op.Response != "status" && op.Response != "file" || !op.ReadOnly || op.Method != "" && op.Method != "GET") {
		return errors.New("HTTP status response requires a read-only GET")
	}
	for key, arg := range op.QueryLists {
		if key == "" || !identifier.MatchString(arg) || op.Query[key] != "" {
			return errors.New("repeated query parameters require distinct keys and argument names")
		}
	}
	if r := op.Range; r != nil {
		if !identifier.MatchString(r.Start) || !identifier.MatchString(r.End) || r.Start == r.End || r.MaxSeconds < 1 || r.MaxSeconds > 366*86400 || r.Format != "date" && r.Format != "rfc3339" {
			return errors.New("invalid HTTP query range")
		}
	}
	return nil
}

func (r *HTTPRange) validate(input map[string]any) error {
	if r == nil {
		return nil
	}
	layout := time.RFC3339Nano
	if r.Format == "date" {
		layout = "20060102"
	}
	a, _ := input[r.Start].(string)
	b, _ := input[r.End].(string)
	start, e1 := time.Parse(layout, a)
	end, e2 := time.Parse(layout, b)
	duration := end.Sub(start)
	if r.Format == "date" {
		duration += 24 * time.Hour
	}
	if e1 != nil || e2 != nil || end.Before(start) || duration > time.Duration(r.MaxSeconds)*time.Second {
		return errors.New("query time range exceeds the connection policy or is invalid")
	}
	return nil
}

func render(value string, input map[string]any, path bool) (string, error) {
	var err error
	out := parameter.ReplaceAllStringFunc(value, func(s string) string {
		v, ok := input[s[1:len(s)-1]]
		if !ok {
			err = ErrInvalid
			return ""
		}
		var text string
		switch v := v.(type) {
		case string:
			text = v
		case float64, bool:
			text = fmt.Sprint(v)
		default:
			err = ErrInvalid
		}
		if path {
			// Reject pre-encoded routing characters too: an upstream proxy may
			// decode again before applying its own path authorization.
			if text == "." || text == ".." || strings.ContainsAny(text, "%/\\\x00") {
				err = ErrInvalid
			}
			text = url.PathEscape(text)
		}
		return text
	})
	return out, err
}
func (r *runtime) authorize(req *http.Request, secret string) {
	if secret != "" {
		value := secret
		if r.config.AuthScheme != "raw" {
			value = r.config.AuthScheme + " " + secret
		}
		req.Header.Set(r.config.AuthHeader, value)
	}
}
func (r *runtime) httpCall(ctx context.Context, name string, input map[string]any, secret string) (data any, sensitive []string, err error) {
	var op Operation
	for _, candidate := range r.config.Operations {
		if candidate.Name == name {
			op = candidate
		}
	}
	if err := op.Range.validate(input); err != nil {
		return nil, nil, err
	}
	path, err := render(op.Path, input, true)
	if err != nil {
		return nil, sensitive, err
	}
	u, err := url.Parse(strings.TrimRight(r.config.Endpoint, "/") + path)
	if err != nil {
		return nil, sensitive, ErrInvalid
	}
	q := u.Query()
	for key, template := range op.Query {
		value, e := render(template, input, false)
		if e != nil {
			return nil, sensitive, e
		}
		q.Set(key, value)
	}
	for key, arg := range op.QueryLists {
		values, ok := input[arg].([]any)
		if !ok || len(values) == 0 || len(values) > 100 {
			return nil, sensitive, ErrInvalid
		}
		for _, value := range values {
			text, ok := value.(string)
			if !ok {
				raw, e := json.Marshal(value)
				if e != nil {
					return nil, sensitive, ErrInvalid
				}
				text = string(raw)
			}
			q.Add(key, text)
		}
	}
	u.RawQuery = q.Encode()
	var body []byte
	if op.Body {
		body, _ = json.Marshal(input)
	}
	if op.BodyArgument != "" {
		value, ok := input[op.BodyArgument].(map[string]any)
		if !ok || op.Body {
			return nil, sensitive, ErrInvalid
		}
		body, err = json.Marshal(value)
		if err != nil {
			return nil, sensitive, ErrInvalid
		}
	}
	req, err := http.NewRequestWithContext(ctx, op.Method, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, sensitive, ErrInvalid
	}
	// A fixed virtual host changes HTTP routing, not the dial target or TLS identity.
	if r.config.HostHeader != "" {
		req.Host = r.config.HostHeader
	}
	req.Header.Set("Accept", "application/json")
	if op.Body || op.BodyArgument != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	sensitive, err = r.authorizeHTTP(ctx, req, body, secret)
	if err != nil {
		return nil, sensitive, err
	}
	if err := checkAuthority(ctx); err != nil {
		return nil, sensitive, err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, sensitive, ctx.Err()
		}
		return nil, sensitive, ErrUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 && (r.config.Login != nil || r.config.OAuthRefresh != nil || r.config.OAuthClientCredentials != nil || r.config.GitHubApp != nil) {
		r.invalidateLogin(req.Header.Get("Authorization"))
	}
	if (resp.StatusCode < 200 || resp.StatusCode >= 300) && op.Response != "status" {
		if op.ReadOnly && (resp.StatusCode == 429 || resp.StatusCode >= 500) {
			return nil, sensitive, fmt.Errorf("upstream HTTP %d: %w", resp.StatusCode, ErrUnavailable)
		}
		if op.ExecutionDiagnostics && (resp.StatusCode == 400 || resp.StatusCode == 422) {
			if detail := httpExecutionDiagnostic(resp.Body, r.config.MaxBytes); detail != "" {
				return nil, sensitive, fmt.Errorf("upstream HTTP %d: %s", resp.StatusCode, detail)
			}
		}
		return nil, sensitive, fmt.Errorf("upstream HTTP %d", resp.StatusCode)
	}
	raw, err := readBounded(resp.Body, r.config.MaxBytes)
	if errors.Is(err, errTooLarge) {
		return nil, sensitive, errors.New("result exceeds connection limit; narrow the request or request a page")
	}
	if err != nil {
		return nil, sensitive, ErrUnavailable
	}
	if op.Response == "file" {
		for _, secret := range sensitive {
			if secret != "" && bytes.Contains(raw, []byte(secret)) {
				return nil, sensitive, errors.New("file response contains a runtime credential")
			}
		}
		return map[string]any{"data": base64.StdEncoding.EncodeToString(raw), "contentType": resp.Header.Get("Content-Type")}, sensitive, nil
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		data = map[string]any{}
	} else if json.Unmarshal(raw, &data) != nil {
		data = map[string]any{"text": string(raw)}
	}
	if op.Response == "status" {
		data = map[string]any{"httpStatus": resp.StatusCode, "body": data}
	}
	return data, sensitive, nil
}

// A configured code executor may return a traceback with a failed HTTP status.
// Preserve only built-in exception identity and a submitted-code line number;
// messages, values, stdout, filesystem paths and provider diagnostics stay private.
func httpExecutionDiagnostic(body io.Reader, maxBytes int) string {
	limit := min(maxBytes, 16<<10)
	raw, err := readBounded(body, limit)
	if err != nil {
		return ""
	}
	var result struct {
		ExitCode *int   `json:"exitCode"`
		Stderr   string `json:"stderr"`
	}
	if json.Unmarshal(raw, &result) != nil || result.ExitCode == nil || *result.ExitCode == 0 {
		return ""
	}
	frames := submittedFrame.FindAllStringSubmatch(result.Stderr, -1)
	exceptions := executionException.FindAllStringSubmatch(result.Stderr, -1)
	if len(frames) == 0 || len(exceptions) == 0 {
		return ""
	}
	return fmt.Sprintf("%s at submitted line %s; inspect runtime values and correct the code", exceptions[len(exceptions)-1][1], frames[len(frames)-1][1])
}
