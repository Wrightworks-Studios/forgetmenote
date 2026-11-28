package web

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"forgetmenote/internal/backups"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/drivebackup"
	"forgetmenote/internal/drivebackup/drivetest"
	"forgetmenote/internal/githubapp"
	"forgetmenote/internal/outgoing"
)

const driveWorkerSecret = "drive-worker-secret-0123456789abcdef"

// driveFixture runs the drive providers against fakes, with the GitHub App
// too when github is set. alice owns "default"; people sign in through the
// trusted proxy.
func driveFixture(t *testing.T, app *Server, github bool) *drivetest.Fake {
	t.Helper()
	if err := app.ConfigureProxy(proxyTestSecret, []string{"127.0.0.1/32"}); err != nil {
		t.Fatal(err)
	}
	if err := app.catalog.Member(catalog.Bootstrap, "default", "alice", "owner"); err != nil {
		t.Fatal(err)
	}
	f := drivetest.New(t)
	dir := t.TempDir()
	files := map[string][]byte{"worker": []byte(driveWorkerSecret)}
	cfg := backups.AppConfig{Workers: map[string]backups.WorkerConfig{"fixture": {Owner: "local", TokenFile: filepath.Join(dir, "worker")}}, Drives: &f.Config}
	if github {
		key, _ := rsa.GenerateKey(rand.Reader, 2048)
		files["key"] = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
		files["client"], files["webhook"] = []byte("synthetic"), []byte(strings.Repeat("s", 32))
		cfg.App = githubapp.Config{AppID: "2", Slug: "backups", ClientID: "backups", PrivateKeyFile: filepath.Join(dir, "key"), ClientSecretFile: filepath.Join(dir, "client"), WebhookSecretFile: filepath.Join(dir, "webhook")}
	}
	for name, b := range files {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := app.ConfigureBackupsConfig(cfg); err != nil {
		t.Fatal(err)
	}
	app.backups.drives.HTTP = f.Client.HTTP
	return f
}

// browserCall is a signed-in person's browser request with its cookies.
func browserCall(app *Server, user, method, path, body string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, app.origin+path, strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("Remote-User", user)
	r.Header.Set("X-ForgetMeNote-Proxy-Secret", proxyTestSecret)
	r.Header.Set("Origin", app.origin)
	r.Header.Set("Content-Type", "application/json")
	for _, c := range cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	app.ServeHTTP(w, r)
	return w
}

func workerCall(app *Server, path, body string) *httptest.ResponseRecorder {
	method := "POST"
	if body == "" {
		method = "GET"
	}
	r := httptest.NewRequest(method, "http://localhost/internal/backups/v1/"+path, strings.NewReader(body))
	r.Header.Set("X-Backup-Worker", "fixture")
	r.Header.Set("Authorization", "Bearer "+driveWorkerSecret)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	app.ServeHTTP(w, r)
	return w
}

// beginDrive starts a drive connection and returns the provider's
// authorization request and the browser's binding cookie.
func beginDrive(t *testing.T, app *Server, user, project, kind string) (url.Values, *http.Cookie) {
	t.Helper()
	w := browserCall(app, user, "POST", "/p/"+project+"/api/managed-backup/drive/connect", `{"kind":"`+kind+`"}`)
	var out struct{ URL string }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
		t.Fatalf("connect: %d %s", w.Code, w.Body)
	}
	address, _ := url.Parse(out.URL)
	for _, c := range w.Result().Cookies() {
		if c.Name == driveBinding.name {
			return address.Query(), c
		}
	}
	t.Fatal("connect set no binding cookie")
	return nil, nil
}

func driveCallbackCall(app *Server, user, kind string, query url.Values, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	return browserCall(app, user, "GET", driveCallbackPath(kind)+"?"+query.Encode(), "", cookies...)
}

// disclosed reports a refresh token the providers issued in body.
func disclosed(f *drivetest.Fake, body string) bool {
	for _, token := range f.RefreshTokens() {
		if strings.Contains(body, token) {
			return true
		}
	}
	return strings.Contains(body, "sealed")
}

// An Owner connects Google Drive with PKCE, a state the provider returns and
// the browser that started it, then consents on a setup screen naming the
// account and folder. The encryption password is shown once; no refresh
// token reaches a response, the worker's list or a lease.
func TestDriveConnectionNeedsItsBrowserStateAndConsent(t *testing.T) {
	app, _ := fixture(t)
	f := driveFixture(t, app, false)
	status := browserCall(app, "alice", "GET", "/p/default/api/managed-backup", "")
	if status.Code != 200 || !strings.Contains(status.Body.String(), `"configured":false`) || !strings.Contains(status.Body.String(), `"kind":"gdrive"`) || !strings.Contains(status.Body.String(), `"kind":"dropbox"`) {
		t.Fatalf("status without a GitHub App: %d %s", status.Code, status.Body)
	}
	if w := browserCall(app, "alice", "POST", "/p/default/api/managed-backup/connect", `{}`); w.Code != 503 {
		t.Fatalf("a GitHub setup started without a GitHub App: %d", w.Code)
	}
	if w := browserCall(app, "alice", "POST", "/p/default/api/managed-backup/drive/connect", `{"kind":"onedrive"}`); w.Code != 503 {
		t.Fatalf("an unconfigured kind started: %d", w.Code)
	}
	if w := browserCall(app, "reader", "POST", "/p/default/api/managed-backup/drive/connect", `{"kind":"gdrive"}`); w.Code != 403 {
		t.Fatalf("a non-Owner started a setup: %d", w.Code)
	}
	query, cookie := beginDrive(t, app, "alice", "default", drivebackup.Google)
	code := f.Code(drivebackup.Google, "alice@example.com")
	callback := url.Values{"state": {query.Get("state")}, "code": {code}}
	if w := driveCallbackCall(app, "alice", drivebackup.Google, callback); w.Code != 403 {
		t.Fatalf("a callback without its browser's binding: %d", w.Code)
	}
	if w := driveCallbackCall(app, "alice", drivebackup.Google, url.Values{"state": {"forged"}, "code": {code}}, cookie); w.Code != 403 {
		t.Fatalf("a callback with another state: %d", w.Code)
	}
	if w := driveCallbackCall(app, "mallory", drivebackup.Google, callback, cookie); w.Code != 403 {
		t.Fatalf("another person's callback: %d", w.Code)
	}
	if w := driveCallbackCall(app, "alice", drivebackup.Dropbox, callback, cookie); w.Code != 403 {
		t.Fatalf("a callback for another provider: %d", w.Code)
	}
	w := driveCallbackCall(app, "alice", drivebackup.Google, callback, cookie)
	if w.Code != 303 || w.Header().Get("Location") != "/?project=default&backup=gdrive" {
		t.Fatalf("callback: %d %s %s", w.Code, w.Header().Get("Location"), w.Body)
	}
	f.Lock()
	verifier := f.Verifiers[len(f.Verifiers)-1]
	f.Unlock()
	challenge := sha256.Sum256([]byte(verifier))
	if query.Get("code_challenge") != base64.RawURLEncoding.EncodeToString(challenge[:]) || query.Get("code_challenge_method") != "S256" {
		t.Fatal("the code exchange did not send the authorization's PKCE verifier")
	}
	if w = driveCallbackCall(app, "alice", drivebackup.Google, callback, cookie); w.Code != 403 {
		t.Fatalf("a replayed callback: %d", w.Code)
	}
	pending := browserCall(app, "alice", "GET", "/p/default/api/managed-backup/drive/pending?kind=gdrive", "", cookie)
	var offer struct {
		Account, Path string
		Continuing    bool
	}
	if pending.Code != 200 || json.Unmarshal(pending.Body.Bytes(), &offer) != nil || offer.Account != "alice@example.com" || !strings.HasPrefix(offer.Path, "ForgetMeNote/default-") || offer.Continuing || disclosed(f, pending.Body.String()) {
		t.Fatalf("setup screen: %d %s", pending.Code, pending.Body)
	}
	if w = browserCall(app, "alice", "GET", "/p/default/api/managed-backup/drive/pending?kind=gdrive", ""); w.Code != 403 {
		t.Fatalf("another browser read the setup: %d", w.Code)
	}
	if w = browserCall(app, "alice", "POST", "/p/default/api/managed-backup/drive/enable", `{"kind":"gdrive","generation":0,"encrypt":true}`, cookie); w.Code != 400 {
		t.Fatalf("a setup without consent: %d %s", w.Code, w.Body)
	}
	w = browserCall(app, "alice", "POST", "/p/default/api/managed-backup/drive/enable", `{"kind":"gdrive","generation":0,"encrypt":true,"consent":true}`, cookie)
	var connected struct {
		Drive    backups.Drive
		Password string
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &connected) != nil || len(connected.Password) != 43 || !connected.Drive.Connected || !connected.Drive.Encrypted || connected.Drive.Path != offer.Path || disclosed(f, w.Body.String()) || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("enable: %d %s", w.Code, w.Body)
	}
	if w = browserCall(app, "alice", "POST", "/p/default/api/managed-backup/drive/enable", `{"kind":"gdrive","generation":1,"encrypt":true,"consent":true}`, cookie); w.Code != 403 {
		t.Fatalf("a consumed setup was used again: %d", w.Code)
	}
	status = browserCall(app, "alice", "GET", "/p/default/api/managed-backup", "")
	if status.Code != 200 || !strings.Contains(status.Body.String(), `"account":"alice@example.com"`) || strings.Contains(status.Body.String(), connected.Password) || disclosed(f, status.Body.String()) {
		t.Fatalf("status: %d %s", status.Code, status.Body)
	}
	list := workerCall(app, "bindings", "")
	if list.Code != 200 || !strings.Contains(list.Body.String(), `"path":"`+offer.Path+`"`) || strings.Contains(list.Body.String(), connected.Password) || disclosed(f, list.Body.String()) {
		t.Fatalf("worker list: %d %s", list.Code, list.Body)
	}
	head := strings.Repeat("a", 40)
	lease := workerCall(app, "lease", `{"project":"default","kind":"gdrive","generation":1,"revision":"`+head+`"}`)
	var l backups.DriveLease
	if lease.Code != 200 || json.Unmarshal(lease.Body.Bytes(), &l) != nil || l.Encryption == nil || l.Encryption.Password != connected.Password || time.Until(l.TokenExpires) < 10*time.Minute || disclosed(f, lease.Body.String()) {
		t.Fatalf("lease: %d %s", lease.Code, lease.Body)
	}
	receipt := workerCall(app, "receipt", `{"lease":"`+l.ID+`","project":"default","kind":"gdrive","generation":1,"revision":"`+head+`","snapshot":{"name":"default-20261127T100000Z-aaaaaaaaaaaa.bundle.enc","sha256":"`+strings.Repeat("b", 64)+`","size":10,"kept":1,"deleted":[]}}`)
	if receipt.Code != 200 || disclosed(f, receipt.Body.String()) || strings.Contains(receipt.Body.String(), connected.Password) {
		t.Fatalf("receipt: %d %s", receipt.Code, receipt.Body)
	}
	status = browserCall(app, "alice", "GET", "/p/default/api/managed-backup", "")
	if !strings.Contains(status.Body.String(), `"lastSnapshot":"default-20261127T100000Z-aaaaaaaaaaaa.bundle.enc"`) || !strings.Contains(status.Body.String(), `"kept":1`) {
		t.Fatalf("status after a receipt: %s", status.Body)
	}
	refresh := f.RefreshTokens()
	if w = browserCall(app, "alice", "POST", "/p/default/api/managed-backup/control", `{"kind":"gdrive","generation":1,"action":"disconnect"}`); w.Code != 200 || !strings.Contains(w.Body.String(), `"connected":false`) {
		t.Fatalf("disconnect: %d %s", w.Code, w.Body)
	}
	for _, token := range refresh {
		if f.Live(token) {
			t.Fatal("disconnect left the Google grant live")
		}
	}
}

// A personal notebook's history goes to a destination only from its own
// account: an Admin's authority over another person's Inbox connects neither
// a drive nor GitHub, nor controls one.
func TestPersonalNotebookBackupsStayWithTheirPerson(t *testing.T) {
	app, _ := fixture(t)
	f := driveFixture(t, app, true)
	if err := app.catalog.SetUser(catalog.Bootstrap, catalog.User{ID: "admin", Admin: true}); err != nil {
		t.Fatal(err)
	}
	inbox, err := app.catalog.EnsureInbox("bob")
	if err != nil {
		t.Fatal(err)
	}
	base := "/p/" + inbox.ID + "/api/managed-backup"
	if w := browserCall(app, "admin", "POST", base+"/drive/connect", `{"kind":"dropbox"}`); w.Code != 403 {
		t.Errorf("an Admin started a drive backup of another person's Inbox: %d %s", w.Code, w.Body)
	}
	if w := browserCall(app, "admin", "POST", base+"/connect", `{}`); w.Code != 403 {
		t.Errorf("an Admin started a GitHub backup of another person's Inbox: %d %s", w.Code, w.Body)
	}
	if w := browserCall(app, "bob", "POST", base+"/connect", `{}`); w.Code != 200 || !strings.Contains(w.Body.String(), "github.com/login/oauth/authorize") {
		t.Fatalf("the Inbox's person could not start a GitHub backup: %d %s", w.Code, w.Body)
	}
	query, cookie := beginDrive(t, app, "bob", inbox.ID, drivebackup.Dropbox)
	callback := url.Values{"state": {query.Get("state")}, "code": {f.Code(drivebackup.Dropbox, "bob@example.com")}}
	if w := driveCallbackCall(app, "bob", drivebackup.Dropbox, callback, cookie); w.Code != 303 {
		t.Fatalf("callback: %d %s", w.Code, w.Body)
	}
	if w := browserCall(app, "admin", "POST", base+"/drive/enable", `{"kind":"dropbox","generation":0,"consent":true}`, cookie); w.Code != 403 {
		t.Fatalf("an Admin finished another person's setup with their browser binding: %d %s", w.Code, w.Body)
	}
	if w := browserCall(app, "bob", "POST", base+"/drive/enable", `{"kind":"dropbox","generation":0,"consent":true}`, cookie); w.Code != 200 || strings.Contains(w.Body.String(), `"password"`) {
		t.Fatalf("bob's unencrypted setup: %d %s", w.Code, w.Body)
	}
	if w := browserCall(app, "admin", "POST", base+"/control", `{"kind":"dropbox","generation":1,"action":"pause"}`); w.Code != 403 {
		t.Fatalf("an Admin paused another person's Inbox backup: %d %s", w.Code, w.Body)
	}
	if w := browserCall(app, "admin", "GET", base, ""); w.Code != 200 {
		t.Fatalf("an Admin could not read the Inbox's backup status: %d", w.Code)
	}
	overview := browserCall(app, "admin", "POST", "/api/organization/action/managed_backup_overview", `{}`)
	if overview.Code != 200 || !strings.Contains(overview.Body.String(), `"kind":"dropbox"`) || strings.Contains(overview.Body.String(), "bob@example.com") || disclosed(f, overview.Body.String()) {
		t.Fatalf("the overview shows a personal notebook's provider account: %d %s", overview.Code, overview.Body)
	}
	if w := workerCall(app, "lease", `{"project":"`+inbox.ID+`","kind":"dropbox","generation":1,"revision":"`+strings.Repeat("a", 40)+`"}`); w.Code != 200 {
		t.Fatalf("lease for the Inbox's own destination: %d %s", w.Code, w.Body)
	}
}

// The Admin page lists every notebook that can be backed up, those without a
// managed destination first, with when each destination last verified. It is
// for Admins only, and offboarding lists a person's drive destinations.
func TestAdminBackupOverviewPutsUnprotectedNotebooksFirst(t *testing.T) {
	app, _ := fixture(t)
	f := driveFixture(t, app, false)
	if err := app.catalog.SetUser(catalog.Bootstrap, catalog.User{ID: "admin", Admin: true}); err != nil {
		t.Fatal(err)
	}
	for id, name := range map[string]string{"zeta": "Zeta", "alpha": "Alpha"} {
		if err := app.catalog.Create(catalog.Bootstrap, id, name); err != nil {
			t.Fatal(err)
		}
	}
	grant, err := app.backups.drives.Exchange(t.Context(), drivebackup.Google, f.Code(drivebackup.Google, "alice@example.com"), "verifier", "https://notes.example.com/backup/google/callback")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = app.backups.manager.ConnectDrive(t.Context(), backups.DriveSetup{Drive: backups.Drive{Kind: drivebackup.Google, Project: "default", Actor: "alice", Owner: app.catalog.StorageOwner("default"), Epoch: app.catalog.Epoch("alice"), Authority: app.catalog.ProjectAccessEpoch("alice", "default"), NotebookGeneration: app.catalog.Authority("default").Generation, Account: grant.Account, AccountID: grant.AccountID, Path: backups.DrivePath("default")}, Refresh: grant.Refresh}); err != nil {
		t.Fatal(err)
	}
	if w := browserCall(app, "alice", "POST", "/api/organization/action/managed_backup_overview", `{}`); w.Code != 403 {
		t.Fatalf("an Owner read the instance's backup overview: %d", w.Code)
	}
	w := browserCall(app, "admin", "POST", "/api/organization/action/managed_backup_overview", `{}`)
	var overview backupOverview
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &overview) != nil {
		t.Fatalf("overview: %d %s", w.Code, w.Body)
	}
	order := []string{}
	for _, row := range overview.Notebooks {
		order = append(order, row.Project)
	}
	if strings.Join(order, ",") != "alpha,zeta,default" || len(overview.Notebooks[2].Destinations) != 1 || overview.Notebooks[2].Destinations[0].Account != "alice@example.com" || overview.GitHub || len(overview.Drives) != 2 || disclosed(f, w.Body.String()) {
		t.Fatalf("overview: %s", w.Body)
	}
	offboarding := browserCall(app, "admin", "POST", "/api/organization/action/offboarding_status", `{"user":"alice"}`)
	if offboarding.Code != 200 || !strings.Contains(offboarding.Body.String(), `"destination":"gdrive"`) {
		t.Fatalf("offboarding omits the drive destination: %d %s", offboarding.Code, offboarding.Body)
	}
}

// A copy restored beside the live instance holds the live instance's drive
// grants: it leases none, starts no setup and calls no provider.
func TestPausedInstanceLeasesNoDriveToken(t *testing.T) {
	app := pausedFixture(t)
	f := driveFixture(t, app, false)
	if w := workerCall(app, "lease", `{"project":"default","kind":"gdrive","generation":1,"revision":"`+strings.Repeat("a", 40)+`"}`); w.Code != 503 || !strings.Contains(w.Body.String(), `"code":"outgoing_paused"`) {
		t.Fatalf("paused drive lease: %d %s", w.Code, w.Body)
	}
	if w := browserCall(app, "alice", "POST", "/p/default/api/managed-backup/drive/connect", `{"kind":"gdrive"}`); w.Code != 503 || !strings.Contains(w.Body.String(), outgoing.ErrPaused.Error()) {
		t.Fatalf("paused drive setup: %d %s", w.Code, w.Body)
	}
	f.Lock()
	defer f.Unlock()
	if f.Refreshes != 0 || len(f.Verifiers) != 0 {
		t.Fatal("a paused copy called a drive provider")
	}
}
