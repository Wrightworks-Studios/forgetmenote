package web

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/reposource"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAdministratorRepositoryImportUsesInstalledApp(t *testing.T) {
	app, _ := fixture(t)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for name, body := range map[string][]byte{"key": pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), "client": []byte("fixture-client"), "webhook": []byte(strings.Repeat("a", 32))} {
		if err = os.WriteFile(filepath.Join(dir, name), body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	g, err := reposource.NewGitHub(reposource.Config{AppID: "123", Slug: "fixture", ClientID: "fixture", PrivateKeyFile: filepath.Join(dir, "key"), ClientSecretFile: filepath.Join(dir, "client"), WebhookSecretFile: filepath.Join(dir, "webhook")})
	if err != nil {
		t.Fatal(err)
	}
	revoked := false
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if revoked {
			http.Error(w, "revoked", 403)
			return
		}
		switch r.URL.Path {
		case "/app/installations/7/access_tokens":
			var in struct {
				IDs         []int64           `json:"repository_ids"`
				Permissions map[string]string `json:"permissions"`
			}
			json.NewDecoder(r.Body).Decode(&in)
			if len(in.IDs) != 1 || in.IDs[0] != 42 || in.Permissions["contents"] != "read" {
				t.Error("overbroad token")
			}
			json.NewEncoder(w).Encode(map[string]any{"token": "fixture-installation-token", "expires_at": time.Now().Add(time.Hour)})
		case "/repositories/42":
			json.NewEncoder(w).Encode(reposource.Repository{ID: 42, FullName: "example/source", DefaultBranch: "main", Private: true})
		case "/repos/example/source/commits/main":
			json.NewEncoder(w).Encode(map[string]string{"sha": strings.Repeat("a", 40)})
		default:
			t.Errorf("unexpected GitHub route %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer gh.Close()
	g.API = gh.URL
	m, err := reposource.Open(app.db, filepath.Join(dir, "sources"), nil, g, func(project, actor string) error { return app.catalog.CheckNotebookOwner(actor, project) })
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	app.repositories = m
	app.repositoryConnections = &repositoryConnections{github: g}
	in := `{"id":"source","name":"Source","installation":7,"repository":42,"shareWithNotebook":true,"externalProcessing":true}`
	endpoint := "/api/v1/actions/import_repository"
	w := apiCall(app, "POST", endpoint, in, testToken)
	if w.Code != 200 {
		t.Fatalf("import: %d %s", w.Code, w.Body.String())
	}
	s, err := m.Status("source")
	if err != nil || !s.Connected || !s.IndexConfig.ExternalProcessing {
		t.Fatalf("source: %+v %v", s, err)
	}
	if got := app.catalog.Authority("source"); got.Type != "github" {
		t.Fatal("wrong notebook authority", got)
	}
	// A lost response can be resumed without connecting the same notebook twice.
	if w = apiCall(app, "POST", endpoint, in, testToken); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	s2, _ := m.Status("source")
	if s2.ID != s.ID {
		t.Fatal("duplicate source on resume")
	}
	app.ConfigureProxy(proxyTestSecret, []string{"127.0.0.1/32"})
	app.catalog.Member(catalog.Bootstrap, "default", "owner", "owner")
	grant, err := issueCredential(app.catalog, app.oauth, "owner", CredentialSpec{Name: "owner", AllProjects: true, Scopes: []string{"notes:read", "notes:manage"}, Audiences: []string{"api"}, Days: 1})
	if err != nil {
		t.Fatal(err)
	}
	if w = apiCall(app, "POST", endpoint, strings.ReplaceAll(in, `"source"`, `"other"`), grant.Token); w.Code != 403 {
		t.Fatal("owner imported through instance credentials", w.Code, w.Body.String())
	}
	if w = apiCall(app, "POST", endpoint, strings.ReplaceAll(in, `"source"`, `"default"`), testToken); w.Code == 200 {
		t.Fatal("changed existing notebook authority")
	}
	revoked = true
	if w = apiCall(app, "POST", endpoint, strings.ReplaceAll(in, `"source"`, `"denied"`), testToken); w.Code == 200 {
		t.Fatal("import accepted revoked installation")
	}
	for _, p := range app.catalog.List(catalog.Bootstrap) {
		if p.ID == "denied" {
			t.Fatal("failed authorization left a notebook")
		}
	}
}

func TestRepositoryWebhookUsesSignatureInsteadOfBrowserAuthentication(t *testing.T) {
	app, _ := fixture(t)
	dir := t.TempDir()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	secret := strings.Repeat("synthetic-webhook-", 3)
	files := map[string][]byte{
		"app.pem": pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}),
		"client":  []byte("synthetic-client-secret"),
		"webhook": []byte(secret),
	}
	for name, content := range files {
		if err = os.WriteFile(filepath.Join(dir, name), content, 0600); err != nil {
			t.Fatal(err)
		}
	}
	g, err := reposource.NewGitHub(reposource.Config{AppID: "123", Slug: "fixture", ClientID: "fixture", PrivateKeyFile: filepath.Join(dir, "app.pem"), ClientSecretFile: filepath.Join(dir, "client"), WebhookSecretFile: filepath.Join(dir, "webhook")})
	if err != nil {
		t.Fatal(err)
	}
	m, err := reposource.Open(app.db, filepath.Join(dir, "sources"), nil, g, func(string, string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	app.repositories = m
	app.repositoryConnections = &repositoryConnections{github: g}
	body := `{"repository":{"id":42},"installation":{"id":7}}`
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(body))
	signature := "sha256=" + hex.EncodeToString(h.Sum(nil))
	for _, test := range []struct {
		name, body, signature string
		status                int
	}{
		{"signed without browser login", body, signature, http.StatusAccepted},
		{"unsigned", body, "", http.StatusForbidden},
		{"tampered bytes", body + " ", signature, http.StatusForbidden},
		{"oversized", strings.Repeat("a", (1<<20)+1), signature, http.StatusRequestEntityTooLarge},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", app.origin+"/repository/github/webhook", strings.NewReader(test.body))
			r.Header.Set("X-Hub-Signature-256", test.signature)
			r.Header.Set("X-GitHub-Event", "push")
			r.Header.Set("X-GitHub-Delivery", "delivery-one")
			w := httptest.NewRecorder()
			app.ServeHTTP(w, r)
			if w.Code != test.status {
				t.Fatalf("status: %d, %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestRepositorySourceRoutesRequireNotebookAuthority(t *testing.T) {
	app, _ := fixture(t)
	if w := call(app, "GET", "/api/repository", "", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous source status: %d", w.Code)
	}
	w := call(app, "GET", "/api/repository", "", testToken)
	if w.Code != http.StatusOK {
		t.Fatalf("source status: %d %s", w.Code, w.Body.String())
	}
	var status struct {
		Configured bool `json:"configured"`
		Connected  bool `json:"connected"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil || status.Configured || status.Connected {
		t.Fatalf("unconfigured source: %s (%v)", w.Body.String(), err)
	}
}

func TestRepositoryControlsRequireOwnerAndUseSeparateConfiguration(t *testing.T) {
	app, _ := fixture(t)
	if err := app.ConfigureProxy(proxyTestSecret, []string{"127.0.0.1/32"}); err != nil {
		t.Fatal(err)
	}
	for actor, role := range map[string]string{"reader": "read", "editor": "edit", "owner": "owner"} {
		if err := app.catalog.Member(catalog.Bootstrap, "default", actor, role); err != nil {
			t.Fatal(err)
		}
	}
	for _, endpoint := range []string{"connect", "select", "control", "refresh"} {
		for _, actor := range []string{"reader", "editor"} {
			w := proxyCall(app, actor, "POST", "/p/default/api/repository/"+endpoint, `{}`)
			if w.Code != 403 {
				t.Errorf("%s %s: %d", actor, endpoint, w.Code)
			}
		}
		w := proxyCall(app, "owner", "POST", "/p/default/api/repository/"+endpoint, `{}`)
		want := http.StatusServiceUnavailable
		if w.Code != want {
			t.Errorf("owner unconfigured %s: %d", endpoint, w.Code)
		}
	}
}
