package web

import (
	"os"
	"testing"
	"time"

	"forgetmenote/internal/auth"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/pg/pgtest"
)

func TestMain(m *testing.M) {
	// Run in UTC, as the application's containers do. Commits take their zone
	// offset from time.Local, and a test's t.Setenv("TZ") cannot change a zone
	// that an earlier test in the process already loaded, so the UTC goldens
	// and TestMCPContractCommitTimes otherwise depend on which tests ran first.
	// Git subprocesses read TZ.
	time.Local = time.UTC
	os.Setenv("TZ", "UTC")
	catalog.UseTestPasswordCost()
	pgtest.Main(m)
}

// reopenOAuth opens the server's OAuth state again, as a restarted gateway does.
func reopenOAuth(app *Server) (*oauthState, error) { return auth.OpenState(app.db) }
