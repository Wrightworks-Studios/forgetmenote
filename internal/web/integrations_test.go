package web

import (
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"forgetmenote/internal/integrations"
	"forgetmenote/internal/sources"
)

// offeredSource reports whether this build imports a source with the name.
func offeredSource(filename string) bool {
	extension := strings.ToLower(path.Ext(filename))
	for _, format := range sources.Formats() {
		if slices.Contains(format.Extensions, extension) {
			return true
		}
	}
	return false
}

// requireSlack skips a case that configures a Slack app.
func requireSlack(t *testing.T) {
	t.Helper()
	if err := integrations.Check("slack"); err != nil {
		t.Skip(err)
	}
}

// Administration names the optional integrations this build leaves out, and
// none for the full build.
func TestOrganizationStatusNamesOmittedIntegrations(t *testing.T) {
	app, _ := fixture(t)
	admin := recoverySession(t, app)
	w := localRequest(app, "/api/organization/action/organization_status", `{}`, admin)
	var status struct {
		Omitted []integrations.Integration `json:"omittedIntegrations"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &status) != nil || status.Omitted == nil || !reflect.DeepEqual(status.Omitted, integrations.Omitted()) {
		t.Fatal(w.Code, w.Body.String())
	}
}

// A build without Slack refuses a Slack app configuration by name, so the
// instance starts with no Slack; the full build configures the same file.
func TestSlackConfigurationNeedsItsIntegration(t *testing.T) {
	app := chatFixture(t)
	dir := t.TempDir()
	signing, bot := filepath.Join(dir, "signing"), filepath.Join(dir, "bot")
	for _, file := range []string{signing, bot} {
		if err := os.WriteFile(file, []byte("fixture-secret"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	err := app.ConfigureSlackConfig(SlackConfig{Team: "T123", SigningSecretFile: signing, BotTokenFile: bot})
	if integrations.Included("slack") {
		if err != nil || app.slack == nil {
			t.Fatal(err)
		}
		return
	}
	if err == nil || !strings.Contains(err.Error(), "fmn_omit_slack") || app.slack != nil {
		t.Fatal(err)
	}
}
