package web

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"forgetmenote/internal/backups"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/githubapp"
	"forgetmenote/internal/instanceconfig"
	"forgetmenote/internal/pg/pgtest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSectionedBackupCredentialsCannotUseNotebookThroughDataSymlink(t *testing.T) {
	dir := t.TempDir()
	actual := filepath.Join(dir, "actual")
	if err := os.Mkdir(actual, 0700); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(dir, "data-alias")
	if err := os.Symlink(actual, data); err != nil {
		t.Fatal(err)
	}
	c, err := catalog.Open(data, nil, pgtest.Dir(t, data))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	app, err := New(c, pgtest.Org(t), data, "http://localhost", testToken)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	store, err := c.Access(catalog.Bootstrap, "default", "edit")
	if err != nil {
		t.Fatal(err)
	}
	// A notebook's files are on disk only in the working tree it had before
	// the cutover, reached here through the data directory's symlink.
	const workerSecret = "synthetic-backup-worker-token-in-readable-note"
	worker := filepath.Join(previousReleaseTree(t, store), "notes", "worker-credential.md")
	if err := os.MkdirAll(filepath.Dir(worker), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(worker, []byte(workerSecret), 0600); err != nil {
		t.Fatal(err)
	}
	if err := app.privateRuntimeFile(worker); err == nil || !strings.Contains(err.Error(), "outside notebook") {
		t.Fatalf("fixture is not within guarded notebook: %v", err)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyfile := filepath.Join(dir, "app.pem")
	client := filepath.Join(dir, "client-secret")
	webhook := filepath.Join(dir, "webhook-secret")
	for file, raw := range map[string][]byte{keyfile: pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), client: []byte("synthetic-client-secret"), webhook: []byte(strings.Repeat("w", 32))} {
		if err := os.WriteFile(file, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	config := instanceconfig.Config{Version: 1, Integrations: instanceconfig.Integrations{Backups: &backups.AppConfig{App: githubapp.Config{AppID: "123", Slug: "fixture", ClientID: "synthetic", ClientSecretFile: client, PrivateKeyFile: keyfile, WebhookSecretFile: webhook}, Workers: map[string]backups.WorkerConfig{"fixture": {Owner: "local", TokenFile: worker}}}}}
	if err := app.ConfigureInstance(instanceconfig.Resolved{Config: config}); err == nil || !strings.Contains(err.Error(), "outside notebook") {
		t.Fatalf("sectioned backup installed worker authority from a readable notebook credential through data symlink: %v", err)
	}
}
