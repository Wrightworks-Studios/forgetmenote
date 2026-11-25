package web

import (
	"encoding/json"
	"path"
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

// requireLocalOCR skips a case whose source needs local OCR.
func requireLocalOCR(t *testing.T) {
	t.Helper()
	if err := integrations.Check("ocr"); err != nil {
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
