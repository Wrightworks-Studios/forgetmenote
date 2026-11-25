package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type Session struct {
	Server   string `json:"server"`
	Token    string `json:"token"`
	Refresh  string `json:"refresh,omitempty"`
	ClientID string `json:"clientId,omitempty"`
	Expires  int64  `json:"expires,omitempty"`
}
type configuration struct {
	Active   string             `json:"active"`
	Sessions map[string]Session `json:"sessions"`
}

func configDirectory() (string, error) {
	if p := os.Getenv("FORGETMENOTE_CONFIG_DIR"); p != "" {
		return filepath.Abs(p)
	}
	p, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(p, "forgetmenote"), nil
}
func withConfig(dir string, fn func(*configuration) error) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	st, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 || st.Mode().Perm()&0077 != 0 {
		return errors.New("client configuration directory must be private (0700) and not a symlink")
	}
	lock, err := os.OpenFile(filepath.Join(dir, "config.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	cfg := configuration{Sessions: map[string]Session{}}
	path := filepath.Join(dir, "config.json")
	raw, err := regularFile(path)
	if err == nil {
		if json.Unmarshal(raw, &cfg) != nil || cfg.Sessions == nil {
			return errors.New("invalid client configuration")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	before, _ := json.Marshal(cfg)
	if err = fn(&cfg); err != nil {
		return err
	}
	after, _ := json.MarshalIndent(cfg, "", "  ")
	compact, _ := json.Marshal(cfg)
	if string(before) == string(compact) {
		return nil
	}
	return privateWrite(path, append(after, '\n'))
}
func saveSession(dir string, session Session) error {
	return withConfig(dir, func(c *configuration) error {
		c.Active = session.Server
		c.Sessions[session.Server] = session
		return nil
	})
}
func configuredClient(ctx context.Context, dir, server string) (*Client, error) {
	if server == "" {
		server = os.Getenv("FORGETMENOTE_SERVER")
	}
	if server != "" {
		var err error
		server, err = ValidateServer(server)
		if err != nil {
			return nil, err
		}
	}
	if token := os.Getenv("FORGETMENOTE_TOKEN"); token != "" && server != "" {
		return &Client{Server: server, Token: strings.TrimSpace(token)}, nil
	}
	var session Session
	err := withConfig(dir, func(cfg *configuration) error {
		if server == "" {
			server = cfg.Active
		}
		var ok bool
		session, ok = cfg.Sessions[server]
		if !ok {
			return errors.New("sign in with forgetmenote auth login --server URL")
		}
		origin, err := ValidateServer(server)
		if err != nil || origin != server || session.Server != server {
			return errors.New("invalid server in client configuration")
		}
		if session.Refresh != "" && session.Expires <= time.Now().Add(time.Minute).Unix() {
			var result tokenResponse
			err := postForm(ctx, server+"/oauth/token", url.Values{"grant_type": {"refresh_token"}, "client_id": {session.ClientID}, "refresh_token": {session.Refresh}, "resource": {server + "/api/v1"}}, &result)
			if err != nil {
				return fmt.Errorf("session refresh failed; sign in again: %w", err)
			}
			if result.AccessToken == "" || result.RefreshToken == "" {
				return errors.New("invalid refreshed session")
			}
			session.Token, session.Refresh, session.Expires = result.AccessToken, result.RefreshToken, time.Now().Unix()+result.ExpiresIn
			cfg.Sessions[server] = session
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &Client{Server: session.Server, Token: session.Token}, nil
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
}

func postForm(ctx context.Context, endpoint string, values url.Values, out any) error {
	r, err := http.NewRequestWithContext(ctx, "POST", endpoint, strings.NewReader(values.Encode()))
	if err != nil {
		return err
	}
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h := http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := h.Do(r)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return &Error{Status: response.StatusCode, Code: "authentication", Message: http.StatusText(response.StatusCode)}
	}
	if out == nil {
		_, err = io.Copy(io.Discard, response.Body)
		return err
	}
	return json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(out)
}
