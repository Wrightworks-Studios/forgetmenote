package web

import (
	"encoding/json"
	"reflect"
	"testing"

	"forgetmenote/internal/instanceconfig"
	"forgetmenote/internal/modules"
)

// Administration's instance status lists every module and whether it is on,
// beside the integrations the build leaves out.
func TestOrganizationStatusListsTheModules(t *testing.T) {
	app, _ := fixture(t)
	if err := app.ConfigureInstance(instanceconfig.Resolved{Config: instanceconfig.Config{Version: 1, Modules: instanceconfig.Modules{Off: []string{}}}}); err != nil {
		t.Fatal(err)
	}
	admin := recoverySession(t, app)
	w := localRequest(app, "/api/organization/action/organization_status", `{}`, admin)
	var status struct {
		Modules []modules.State `json:"modules"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &status) != nil || len(status.Modules) != len(modules.All()) || !reflect.DeepEqual(status.Modules, modules.Set{}.States()) {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, module := range status.Modules {
		if !module.Enabled || module.Title == "" {
			t.Fatal(module)
		}
	}
}
