package main

import (
	"bytes"
	"encoding/json"
	"forgetmenote/internal/catalog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMigrateInstanceConfigurationRetainsSectionsAndRefusesOverwrite(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "connections.json")
	if err := os.WriteFile(legacy, []byte(`{"version":1,"connections":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("INSTANCE_CONFIG_FILE", "")
	t.Setenv("DATA_CONNECTIONS_CONFIG_FILE", legacy)
	destination := filepath.Join(dir, "instance.json")
	var output bytes.Buffer
	if err := runAdmin([]string{"migrate-instance-config", "--data", filepath.Join(dir, "data"), "--output", destination}, bytes.NewReader(nil), &output); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Version      int `json:"version"`
		Integrations struct {
			Data json.RawMessage `json:"data"`
		} `json:"integrations"`
	}
	if json.Unmarshal(raw, &config) != nil || config.Version != 1 || !bytes.Contains(config.Integrations.Data, []byte(`"connections": []`)) {
		t.Fatalf("migration lost typed configuration: %s", raw)
	}
	info, err := os.Stat(destination)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("migration permissions: %v %v", info, err)
	}
	if bytes.Contains(output.Bytes(), []byte("connections.json")) || bytes.Contains(output.Bytes(), []byte(`"connections"`)) {
		t.Fatal("migration disclosed private configuration")
	}
	if err := runAdmin([]string{"migrate-instance-config", "--data", filepath.Join(dir, "data"), "--output", destination}, bytes.NewReader(nil), &output); err == nil {
		t.Fatal("existing operator configuration overwritten")
	}
	after, _ := os.ReadFile(destination)
	if !bytes.Equal(after, raw) {
		t.Fatal("failed overwrite changed configuration")
	}
}

func TestSectionedIdentityPreventsLocalAdministratorBootstrap(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "instance.json")
	if err := os.WriteFile(file, []byte(`{"version":1,"identity":{"proxy":{"secretFile":"/private/proxy-secret","trustedCidrs":["192.0.2.0/24"]}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("INSTANCE_CONFIG_FILE", file)
	t.Setenv("OIDC_ISSUER_URL", "")
	t.Setenv("PROXY_SECRET_FILE", "")
	data := filepath.Join(dir, "must-not-open")
	var output bytes.Buffer
	err := runAdmin([]string{"bootstrap", "--data", data, "--username", "operator", "--password-stdin"}, strings.NewReader("synthetic-fixture-password"), &output)
	if err == nil || !strings.Contains(err.Error(), "external authentication disabled") {
		t.Fatalf("sectioned external auth bypassed: %v", err)
	}
	if _, err := os.Stat(data); !os.IsNotExist(err) {
		t.Fatal("refused bootstrap initialized the catalog")
	}
}

func TestLegacyBackupCommandUsesSectionedDataUnlessCLIOverrides(t *testing.T) {
	dir := t.TempDir()
	data := filepath.Join(dir, "sectioned-data")
	database := writeDatabaseURL(t, true)
	c, err := catalog.Open(data, nil, instanceDatabase(t, database))
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	file := filepath.Join(dir, "instance.json")
	raw, _ := json.Marshal(map[string]any{"version": 1, "server": map[string]any{"data": data, "database": map[string]string{"urlFile": database}}})
	if err := os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("INSTANCE_CONFIG_FILE", file)
	wrong := filepath.Join(dir, "wrong-default")
	t.Setenv("DATA_DIR", wrong)
	if err := runArguments([]string{"-url", "http://localhost:8080", "backup", filepath.Join(dir, "snapshot.bundle")}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(wrong); !os.IsNotExist(err) {
		t.Fatal("legacy backup opened the environment default instead of sectioned data")
	}
	if err := runArguments([]string{"-data", data, "backup", filepath.Join(dir, "explicit.bundle")}); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryTokenUsesValidSectionedDataAndRequiresExplicitDataOnBadConfig(t *testing.T) {
	dir := t.TempDir()
	data := filepath.Join(dir, "configured")
	if err := os.Mkdir(data, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "admin.token"), []byte("SYNTHETIC_CONFIGURED_RECOVERY_TOKEN"), 0600); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "instance.json")
	raw, _ := json.Marshal(map[string]any{"version": 1, "server": map[string]string{"data": data}})
	if err := os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("INSTANCE_CONFIG_FILE", file)
	wrong := filepath.Join(dir, "unrelated-default")
	t.Setenv("DATA_DIR", wrong)
	// The caller-visible proof is which data directory is opened. The command's
	// stdout is redirected privately; no recovery key is exposed in test output.
	output, err := os.CreateTemp(dir, "token-output-")
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	previous := os.Stdout
	os.Stdout = output
	err = runArguments([]string{"token"})
	os.Stdout = previous
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(wrong); !os.IsNotExist(err) {
		t.Fatal("recovery token came from another data directory")
	}
	if err := os.WriteFile(file, []byte(`{"version":1,"unknown":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := runArguments([]string{"token"}); err == nil || !strings.Contains(err.Error(), "-data") {
		t.Fatal("invalid config silently selected a different recovery directory")
	}
	os.Stdout = output
	err = runArguments([]string{"-data", data, "token"})
	os.Stdout = previous
	if err != nil {
		t.Fatalf("explicit recovery could not bypass broken runtime config: %v", err)
	}
}
