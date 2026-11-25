package connectors

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
)

type HTTPSigning struct {
	Region  string `json:"region"`
	Service string `json:"service"`
}

// HTTPLogin owns a renewable bearer session at the connection's fixed origin.
// Credentials and fixed login fields cannot be supplied by a tool caller.
type HTTPLogin struct {
	Path         string         `json:"path"`
	UsernameFile string         `json:"usernameFile,omitempty"`
	UsernameEnv  string         `json:"usernameEnv,omitempty"`
	Fields       map[string]any `json:"fields,omitempty"`
	TokenField   string         `json:"tokenField"`
	TTLSeconds   int            `json:"ttlSeconds,omitempty"`
}

func credentialReference(file, env string) bool {
	return file != "" && filepath.IsAbs(file) && env == "" || file == "" && regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`).MatchString(env)
}

func validateHTTPAuth(c *Connection) error {
	if c.GitHubApp != nil {
		if c.Kind != "http" || c.OAuthClientCredentials != nil || c.OAuthRefresh != nil || c.AWS != nil || c.Login != nil || c.SecretFile != "" || c.SecretEnv != "" || c.UsernameFile != "" || c.UsernameEnv != "" || c.AuthHeader != "Authorization" || c.AuthScheme != "Bearer" {
			return errors.New("GitHub App authentication cannot be combined with another authentication mode")
		}
		g := *c.GitHubApp
		g.Repositories = slices.Clone(g.Repositories)
		g.Permissions = maps.Clone(g.Permissions)
		if err := g.validate(); err != nil {
			return err
		}
		c.GitHubApp = &g
	}
	if c.OAuthClientCredentials != nil {
		if c.Kind != "http" || c.OAuthRefresh != nil || c.AWS != nil || c.Login != nil || c.SecretFile != "" || c.SecretEnv != "" || c.UsernameFile != "" || c.UsernameEnv != "" || c.AuthHeader != "Authorization" || c.AuthScheme != "Bearer" {
			return errors.New("OAuth client credentials cannot be combined with another authentication mode")
		}
		if err := c.OAuthClientCredentials.validate(); err != nil {
			return err
		}
		copy := *c.OAuthClientCredentials
		c.OAuthClientCredentials = &copy
	}

	if c.OAuthRefresh != nil {
		if c.Kind != "http" && c.Kind != "email" && c.Kind != "sheets" && c.Kind != "calendar" || c.AWS != nil || c.Login != nil || c.SecretFile != "" || c.SecretEnv != "" || c.UsernameFile != "" || c.UsernameEnv != "" || c.AuthHeader != "Authorization" || c.AuthScheme != "Bearer" {
			return errors.New("OAuth refresh cannot be combined with another authentication mode")
		}
		if err := c.OAuthRefresh.validate(); err != nil {
			return err
		}
		copy := *c.OAuthRefresh
		c.OAuthRefresh = &copy
	}
	user := c.UsernameFile != "" || c.UsernameEnv != ""
	if c.Kind != "http" && (user || c.AWS != nil || c.Login != nil) {
		return errors.New("HTTP authentication requires an HTTP connection")
	}
	if user && (c.AuthScheme != "Basic" || !credentialReference(c.UsernameFile, c.UsernameEnv) || !credentialReference(c.SecretFile, c.SecretEnv) || c.AWS != nil || c.Login != nil) {
		return errors.New("Basic authentication requires separate username and password references")
	}
	if c.AWS != nil {
		a := *c.AWS
		if !regexp.MustCompile(`^[a-z0-9-]+$`).MatchString(a.Region) || !regexp.MustCompile(`^[a-z0-9-]+$`).MatchString(a.Service) || c.SecretFile != "" || c.SecretEnv != "" || c.Login != nil || c.AuthHeader != "Authorization" || c.AuthScheme != "Bearer" {
			return errors.New("AWS signing requires region, service and runtime AWS credentials only")
		}
		c.AWS = &a
	}
	if c.Login != nil {
		l := *c.Login
		l.Fields = maps.Clone(l.Fields)
		if l.TTLSeconds == 0 {
			l.TTLSeconds = 300
		}
		if !strings.HasPrefix(l.Path, "/") || strings.HasPrefix(l.Path, "//") || strings.ContainsAny(l.Path, "?#\\{}%") || strings.Contains(l.Path, "..") || !credentialReference(l.UsernameFile, l.UsernameEnv) || !credentialReference(c.SecretFile, c.SecretEnv) || l.TokenField == "" || !identifier.MatchString(l.TokenField) || l.TTLSeconds < 1 || l.TTLSeconds > 3600 || c.AuthHeader != "Authorization" || c.AuthScheme != "Bearer" {
			return errors.New("invalid HTTP login policy")
		}
		for k, v := range l.Fields {
			if k == "username" || k == "password" {
				return errors.New("login credentials must use runtime references")
			}
			switch v.(type) {
			case string, bool, float64:
			default:
				return errors.New("login fields must be scalar")
			}
		}
		c.Login = &l
	}
	return nil
}

func (r *runtime) configureHTTPSigning() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(r.config.AWS.Region), awsconfig.WithHTTPClient(r.client))
	if err != nil {
		return errors.New("cannot configure AWS runtime credentials")
	}
	r.aws = c.Credentials
	return nil
}

func (r *runtime) authorizeHTTP(ctx context.Context, req *http.Request, body []byte, password string) ([]string, error) {
	if r.config.GitHubApp != nil {
		return r.authorizeGitHubApp(ctx, req)
	}
	if r.config.OAuthClientCredentials != nil {
		return r.authorizeClientCredentials(ctx, req)
	}

	if r.config.OAuthRefresh != nil {
		return r.authorizeRefresh(ctx, req)
	}
	if a := r.config.AWS; a != nil {
		credentials, err := r.aws.Retrieve(ctx)
		if err != nil {
			return nil, ErrUnavailable
		}
		sum := sha256.Sum256(body)
		payload := hex.EncodeToString(sum[:])
		req.Header.Set("X-Amz-Content-Sha256", payload)
		if err = v4.NewSigner().SignHTTP(ctx, credentials, req, payload, a.Service, a.Region, time.Now()); err != nil {
			return nil, ErrUnavailable
		}
		return []string{credentials.AccessKeyID, credentials.SecretAccessKey, credentials.SessionToken, req.Header.Get("Authorization")}, nil
	}
	if l := r.config.Login; l != nil {
		user, err := readCredential(l.UsernameFile, l.UsernameEnv)
		if err != nil {
			return nil, err
		}
		token, err := r.login(ctx, user, password)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		return []string{user, password, token}, nil
	}
	if r.config.UsernameFile != "" || r.config.UsernameEnv != "" {
		user, err := readCredential(r.config.UsernameFile, r.config.UsernameEnv)
		if err != nil {
			return nil, err
		}
		req.SetBasicAuth(user, password)
		return []string{user, password, strings.TrimPrefix(req.Header.Get("Authorization"), "Basic ")}, nil
	}
	r.authorize(req, password)
	return nil, nil
}

func (r *runtime) login(ctx context.Context, user, password string) (string, error) {
	r.loginMu.Lock()
	defer r.loginMu.Unlock()
	fingerprint := hash([]string{user, password})
	if r.loginFingerprint == fingerprint && time.Now().Before(r.loginUntil) {
		return r.loginToken, nil
	}
	l := r.config.Login
	fields := maps.Clone(l.Fields)
	if fields == nil {
		fields = map[string]any{}
	}
	fields["username"], fields["password"] = user, password
	body, _ := json.Marshal(fields)
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(r.config.Endpoint, "/")+l.Path, bytes.NewReader(body))
	if err != nil {
		return "", ErrInvalid
	}
	req.Header.Set("Content-Type", "application/json")
	if err := checkAuthority(ctx); err != nil {
		return "", err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return "", ErrUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", errors.New("upstream login rejected")
	}
	raw, err := readBounded(resp.Body, 64<<10)
	if err != nil {
		return "", ErrUnavailable
	}
	var data map[string]any
	if json.Unmarshal(raw, &data) != nil {
		return "", ErrUnavailable
	}
	token, ok := data[l.TokenField].(string)
	if !ok || token == "" || strings.ContainsAny(token, "\r\n") || len(token) > 32<<10 {
		return "", ErrUnavailable
	}
	until := time.Now().Add(time.Duration(l.TTLSeconds) * time.Second)
	// The server-issued JWT is opaque authentication. Its expiry can only shorten
	// our cache lifetime; it never grants identity or extends the configured TTL.
	if parts := strings.Split(token, "."); len(parts) == 3 {
		var claims struct {
			Exp int64 `json:"exp"`
		}
		b, e := base64.RawURLEncoding.DecodeString(parts[1])
		if e == nil && json.Unmarshal(b, &claims) == nil && claims.Exp > 0 {
			deadline := time.Unix(claims.Exp, 0).Add(-15 * time.Second)
			if deadline.Before(until) {
				until = deadline
			}
		}
	}
	r.loginToken, r.loginFingerprint, r.loginUntil = token, fingerprint, until
	return token, nil
}

func (r *runtime) invalidateLogin(token string) {
	r.loginMu.Lock()
	defer r.loginMu.Unlock()
	if r.loginToken == strings.TrimPrefix(token, "Bearer ") {
		r.loginUntil = time.Time{}
	}
}

func (r *runtime) httpSecrets() ([]string, error) {
	out := []string{}
	if c := r.config.OAuthClientCredentials; c != nil {
		for _, ref := range [][2]string{{c.ClientIDFile, c.ClientIDEnv}, {c.ClientSecretFile, c.ClientSecretEnv}} {
			value, err := readCredential(ref[0], ref[1])
			if err != nil {
				return nil, err
			}
			out = append(out, value)
		}
		out = append(out, base64.StdEncoding.EncodeToString([]byte(out[0]+":"+out[1])))
	}
	if c := r.config.OAuthRefresh; c != nil {
		for _, ref := range [][2]string{{c.ClientSecretFile, c.ClientSecretEnv}, {c.RefreshTokenFile, c.RefreshTokenEnv}} {
			value, err := readCredential(ref[0], ref[1])
			if err != nil {
				return nil, err
			}
			out = append(out, value)
		}
	}
	file, env := r.config.UsernameFile, r.config.UsernameEnv
	if l := r.config.Login; l != nil {
		file, env = l.UsernameFile, l.UsernameEnv
	}
	if file != "" || env != "" {
		user, err := readCredential(file, env)
		if err != nil {
			return nil, err
		}
		out = append(out, user)
		if r.config.AuthScheme == "Basic" {
			password, err := r.secret()
			if err != nil {
				return nil, err
			}
			out = append(out, base64.StdEncoding.EncodeToString([]byte(user+":"+password)))
		}
	}
	r.loginMu.Lock()
	out = append(out, r.loginToken)
	r.loginMu.Unlock()
	if r.aws != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		c, err := r.aws.Retrieve(ctx)
		if err != nil {
			return nil, err
		}
		out = append(out, c.AccessKeyID, c.SecretAccessKey, c.SessionToken)
	}
	return out, nil
}
