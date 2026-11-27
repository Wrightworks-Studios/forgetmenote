package web

import (
	"encoding/json"
	"slices"
	"testing"

	"forgetmenote/internal/modules"
	"forgetmenote/internal/pg/pgtest"
)

// Each gateway records the modules it turns off. Administration lists every
// live gateway with its set, this one first, and says which differ; a gateway
// that stops drops out.
func TestAdministrationListsTheModulesEachGatewayTurnsOff(t *testing.T) {
	set, err := modules.Parse([]string{"judgments"})
	if err != nil {
		t.Fatal(err)
	}
	app, _ := fixtureWithModules(t, set)
	status := func(s *Server) []gatewayModules {
		t.Helper()
		w := apiCall(s, "POST", "/api/v1/actions/organization_status", `{}`, testToken)
		var out struct {
			Gateways []gatewayModules `json:"gateways"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatalf("organization_status: HTTP %d %s", w.Code, w.Body)
		}
		return out.Gateways
	}
	if got := status(app); len(got) != 1 || got[0].Gateway != app.db.Holder || !got[0].This || !got[0].Same || !slices.Equal(got[0].Off, []string{"judgments"}) {
		t.Fatalf("one gateway: %+v", got)
	}

	db, leave := pgtest.Gateway(t, app.db)
	other, err := NewWithBlobs(app.catalog, db, app.blobs, modules.Set{}, t.TempDir(), app.origin, app.token)
	if err != nil {
		t.Fatal(err)
	}
	if got := status(app); len(got) != 2 || !got[0].This || !got[0].Same || got[1].Gateway != db.Holder || got[1].This || got[1].Same || len(got[1].Off) != 0 {
		t.Fatalf("seen from the first gateway: %+v", got)
	}
	if got := status(other); len(got) != 2 || got[0].Gateway != db.Holder || !got[0].Same || got[1].Gateway != app.db.Holder || got[1].Same {
		t.Fatalf("seen from the second gateway: %+v", got)
	}
	other.Close()
	leave()
	if got := status(app); len(got) != 1 || !got[0].This {
		t.Fatalf("after the second gateway stopped: %+v", got)
	}
}
