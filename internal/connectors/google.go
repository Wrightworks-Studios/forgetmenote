package connectors

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
)

type SheetArea struct {
	SheetID         int64  `json:"sheetID"`
	Range           string `json:"range"`
	Write           bool   `json:"write,omitempty"`
	Append          bool   `json:"append,omitempty"`
	AutomationOwned bool   `json:"automationOwned,omitempty"`
}
type SheetsConfig struct {
	Account     string      `json:"account"`
	Spreadsheet string      `json:"spreadsheet"`
	Areas       []SheetArea `json:"areas"`
}

var googleID = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

func configureGoogle(c *Connection) error {
	if c.Kind != "email" && c.Kind != "sheets" && c.Kind != "calendar" {
		if c.Email != nil || c.Sheets != nil || c.Calendar != nil {
			return ErrInvalid
		}
		return nil
	}
	googleHost := map[string]string{"email": "gmail.googleapis.com", "sheets": "sheets.googleapis.com", "calendar": "www.googleapis.com"}[c.Kind]
	if c.Kind == "email" {
		if c.Email == nil || c.Email.Provider != "gmail" {
			return errors.New("unsupported email provider; supported: gmail")
		}
	}
	if c.Endpoint == "" {
		c.Endpoint = "https://" + googleHost
	}
	u, err := url.Parse(c.Endpoint)
	if err != nil || u.Path != "" && u.Path != "/" || u.Host != googleHost && u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost" {
		return errors.New("Google endpoint must be its Gmail, Sheets or Calendar service host or a loopback fixture")
	}
	if c.OAuthRefresh == nil && c.personalToken == nil {
		return errors.New("Google connections require operator OAuth refresh references")
	}
	if u.Host == googleHost && c.OAuthRefresh != nil && c.OAuthRefresh.TokenURL != "https://oauth2.googleapis.com/token" {
		return errors.New("Google production connections require Google's OAuth token endpoint")
	}
	if c.Kind == "email" {
		if c.Email == nil || c.Sheets != nil || c.Calendar != nil || !identifier.MatchString(c.Email.Workflow) {
			return errors.New("Email requires mailbox and workflow identity")
		}
		copy := *c.Email
		copy.Recipients = slices.Clone(copy.Recipients)
		c.Email = &copy
		c.Email.Mailbox, err = emailAddress(c.Email.Mailbox)
		if err != nil {
			return err
		}
		for i, v := range c.Email.Recipients {
			c.Email.Recipients[i], err = emailAddress(v)
			if err != nil {
				return err
			}
		}
	} else if c.Kind == "calendar" {
		if c.Calendar == nil || c.Email != nil || c.Sheets != nil {
			return errors.New("Calendar requires the account and the calendar it reads")
		}
		copy := *c.Calendar
		c.Calendar = &copy
		c.Calendar.Account, err = emailAddress(c.Calendar.Account)
		if err != nil {
			return err
		}
		if err = checkCalendarID(c.Calendar.Calendar); err != nil {
			return err
		}
	} else {
		if c.Sheets == nil || c.Email != nil || c.Calendar != nil || !googleID.MatchString(c.Sheets.Spreadsheet) || len(c.Sheets.Areas) == 0 {
			return errors.New("Sheets requires account, spreadsheet and allowed areas")
		}
		copy := *c.Sheets
		copy.Areas = slices.Clone(copy.Areas)
		c.Sheets = &copy
		c.Sheets.Account, err = emailAddress(c.Sheets.Account)
		if err != nil {
			return err
		}
		for _, area := range c.Sheets.Areas {
			if _, err := parseSheetRange(area.Range); err != nil {
				return err
			}
			if area.SheetID < 0 || area.Append && (!area.Write || !area.AutomationOwned) {
				return ErrInvalid
			}
		}
	}
	c.Projects = slices.Clone(c.Projects)
	return nil
}
func (r *runtime) googleJSON(ctx context.Context, token, method, path string, query url.Values, body any) (map[string]any, error) {
	var raw []byte
	var err error
	if body != nil {
		raw, err = json.Marshal(body)
		if err != nil {
			return nil, Rejected("invalid Google request")
		}
	}
	base := strings.TrimRight(r.config.Endpoint, "/")
	if base == "https://sheets.googleapis.com" && path == "/oauth2/v3/userinfo" {
		base = "https://www.googleapis.com"
	}
	target := base + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(raw))
	if err != nil {
		return nil, Rejected("invalid Google request")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if err = checkAuthority(ctx); err != nil {
		if method != "GET" {
			err = Rejected(err.Error())
		}
		return nil, err
	}
	if method != "GET" {
		if attempted, ok := ctx.Value(googleAttemptKey{}).(*bool); ok {
			*attempted = true
		}
	}
	response, err := r.client.Do(req)
	write := method != "GET"
	if err != nil {
		if write {
			return nil, Unknown("Google transport failed after possible submission")
		}
		return nil, fmt.Errorf("%w: Google read transport failed", ErrUnavailable)
	}
	defer response.Body.Close()
	raw, err = readBounded(response.Body, r.config.MaxBytes)
	if err != nil {
		if write {
			return nil, Unknown("Google response unavailable or exceeds maxBytes")
		}
		return nil, errors.New("Google response exceeds maxBytes; request a smaller page or range")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		reason := fmt.Sprintf("Google HTTP %d", response.StatusCode)
		switch response.StatusCode {
		case 401:
			r.invalidateLogin("Bearer " + token)
			reason += "; authorization expired or revoked; reauthorize configured account"
		case 403:
			reason += "; insufficient OAuth scopes or resource permission; review enabled operation scopes and reauthorize"
		case 404:
			reason += "; resource unavailable"
		}
		if write {
			if response.StatusCode >= 400 && response.StatusCode < 500 && response.StatusCode != 408 {
				return nil, Rejected(reason)
			}
			return nil, Unknown(reason)
		}
		return nil, &googleReadError{response.StatusCode, reason}
	}
	var result map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&result) != nil || result == nil || decoder.Decode(new(any)) != io.EOF {
		if write {
			return nil, Unknown("Google acknowledgment is malformed")
		}
		return nil, errors.New("Google returned malformed JSON")
	}
	return result, nil
}

type googleReadError struct {
	Status  int
	Message string
}

func (e *googleReadError) Error() string { return e.Message }
func (e *googleReadError) Unwrap() error {
	if e.Status == 429 || e.Status >= 500 {
		return ErrUnavailable
	}
	return nil
}
func googleStatus(err error, code int) bool {
	var e *googleReadError
	return errors.As(err, &e) && e.Status == code
}
func str(v any) string            { s, _ := v.(string); return s }
func object(v any) map[string]any { m, _ := v.(map[string]any); return m }
func array(v any) []any           { a, _ := v.([]any); return a }

type googleAttemptKey struct{}

func (r *runtime) googleCall(ctx context.Context, name string, in map[string]any) (value any, credentials []string, callErr error) {
	write := slices.Contains([]string{"draft-create", "draft-update", "send", "draft-send", "update", "append"}, name)
	attempted := false
	ctx = context.WithValue(ctx, googleAttemptKey{}, &attempted)
	defer func() {
		if write && callErr != nil && !attempted && !WriteRejected(callErr) {
			callErr = Rejected(callErr.Error())
		}
	}()
	token, secrets, err := r.refreshedToken(ctx)
	if err != nil {
		if write {
			err = Rejected(err.Error())
		}
		return nil, secrets, err
	}
	account, profile, err := r.googleIdentity(ctx, token)
	if err != nil {
		if write {
			err = Rejected(err.Error())
		}
		return nil, secrets, err
	}
	if name == "status" {
		ops := []string{}
		for _, op := range r.config.Operations {
			ops = append(ops, op.Name)
		}
		r.loginMu.Lock()
		scopes := strings.Fields(r.loginScopes)
		r.loginMu.Unlock()
		scopeStatus := "reported"
		if len(scopes) == 0 {
			scopeStatus = "not reported; Google validates scopes on each operation"
		}
		status := map[string]any{"account": account, "configuredOperations": ops, "oauthScopes": scopes, "scopeStatus": scopeStatus, "complete": true, "retrievedAt": time.Now().UTC()}
		if r.config.Kind == "email" {
			cursor := str(profile["historyId"])
			if !googleID.MatchString(cursor) {
				return nil, secrets, errors.New("email provider omitted change cursor")
			}
			status["provider"], status["cursor"], status["capabilities"] = r.config.Email.Provider, cursor, gmailCapabilities()
		} else {
			status["profile"] = profile
		}
		if r.config.Kind == "calendar" {
			status["calendar"] = r.config.Calendar.Calendar
		}
		return status, secrets, nil
	}
	var result any
	switch r.config.Kind {
	case "email":
		result, err = r.gmailCall(ctx, token, name, in, account)
	case "sheets":
		result, err = r.sheetsCall(ctx, token, name, in, account)
	default:
		result, err = r.calendarCall(ctx, token, name, in)
	}
	return result, secrets, err
}
func (r *runtime) googleIdentity(ctx context.Context, token string) (string, map[string]any, error) {
	path := "/gmail/v1/users/me/profile"
	expected := r.config.Email
	if r.config.Kind != "email" {
		path = "/oauth2/v3/userinfo"
	}
	profile, err := r.googleJSON(ctx, token, "GET", path, nil, nil)
	if err != nil {
		return "", nil, err
	}
	account := str(profile["emailAddress"])
	want := ""
	if expected != nil {
		want = expected.Mailbox
	} else {
		account = str(profile["email"])
		if r.config.Kind == "calendar" {
			want = r.config.Calendar.Account
		} else {
			want = r.config.Sheets.Account
		}
		if profile["email_verified"] != true {
			return "", nil, errors.New("Google account email is not verified; grant openid email scopes")
		}
	}
	if !strings.EqualFold(account, want) {
		return "", nil, errors.New("Google authorized account mismatch; reauthorize the configured account")
	}
	return strings.ToLower(account), profile, nil
}

func timeNow() time.Time { return time.Now().UTC() }
