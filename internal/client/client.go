// Package client owns the HTTP boundary used by the command-line client.
package client

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
	"strings"
	"time"
)

type Client struct {
	Server, Token string
	HTTP          *http.Client
}
type Error struct {
	Status        int
	Code, Message string
}

func (e *Error) Error() string {
	if e.Status == http.StatusForbidden {
		return e.Code + ": " + e.Message + " (actions that manage notebooks or access need a session from `forgetmenote auth login --manage`)"
	}
	return e.Code + ": " + e.Message
}
func (c *Client) Call(ctx context.Context, name string, in, out any) error {
	if !regexp.MustCompile(`^[a-z][a-z0-9_]*$`).MatchString(name) {
		return errors.New("invalid action name")
	}
	return c.request(ctx, "POST", "/api/v1/actions/"+name, in, out)
}

func ValidateServer(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSuffix(raw, "/"))
	if err != nil || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1")) {
		return "", errors.New("server must be an HTTPS origin (HTTP is allowed for localhost)")
	}
	return u.String(), nil
}
func (c *Client) Get(ctx context.Context, path string, out any) error {
	return c.request(ctx, "GET", path, nil, out)
}
func (c *Client) request(ctx context.Context, method, path string, in, out any) error {
	server, err := ValidateServer(c.Server)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(path, "/api/v1/") {
		return errors.New("invalid API path")
	}
	var body io.Reader
	if in != nil {
		raw, e := json.Marshal(in)
		if e != nil {
			return e
		}
		body = bytes.NewReader(raw)
	}
	r, err := http.NewRequestWithContext(ctx, method, server+path, body)
	if err != nil {
		return err
	}
	r.Header.Set("Authorization", "Bearer "+c.Token)
	r.Header.Set("Content-Type", "application/json")
	h := http.Client{Timeout: 2 * time.Minute}
	if c.HTTP != nil {
		h = *c.HTTP
	}
	h.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := h.Do(r)
	if err != nil {
		return fmt.Errorf("server request failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var value struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.NewDecoder(io.LimitReader(response.Body, 65536)).Decode(&value) != nil || value.Error.Code == "" {
			return &Error{Status: response.StatusCode, Code: "http_error", Message: http.StatusText(response.StatusCode)}
		}
		return &Error{Status: response.StatusCode, Code: value.Error.Code, Message: value.Error.Message}
	}
	if out == nil {
		_, err = io.Copy(io.Discard, response.Body)
		return err
	}
	d := json.NewDecoder(response.Body)
	if err = d.Decode(out); err != nil {
		return fmt.Errorf("incomplete server response: %w", err)
	}
	if err = d.Decode(new(any)); err != io.EOF {
		return errors.New("unexpected trailing server response")
	}
	return nil
}
