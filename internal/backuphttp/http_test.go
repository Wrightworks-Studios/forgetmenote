package backuphttp_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"forgetmenote/internal/backuphttp"
	"forgetmenote/internal/backups"
	"forgetmenote/internal/drivebackup"
	"forgetmenote/internal/drivebackup/drivetest"
	"forgetmenote/internal/pg/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

// The worker reads drive destinations, leases, checks, receipts and drains
// them with a kind, in the shapes the worker contract names, and a request
// without a kind keeps the GitHub binding's meaning.
func TestWorkerDriveContract(t *testing.T) {
	f := drivetest.New(t)
	m, err := backups.Open(pgtest.Org(t), nil, f.Client, func(b backups.Binding) error {
		if b.Owner != "local" {
			return backups.ErrDenied
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	ctx := t.Context()
	grant, err := f.Client.Exchange(ctx, drivebackup.Dropbox, f.Code(drivebackup.Dropbox, "alice@example.com"), "verifier", "https://notes.example.com/backup/dropbox/callback")
	if err != nil {
		t.Fatal(err)
	}
	d, password, err := m.ConnectDrive(ctx, backups.DriveSetup{Drive: backups.Drive{Kind: drivebackup.Dropbox, Project: "notes", Actor: "alice", Owner: "local", Account: grant.Account, AccountID: grant.AccountID, Path: backups.DrivePath("notes")}, Refresh: grant.Refresh, Encrypt: true})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(backuphttp.New(m, map[string]backuphttp.Worker{"worker": backuphttp.NewWorker("local", []byte("worker-secret"))}))
	defer server.Close()
	call := func(method, path, body string) (int, string) {
		t.Helper()
		r, _ := http.NewRequest(method, server.URL+"/internal/backups/v1/"+path, strings.NewReader(body))
		r.Header.Set("X-Backup-Worker", "worker")
		r.Header.Set("Authorization", "Bearer worker-secret")
		r.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		raw, _ := io.ReadAll(res.Body)
		if strings.Contains(string(raw), grant.Refresh) || strings.Contains(string(raw), "sealed") {
			t.Fatalf("%s %s disclosed the stored grant: %s", method, path, raw)
		}
		return res.StatusCode, string(raw)
	}
	keys := func(raw string) []string {
		var out map[string]json.RawMessage
		if err := json.Unmarshal([]byte(raw), &out); err != nil {
			t.Fatal(raw, err)
		}
		names := []string{}
		for name := range out {
			names = append(names, name)
		}
		slices.Sort(names)
		return names
	}

	status, raw := call("GET", "bindings", "")
	var entries []json.RawMessage
	if status != 200 || json.Unmarshal([]byte(raw), &entries) != nil || len(entries) != 1 || strings.Contains(raw, password) || strings.Contains(raw, "alice") {
		t.Fatalf("bindings %d: %s", status, raw)
	}
	if got := keys(string(entries[0])); !slices.Equal(got, []string{"blocked", "enabled", "encrypted", "generation", "kind", "notebookGeneration", "path", "project", "request"}) {
		t.Fatalf("drive entry fields: %v", got)
	}

	sha := strings.Repeat("a", 40)
	if status, _ = call("POST", "lease", `{"project":"notes","generation":1,"revision":"`+sha+`"}`); status != 403 {
		t.Fatalf("a GitHub lease without a GitHub App answered %d", status)
	}
	status, raw = call("POST", "lease", `{"project":"notes","kind":"dropbox","generation":1,"revision":"`+sha+`"}`)
	if status != 200 {
		t.Fatalf("lease %d: %s", status, raw)
	}
	if got := keys(raw); !slices.Equal(got, []string{"encryption", "expires", "generation", "id", "kind", "notebookGeneration", "path", "project", "revision", "token", "tokenExpires"}) {
		t.Fatalf("lease fields: %v", got)
	}
	var lease struct {
		ID           string
		Path         string
		Token        string
		TokenExpires time.Time
		Expires      time.Time
		Encryption   *struct{ Password string }
	}
	json.Unmarshal([]byte(raw), &lease)
	if lease.Path != d.Path || lease.Token == "" || time.Until(lease.TokenExpires) < 10*time.Minute || time.Until(lease.Expires) > 3*time.Minute || lease.Encryption == nil || lease.Encryption.Password != password {
		t.Fatalf("lease: %s", raw)
	}
	if status, raw = call("POST", "check", `{"lease":"`+lease.ID+`"}`); status != 200 || raw != "{\"valid\":true}\n" {
		t.Fatalf("check %d: %s", status, raw)
	}
	if status, _ = call("POST", "receipt", `{"lease":"`+lease.ID+`","project":"notes","generation":1,"revision":"`+sha+`","snapshot":{"name":"x","sha256":"`+strings.Repeat("b", 64)+`","size":1,"kept":1,"deleted":[]}}`); status != 400 {
		t.Fatalf("a GitHub receipt naming a snapshot answered %d", status)
	}
	receipt := `{"lease":"` + lease.ID + `","project":"notes","kind":"dropbox","generation":1,"revision":"` + sha + `","snapshot":{"name":"notes-20261127T100000Z-aaaaaaaaaaaa.bundle.enc","sha256":"` + strings.Repeat("b", 64) + `","size":2048,"kept":2,"deleted":["notes-20261020T100000Z-bbbbbbbbbbbb.bundle.enc"]}}`
	if status, raw = call("POST", "receipt", receipt); status != 200 || !strings.Contains(raw, `"lastSnapshot":"notes-20261127T100000Z-aaaaaaaaaaaa.bundle.enc"`) {
		t.Fatalf("receipt %d: %s", status, raw)
	}
	if status, _ = call("POST", "check", `{"lease":"`+lease.ID+`"}`); status != 403 {
		t.Fatalf("a completed lease checked %d", status)
	}
	if status, _ = call("POST", "drain", `{"project":"notes","kind":"dropbox","generation":1,"handoff":"`+strings.Repeat("c", 32)+`"}`); status != 403 {
		t.Fatalf("a drain without a handoff answered %d", status)
	}
	if status, _ = call("POST", "lease", `{"project":"notes","kind":"dropbox","generation":1,"revision":"`+sha+`","token":"x"}`); status != 400 {
		t.Fatalf("a lease with an unknown field answered %d", status)
	}
}
