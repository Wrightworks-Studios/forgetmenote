package drivebackup_test

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"forgetmenote/internal/drivebackup"
	"forgetmenote/internal/drivebackup/drivetest"
)

// The authorization asks for an offline grant with PKCE and nothing beyond
// the destination's permissions.
func TestAuthorizeURLRequestsOfflineDestinationAccessWithPKCE(t *testing.T) {
	f := drivetest.New(t)
	verifier := strings.Repeat("v", 43)
	challenge := sha256.Sum256([]byte(verifier))
	for kind, want := range map[string]map[string]string{
		drivebackup.Google:  {"scope": drivebackup.GoogleScope, "access_type": "offline", "prompt": "consent", "client_id": "google-client.example.com"},
		drivebackup.Dropbox: {"scope": strings.Join(drivebackup.DropboxScopes, " "), "token_access_type": "offline", "client_id": "dropbox-app-key"},
	} {
		address, err := f.Client.AuthorizeURL(kind, "https://notes.example.com/backup/x/callback", "state-1", verifier)
		if err != nil {
			t.Fatal(err)
		}
		u, _ := url.Parse(address)
		host := map[string]string{drivebackup.Google: "accounts.google.com", drivebackup.Dropbox: "www.dropbox.com"}[kind]
		if u.Host != host {
			t.Fatalf("%s authorizes at %s", kind, u.Host)
		}
		q := u.Query()
		want["code_challenge"] = base64.RawURLEncoding.EncodeToString(challenge[:])
		want["code_challenge_method"] = "S256"
		want["state"] = "state-1"
		want["response_type"] = "code"
		for key, value := range want {
			if q.Get(key) != value {
				t.Errorf("%s %s = %q, want %q", kind, key, q.Get(key), value)
			}
		}
		if q.Get("code_verifier") != "" || strings.Contains(address, verifier) {
			t.Errorf("%s authorization URL discloses the verifier", kind)
		}
	}
}

// A code exchange sends the verifier and names the account; the grant then
// refreshes, and revoking it ends it at the provider.
func TestGrantExchangeRefreshAndRevoke(t *testing.T) {
	f := drivetest.New(t)
	ctx := t.Context()
	for _, kind := range drivebackup.Kinds {
		code := f.Code(kind, "Alice@Example.com")
		grant, err := f.Client.Exchange(ctx, kind, code, "verifier-"+kind, "https://notes.example.com/callback")
		if err != nil {
			t.Fatal(kind, err)
		}
		f.Lock()
		sent := f.Verifiers[len(f.Verifiers)-1]
		f.Unlock()
		if sent != "verifier-"+kind || grant.Account != "alice@example.com" || grant.Refresh == "" || grant.AccountID == "" {
			t.Fatalf("%s grant: %+v, verifier %q", kind, grant, sent)
		}
		if _, err = f.Client.Exchange(ctx, kind, code, "verifier-"+kind, "https://notes.example.com/callback"); err == nil {
			t.Fatalf("%s: a code was exchanged twice", kind)
		}
		access, err := f.Client.Refresh(ctx, kind, grant.Refresh)
		if err != nil || access.Token == "" || time.Until(access.Expires) < 50*time.Minute {
			t.Fatalf("%s refresh: %+v %v", kind, access, err)
		}
		if err = f.Client.Revoke(ctx, kind, grant.Refresh); err != nil {
			t.Fatal(kind, err)
		}
		if f.Live(grant.Refresh) {
			t.Fatalf("%s grant survived revocation", kind)
		}
		if _, err = f.Client.Refresh(ctx, kind, grant.Refresh); !errors.Is(err, drivebackup.ErrRevoked) {
			t.Fatalf("%s refresh after revocation: %v", kind, err)
		}
		if err = f.Client.Revoke(ctx, kind, grant.Refresh); err != nil {
			t.Fatalf("%s: revoking a revoked grant failed: %v", kind, err)
		}
	}
	f.Lock()
	f.Down = true
	f.Unlock()
	if _, err := f.Client.Refresh(ctx, drivebackup.Google, "refresh-x"); !errors.Is(err, drivebackup.ErrUnavailable) || errors.Is(err, drivebackup.ErrRevoked) {
		t.Fatal("an outage reads as a revoked grant:", err)
	}
}

// The seal key needs 32 bytes of key material, given raw or as hex or base64
// text; sealed values open only for their own purpose.
func TestSealKeyAndPurposes(t *testing.T) {
	f := drivetest.New(t)
	dir := t.TempDir()
	for name, content := range map[string]string{
		"short raw":    strings.Repeat("k", 31),
		"short hex":    strings.Repeat("ab", 16),
		"short base64": base64.StdEncoding.EncodeToString(make([]byte, 24)),
		"empty":        "\n",
	} {
		path := filepath.Join(dir, strings.ReplaceAll(name, " ", "-"))
		os.WriteFile(path, []byte(content), 0600)
		c := f.Config
		c.SealKeyFile = path
		if _, err := drivebackup.New(c); err == nil {
			t.Errorf("%s seal key accepted", name)
		}
	}
	for name, content := range map[string]string{
		"hex":    strings.Repeat("ab", 32) + "\n",
		"base64": base64.StdEncoding.EncodeToString(make([]byte, 32)) + "\n",
		"raw":    strings.Repeat("\x01", 32),
	} {
		path := filepath.Join(dir, name)
		os.WriteFile(path, []byte(content), 0600)
		c := f.Config
		c.SealKeyFile = path
		if _, err := drivebackup.New(c); err != nil {
			t.Errorf("%s seal key refused: %v", name, err)
		}
	}
	sealed, err := f.Client.Seal("refresh gdrive:notes", "refresh-secret")
	if err != nil || strings.Contains(sealed, "refresh-secret") {
		t.Fatal(sealed, err)
	}
	if plain, err := f.Client.Open("refresh gdrive:notes", sealed); err != nil || plain != "refresh-secret" {
		t.Fatal(plain, err)
	}
	if _, err := f.Client.Open("refresh gdrive:other", sealed); err == nil {
		t.Fatal("a sealed value opened for another purpose")
	}
	other := drivetest.New(t)
	if _, err := other.Client.Open("refresh gdrive:notes", sealed); err == nil {
		t.Fatal("a sealed value opened under another key")
	}
	if p := drivebackup.Password(); len(p) != 43 || p == drivebackup.Password() {
		t.Fatal("password is not 32 random bytes in base64url:", p)
	}
}
