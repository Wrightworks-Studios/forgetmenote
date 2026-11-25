package web

import (
	"testing"

	"forgetmenote/internal/auth"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/pg/pgtest"
)

func TestMain(m *testing.M) {
	catalog.UseTestPasswordCost()
	pgtest.Main(m)
}

// reopenOAuth opens the server's OAuth state again, as a restarted gateway does.
func reopenOAuth(app *Server) (*oauthState, error) { return auth.OpenState(app.db) }
