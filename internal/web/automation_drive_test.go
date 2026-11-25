package web

import (
	"encoding/json"
	"forgetmenote/internal/automation"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/connectors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDriveSyncPreviewIncrementalAndEditedSourceConflict(t *testing.T) {
	for _, filename := range []string{"guide.txt", "guide"} {
		t.Run(filename, func(t *testing.T) {
			app, store := fixture(t)
			version := "1"
			downloads := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/list" {
					json.NewEncoder(w).Encode(map[string]any{"files": []any{map[string]string{"id": "file1", "name": filename, "mimeType": "text/plain", "version": version, "modifiedTime": "2026-10-01T00:00:00Z"}}})
					return
				}
				downloads++
				w.Write([]byte("Source version " + version))
			}))
			defer server.Close()
			schema := json.RawMessage(`{"type":"object","additionalProperties":true}`)
			manager, err := connectors.New(connectors.Config{Version: 1, Connections: []connectors.Connection{{ID: "drive", Kind: "http", Endpoint: server.URL, Operations: []connectors.Operation{{Name: "list", Path: "/list", ReadOnly: true, InputSchema: schema}, {Name: "download", Path: "/download", ReadOnly: true, Response: "file", InputSchema: schema}}}}})
			if err != nil {
				t.Fatal(err)
			}
			app.dataConnections = manager
			spec := automation.Spec{ID: "drive", Kind: "drive-sync", Actor: catalog.Bootstrap, Project: "default", Connections: []string{"drive"}, Drive: &automation.DriveSync{Connection: "drive", Folders: []string{"root"}, Destination: "drive"}}
			run := func(preview bool) error {
				r := automation.Run{ID: automation.Hash(time.Now().String()), Spec: spec, DryRun: preview}
				return app.runAutomation(t.Context(), &r, func() error { return nil })
			}
			if err = spec.Validate(); err != nil {
				t.Fatal(err)
			}
			if err = run(true); err != nil {
				t.Fatal(err)
			}
			state, err := app.automations.Queue.ReadDrive(spec.ID)
			if err != nil || len(state.Files) != 0 {
				t.Fatal("preview persisted state", state, err)
			}
			if err = run(false); err != nil {
				t.Fatal(err)
			}
			state, err = app.automations.Queue.ReadDrive(spec.ID)
			if err != nil || len(state.Files) != 1 {
				t.Fatal(state, err)
			}
			if err = run(false); err != nil || downloads != 2 {
				t.Fatal("unchanged file downloaded again", err, downloads)
			}
			file := state.Files["file1"]
			doc, err := store.Read(t.Context(), file.Path, "")
			if err != nil {
				t.Fatal(err)
			}
			_, err = store.Save(t.Context(), doc.Path, append(doc.Content, []byte("\nHuman edit")...), doc.Revision, "test")
			if err != nil {
				t.Fatal(err)
			}
			version = "2"
			if err = run(false); err == nil {
				t.Fatal("overwrote human edit")
			}
			doc, err = store.Read(t.Context(), file.Path, "")
			if err != nil || !strings.Contains(string(doc.Content), "Human edit") {
				t.Fatal(err, string(doc.Content))
			}
		})
	}
}
