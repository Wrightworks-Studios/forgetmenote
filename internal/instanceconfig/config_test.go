package instanceconfig

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSectionedConfigurationIsStrictAndCannotMixLegacyInputs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "instance.json")
	for _, raw := range []string{`{"version":2}`, `{"version":1,"unknown":true}`, `{"version":1,"ai":{"chat":{"unknown":true}}}`, `{"version":1} {}`, `null`} {
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Fatalf("accepted invalid configuration %s", raw)
		}
	}
	if err := os.WriteFile(path, []byte(`{"version":1,"server":{"headless":false},"integrations":{"data":{"version":1,"connections":[]}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	lookup := func(name string) string {
		if name == "INSTANCE_CONFIG_FILE" {
			return path
		}
		return ""
	}
	resolved, err := Resolve(lookup)
	if err != nil || resolved.Legacy || resolved.Config.Integrations.Data == nil || resolved.Config.Server.Headless == nil || *resolved.Config.Server.Headless {
		t.Fatalf("sectioned configuration: %+v %v", resolved, err)
	}
	for _, name := range LegacyEnvironment {
		mixed := func(key string) string {
			if key == name {
				return "private setting"
			}
			return lookup(key)
		}
		if _, err := Resolve(mixed); err == nil || !strings.Contains(err.Error(), name) {
			t.Fatalf("legacy %s silently merged: %v", name, err)
		}
	}
}

func TestMigrationRoundTripKeepsInlineSectionsAndSecretReferences(t *testing.T) {
	dir := t.TempDir()
	data := filepath.Join(dir, "data.json")
	if err := os.WriteFile(data, []byte(`{"version":1,"connections":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	values := map[string]string{"DATA_CONNECTIONS_CONFIG_FILE": data, "PUBLIC_URL": "http://localhost:8080", "HEADLESS": "true", "PROXY_SECRET_FILE": filepath.Join(dir, "proxy-secret"), "TRUSTED_PROXY_CIDRS": "192.0.2.0/24", "EMBEDDING_URL": "http://localhost:11434/api/embed"}
	legacy, err := FromLegacy(func(key string) string { return values[key] })
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "instance.json")
	if err := Migrate(func(key string) string { return values[key] }, dir, output); err != nil {
		t.Fatal(err)
	}
	current, err := Load(output)
	if err != nil || !reflect.DeepEqual(current, legacy.Config) {
		t.Fatalf("migration changed configuration: %v", err)
	}
	if err := Migrate(func(key string) string { return values[key] }, dir, output); err == nil {
		t.Fatal("existing configuration overwritten")
	}
}

func TestLegacyNullConfigurationCannotSilentlyDisableAFeature(t *testing.T) {
	path := filepath.Join(t.TempDir(), "null.json")
	if err := os.WriteFile(path, []byte("null"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"CHAT_CONFIG_FILE", "DECISION_CONFIG_FILE", "DATA_CONNECTIONS_CONFIG_FILE", "PERSONAL_CONNECTIONS_CONFIG_FILE", "AUTOMATIONS_CONFIG_FILE", "SLACK_CONFIG_FILE", "REPOSITORY_APP_CONFIG_FILE", "BACKUP_APP_CONFIG_FILE"} {
		if _, err := FromLegacy(func(key string) string {
			if key == name {
				return path
			}
			return ""
		}); err == nil {
			t.Fatalf("null %s silently disabled its configuration", name)
		}
	}
}

func TestDockerPythonDefaultDoesNotConflictWithSectionedConfiguration(t *testing.T) {
	file := filepath.Join(t.TempDir(), "instance.json")
	for _, configured := range []string{"", "/private/operator-python"} {
		config := `{"version":1,"integrations":{"python":"` + configured + `"}}`
		if err := os.WriteFile(file, []byte(config), 0600); err != nil {
			t.Fatal(err)
		}
		resolved, err := Resolve(func(key string) string {
			switch key {
			case "INSTANCE_CONFIG_FILE":
				return file
			case "AUTOMATION_PYTHON":
				return "/usr/local/bin/python3"
			}
			return ""
		})
		want := configured
		if want == "" {
			want = "/usr/local/bin/python3"
		}
		if err != nil || resolved.Config.Integrations.Python != want {
			t.Fatalf("inherited Docker runtime rejected or displaced explicit section: got=%q want=%q err=%v", resolved.Config.Integrations.Python, want, err)
		}
	}
}

func TestOutgoingModeIsAServerSetting(t *testing.T) {
	legacy, err := FromLegacy(func(key string) string {
		if key == "OUTGOING" {
			return "paused"
		}
		return ""
	})
	if err != nil || legacy.Config.Server.Outgoing != "paused" {
		t.Fatalf("legacy OUTGOING was not read: %+v %v", legacy.Config.Server, err)
	}
	path := filepath.Join(t.TempDir(), "instance.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"server":{"outgoing":"paused"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	sectioned, err := Load(path)
	if err != nil || sectioned.Server.Outgoing != "paused" {
		t.Fatalf("sectioned server.outgoing was not read: %+v %v", sectioned.Server, err)
	}
	// A copy configured both ways is refused rather than merged.
	if _, err := Resolve(func(key string) string {
		switch key {
		case "INSTANCE_CONFIG_FILE":
			return path
		case "OUTGOING":
			return "paused"
		}
		return ""
	}); err == nil || !strings.Contains(err.Error(), "OUTGOING") {
		t.Fatalf("OUTGOING merged with INSTANCE_CONFIG_FILE: %v", err)
	}
}

// Notebook owners are gone: an owner map, in either form, refuses with what
// to do rather than reading as an unknown setting or being ignored.
func TestRetiredNotebookOwnerMapIsRefused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "instance.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"notebooks":{"owners":{"alpha":{"url":"https://owner.example.test","secretFile":"/run/owner.secret"}}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "notebooks.owners") || !strings.Contains(err.Error(), "removed") {
		t.Fatalf("an instance configuration with notebook owners: %v", err)
	}
	owners := filepath.Join(dir, "owners.json")
	if err := os.WriteFile(owners, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"version":1}`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, sectioned := range []bool{false, true} {
		lookup := func(name string) string {
			switch {
			case name == "OWNER_MAP_FILE":
				return owners
			case name == "INSTANCE_CONFIG_FILE" && sectioned:
				return path
			}
			return ""
		}
		if _, err := Resolve(lookup); err == nil || !strings.Contains(err.Error(), "OWNER_MAP_FILE") || !strings.Contains(err.Error(), "removed") {
			t.Fatalf("OWNER_MAP_FILE (sectioned %v): %v", sectioned, err)
		}
	}
}
