package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/pg/pgtest"
	"forgetmenote/internal/workspace"
)

func TestCLIAdminBootstrapAndPrivateCredential(t *testing.T) {
	t.Setenv("OIDC_ISSUER_URL", "")
	t.Setenv("PROXY_SECRET_FILE", "")
	data := t.TempDir()
	db := instanceDatabase(t, databaseURLFile(t, true))
	var out bytes.Buffer
	args := []string{"bootstrap", "--data", data, "--username", "operator", "--password-stdin"}
	if err := runAdmin(args, strings.NewReader("fixture-long-password\n"), &out); err != nil {
		t.Fatal(err)
	}
	var boot struct{ User string }
	if err := json.Unmarshal(out.Bytes(), &boot); err != nil || boot.User == "" {
		t.Fatal(out.String(), err)
	}
	if err := runAdmin(args, strings.NewReader("another-long-password"), &out); err == nil {
		t.Fatal("bootstrap replaced an administrator")
	}
	target := filepath.Join(t.TempDir(), "client.secret")
	out.Reset()
	credential := []string{"credential", "--data", data, "--user", boot.User, "--projects", "default", "--output", target}
	if err := runAdmin(credential, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	secret, err := os.ReadFile(target)
	if err != nil || !strings.HasPrefix(string(secret), "fmn_") || strings.Contains(out.String(), strings.TrimSpace(string(secret))) {
		t.Fatalf("private output: %v %s", err, out.String())
	}
	st, _ := os.Stat(target)
	if st.Mode().Perm() != 0600 {
		t.Fatal("credential file is not private")
	}
	alias := filepath.Join(t.TempDir(), "data")
	if err = os.Symlink(data, alias); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(data, "projects", "default", "workspace", "credential.md")
	if err = runAdmin([]string{"credential", "--data", alias, "--user", boot.User, "--projects", "default", "--output", inside}, strings.NewReader(""), &out); err == nil {
		t.Fatal("symlinked data directory allowed a secret inside notebook Git")
	}
	if _, err = os.Stat(inside); !os.IsNotExist(err) {
		t.Fatal("credential was written inside notebook", err)
	}
	c, err := catalog.Open(data, nil, db)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err = runAdmin(credential, strings.NewReader(""), &out); err == nil {
		t.Fatal("issued credential while instance held catalog")
	}
}
func TestCLIHelpDoesNotInitializeServer(t *testing.T) {
	data := filepath.Join(t.TempDir(), "must-not-exist")
	t.Setenv("DATA_DIR", data)
	if err := runArguments([]string{"help"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(data); !os.IsNotExist(err) {
		t.Fatal("client opened server data directory")
	}
}

func TestRetiredStorageRollbackRefusesAndRetainsPendingEdits(t *testing.T) {
	dir := t.TempDir()
	c, err := catalog.Open(dir, nil, pgtest.Dir(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	store, err := c.Access(catalog.Bootstrap, "default", "edit")
	if err != nil {
		c.Close()
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err = store.Save(ctx, "pending.md", []byte("before"), "", ""); err != nil {
		c.Close()
		t.Fatal(err)
	}
	base, err := store.LiveRead(ctx, "pending.md")
	if err != nil {
		c.Close()
		t.Fatal(err)
	}
	accepted, err := store.LivePatch(ctx, workspace.TargetedPatch{Path: base.Path, Base: base.LiveRevision, OperationID: "rollback", Edits: []workspace.TextEdit{{Start: 0, End: 6, Old: "before", Text: "retained"}}})
	if err != nil {
		c.Close()
		t.Fatal(err)
	}
	var output bytes.Buffer
	args := []string{"rollback-storage", "--data", dir}
	if err := runAdmin(args, strings.NewReader(""), &output); err == nil {
		c.Close()
		t.Fatal("rollback accessed running owner")
	}
	c.Close()
	// A file-based release cannot read the Postgres catalog, so rollback refuses.
	if err := runAdmin(args, strings.NewReader(""), &output); err == nil || !strings.Contains(err.Error(), "Postgres") {
		t.Fatal("retired storage rollback ran:", err)
	}
	c, err = catalog.Open(dir, nil, pgtest.Dir(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	store, err = c.Access(catalog.Bootstrap, "default", "read")
	if err != nil {
		t.Fatal(err)
	}
	actual, err := store.LiveRead(ctx, base.Path)
	if err != nil || actual.Content != accepted.Content || actual.LiveRevision != accepted.LiveRevision || !actual.Pending {
		t.Fatal("rollback changed pending edits", err)
	}
}

func TestStorageRollbackDoesNotProvisionMissingData(t *testing.T) {
	databaseURLFile(t, true)
	for _, command := range []string{"rollback-storage", "rollback-live-storage", "rollback-semantic-storage"} {
		t.Run(command, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "missing")
			var out bytes.Buffer
			if err := runAdmin([]string{command, "--data", dir}, strings.NewReader(""), &out); err == nil {
				t.Fatal("rollback provisioned replacement storage")
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Fatal("rollback changed nonexistent target", err)
			}
		})
	}
}

// Vectors live in Postgres, so a file-based release has nothing to read and the
// semantic rollback refuses as storage rollback does.
func TestSemanticStorageRollbackIsRetired(t *testing.T) {
	var out bytes.Buffer
	if err := runAdmin([]string{"rollback-semantic-storage", "--data", t.TempDir()}, strings.NewReader(""), &out); err == nil || !strings.Contains(err.Error(), "Postgres") {
		t.Fatal("retired semantic rollback ran:", err)
	}
}

// Notebook owners are gone: their command and the move between them refuse
// with why, before opening or creating anything.
func TestRetiredOwnerCommandsExplainTheirRemoval(t *testing.T) {
	data := filepath.Join(t.TempDir(), "missing")
	var out bytes.Buffer
	if err := runAdmin([]string{"move-notebook", "--data", data, "--notebook", "default", "--owner", "spare"}, strings.NewReader(""), &out); err == nil || !strings.Contains(err.Error(), "removed") {
		t.Fatalf("admin move-notebook: %v", err)
	}
	t.Setenv("DATA_DIR", data)
	if err := runArguments([]string{"owner"}); err == nil || !strings.Contains(err.Error(), "removed") {
		t.Fatalf("the owner command: %v", err)
	}
	if _, err := os.Stat(data); !os.IsNotExist(err) {
		t.Fatal("a retired command created the data directory", err)
	}
}

// The backup command bundles a notebook from the notes store through its Git
// export, as the running instance does.
func TestBackupCommandBundlesTheNotebooksHistory(t *testing.T) {
	data := t.TempDir()
	db := instanceDatabase(t, databaseURLFile(t, true))
	c, err := catalog.Open(data, nil, db)
	if err != nil {
		t.Fatal(err)
	}
	store, err := c.Access(catalog.Bootstrap, "default", "edit")
	if err != nil {
		c.Close()
		t.Fatal(err)
	}
	saved, err := store.Save(t.Context(), "kept.md", []byte("# Kept\n"), "", "test")
	c.Close()
	if err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(t.TempDir(), "default.bundle")
	if err = runArguments([]string{"-data", data, "backup", bundle}); err != nil {
		t.Fatal(err)
	}
	if heads := gitRun(t, t.TempDir(), "bundle", "list-heads", bundle); !strings.Contains(heads, saved.Revision) {
		t.Fatalf("the backup does not hold the notebook's head %s: %s", saved.Revision, heads)
	}
}
