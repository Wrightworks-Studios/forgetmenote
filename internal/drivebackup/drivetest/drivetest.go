// Package drivetest runs fake Google and Dropbox OAuth providers for tests: a
// drivebackup.Client whose requests reach an httptest server in place of the
// providers' hosts, which issues, refreshes and revokes synthetic grants.
package drivetest

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"forgetmenote/internal/drivebackup"
)

// Fake is both providers. Its fields are read and set under Lock.
type Fake struct {
	Client *drivebackup.Client
	Config drivebackup.Config
	sync.Mutex
	// Lifetime is the access tokens' lifetime in seconds.
	Lifetime int
	// Down answers every request with 503.
	Down bool
	// Verifiers are the PKCE verifiers the code exchanges sent.
	Verifiers []string
	// Refreshes counts refresh-token grants.
	Refreshes int
	// Revoked lists the refresh tokens revoked, in order.
	Revoked []string
	codes   map[string]grant
	grants  map[string]*grant
	access  map[string]string
	server  *httptest.Server
}

type grant struct {
	kind, email, id, refresh string
	revoked                  bool
}

// New starts the providers with clients configured for both kinds.
func New(t testing.TB) *Fake {
	t.Helper()
	f := &Fake{Lifetime: 3600, codes: map[string]grant{}, grants: map[string]*grant{}, access: map[string]string{}}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	dir := t.TempDir()
	write := func(name, content string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	f.Config = drivebackup.Config{
		SealKeyFile: write("seal", random(32)+"\n"),
		Google:      &drivebackup.GoogleConfig{ClientID: "google-client.example.com", ClientSecretFile: write("google", "google-client-secret")},
		Dropbox:     &drivebackup.DropboxConfig{AppKey: "dropbox-app-key", AppSecretFile: write("dropbox", "dropbox-app-secret")},
	}
	client, err := drivebackup.New(f.Config)
	if err != nil {
		t.Fatal(err)
	}
	target, _ := url.Parse(f.server.URL)
	base := f.server.Client().Transport
	client.HTTP = &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		r = r.Clone(r.Context())
		r.Header.Set("X-Provider-Host", r.URL.Host)
		r.URL.Scheme, r.URL.Host = target.Scheme, target.Host
		return base.RoundTrip(r)
	})}
	f.Client = client
	return f
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func random(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Code issues an authorization code for an account of kind.
func (f *Fake) Code(kind, email string) string {
	f.Lock()
	defer f.Unlock()
	code := "code-" + random(8)
	f.codes[code] = grant{kind: kind, email: email, id: "dbid:" + email}
	return code
}

// RevokeAccount ends every grant of an account at its provider, as its owner
// removing the app's access does.
func (f *Fake) RevokeAccount(kind, email string) {
	f.Lock()
	defer f.Unlock()
	for _, g := range f.grants {
		if g.kind == kind && g.email == email {
			g.revoked = true
		}
	}
}

// RefreshTokens lists every refresh token the providers issued.
func (f *Fake) RefreshTokens() []string {
	f.Lock()
	defer f.Unlock()
	out := []string{}
	for token := range f.grants {
		out = append(out, token)
	}
	return out
}

// Live reports whether a refresh token still works.
func (f *Fake) Live(refresh string) bool {
	f.Lock()
	defer f.Unlock()
	g := f.grants[refresh]
	return g != nil && !g.revoked
}

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	f.Lock()
	defer f.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if f.Down {
		w.WriteHeader(503)
		return
	}
	host := r.Header.Get("X-Provider-Host")
	refuse := func(status int, code string) {
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(map[string]string{"error": code})
	}
	bearer := func() *grant {
		token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		g := f.grants[f.access[token]]
		if g == nil || g.revoked {
			return nil
		}
		return g
	}
	switch {
	case host == "oauth2.googleapis.com" && r.URL.Path == "/token", host == "api.dropboxapi.com" && r.URL.Path == "/oauth2/token":
		kind, id, secret := drivebackup.Google, "google-client.example.com", "google-client-secret"
		if host == "api.dropboxapi.com" {
			kind, id, secret = drivebackup.Dropbox, "dropbox-app-key", "dropbox-app-secret"
		}
		r.ParseForm()
		if r.PostForm.Get("client_id") != id || r.PostForm.Get("client_secret") != secret {
			refuse(401, "invalid_client")
			return
		}
		var g *grant
		switch r.PostForm.Get("grant_type") {
		case "authorization_code":
			issued, ok := f.codes[r.PostForm.Get("code")]
			if !ok || issued.kind != kind || r.PostForm.Get("code_verifier") == "" {
				refuse(400, "invalid_grant")
				return
			}
			delete(f.codes, r.PostForm.Get("code"))
			f.Verifiers = append(f.Verifiers, r.PostForm.Get("code_verifier"))
			issued.refresh = "refresh-" + kind + "-" + random(16)
			g = &issued
			f.grants[g.refresh] = g
		case "refresh_token":
			g = f.grants[r.PostForm.Get("refresh_token")]
			if g == nil || g.revoked || g.kind != kind {
				refuse(400, "invalid_grant")
				return
			}
			f.Refreshes++
		default:
			refuse(400, "unsupported_grant_type")
			return
		}
		access := "access-" + random(16)
		f.access[access] = g.refresh
		out := map[string]any{"access_token": access, "expires_in": f.Lifetime, "token_type": "Bearer"}
		if r.PostForm.Get("grant_type") == "authorization_code" {
			out["refresh_token"] = g.refresh
		}
		if kind == drivebackup.Google {
			out["scope"] = drivebackup.GoogleScope
		} else {
			out["scope"] = strings.Join(drivebackup.DropboxScopes, " ")
			out["account_id"] = g.id
		}
		json.NewEncoder(w).Encode(out)
	case host == "oauth2.googleapis.com" && r.URL.Path == "/revoke":
		r.ParseForm()
		g := f.grants[r.PostForm.Get("token")]
		if g == nil {
			refuse(400, "invalid_token")
			return
		}
		// Google ends the client's whole grant for the account.
		for _, other := range f.grants {
			if other.kind == drivebackup.Google && other.email == g.email && !other.revoked {
				other.revoked = true
				f.Revoked = append(f.Revoked, other.refresh)
			}
		}
		w.Write([]byte("{}"))
	case host == "www.googleapis.com" && r.URL.Path == "/drive/v3/about":
		g := bearer()
		if g == nil || r.URL.Query().Get("fields") != "user(emailAddress)" {
			refuse(401, "invalid_token")
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"user": map[string]string{"emailAddress": g.email}})
	case host == "api.dropboxapi.com" && r.URL.Path == "/2/users/get_current_account":
		g := bearer()
		if g == nil || r.Method != "POST" {
			refuse(401, "invalid_access_token")
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"account_id": g.id, "email": g.email, "email_verified": true})
	case host == "api.dropboxapi.com" && r.URL.Path == "/2/auth/token/revoke":
		g := bearer()
		if g == nil || r.Method != "POST" {
			refuse(401, "invalid_access_token")
			return
		}
		// Dropbox ends the refresh token the access token came from.
		g.revoked = true
		f.Revoked = append(f.Revoked, g.refresh)
		w.Write([]byte("null"))
	default:
		http.NotFound(w, r)
	}
}
