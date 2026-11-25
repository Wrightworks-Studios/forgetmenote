package web

import (
	"encoding/json"
	"fmt"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"forgetmenote/internal/connectors"
	"forgetmenote/internal/instanceconfig"
)

func TestInstanceConfigurationSummaryMatchesAdministrationWithoutCredentials(t *testing.T) {
	app, _ := fixture(t)
	config := instanceconfig.Config{Version: 1, Integrations: instanceconfig.Integrations{Data: &connectors.Config{Version: 1, Connections: []connectors.Connection{}}}}
	if err := app.ConfigureInstance(instanceconfig.Resolved{Config: config}); err != nil {
		t.Fatal(err)
	}
	w := call(app, "GET", "/api/admin", "", testToken)
	var result struct {
		Instance map[string]bool `json:"instance"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || !reflect.DeepEqual(result.Instance, app.InstanceSummary()) || !result.Instance["dataConnections"] || result.Instance["chat"] {
		t.Fatalf("incorrect Administration summary: %d %s", w.Code, w.Body.String())
	}
	for _, value := range []string{"secretFile", "baseURL", "signingSecret", "botToken"} {
		if strings.Contains(w.Body.String(), value) {
			t.Fatalf("private operator configuration disclosed: %s", value)
		}
	}
	if err := app.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := app.ConfigureInstance(instanceconfig.Resolved{Config: config}); err == nil {
		t.Fatal("configuration accepted after request admission")
	}
}

func TestWebsiteCaptureCredentialsCannotLiveInNotebooks(t *testing.T) {
	for _, field := range []string{"account", "token"} {
		for _, symlink := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/symlink=%v", field, symlink), func(t *testing.T) {
				app, _ := fixture(t)
				store, err := app.catalog.Access(catalog.Bootstrap, "default", "read")
				if err != nil {
					t.Fatal(err)
				}
				account := filepath.Join(t.TempDir(), "account")
				token := filepath.Join(t.TempDir(), "token")
				os.WriteFile(account, []byte(strings.Repeat("a", 32)), 0600)
				os.WriteFile(token, []byte("synthetic-token"), 0600)
				// A notebook's files are on disk only in the working tree it
				// had before the cutover.
				inside := filepath.Join(previousReleaseTree(t, store), "capture-credential.txt")
				if err := os.WriteFile(inside, []byte("synthetic-private"), 0600); err != nil {
					t.Fatal(err)
				}
				chosen := inside
				if symlink {
					chosen = filepath.Join(t.TempDir(), "link")
					if err := os.Symlink(inside, chosen); err != nil {
						t.Fatal(err)
					}
				}
				if field == "account" {
					account = chosen
				} else {
					token = chosen
				}
				config := instanceconfig.Config{Version: 1, AI: instanceconfig.AI{Chat: &chat.Config{WebsiteCapture: &chat.WebsiteCaptureConfig{AccountIDFile: account, SecretFile: token}}}}
				if err := app.ConfigureInstance(instanceconfig.Resolved{Config: config}); err == nil || !strings.Contains(err.Error(), "outside notebook") {
					t.Fatalf("notebook credential accepted: %v", err)
				}
			})
		}
	}
}
