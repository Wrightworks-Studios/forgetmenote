package catalog

import (
	"errors"
	"testing"

	"forgetmenote/internal/pg/pgtest"
	"forgetmenote/internal/workspace"
)

func TestAISettingsPersistWithConflictAndFailedSaveProtection(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(dir, nil, testDB(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	settings, managed := c.AISettings()
	if managed || settings.Embeddings {
		t.Fatal("new instance enabled AI")
	}
	if _, err = c.SetAISettings("stranger", settings); !errors.Is(err, workspace.ErrForbidden) {
		t.Fatal(err)
	}
	settings.Embeddings = true
	saved, err := c.SetAISettings(Bootstrap, settings)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.SetAISettings(Bootstrap, settings); !errors.Is(err, workspace.ErrConflict) {
		t.Fatal(err)
	}
	c.Close()
	c, err = Open(dir, nil, testDB(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if got, ok := c.AISettings(); !ok || got != saved {
		t.Fatalf("policy lost after reopen: %+v", got)
	}
	// A failed catalog write must not change the in-memory policy either.
	pgtest.FailWrites(t, c.db, "catalog")
	saved.Embeddings = false
	if _, err = c.SetAISettings(Bootstrap, saved); err == nil {
		t.Fatal("save unexpectedly succeeded")
	}
	if got, _ := c.AISettings(); !got.Embeddings || got.Revision != saved.Revision {
		t.Fatal("failed save changed policy")
	}
}

func TestAISettingsRejectInvalidPersistedPolicy(t *testing.T) {
	dir := t.TempDir()
	storeCatalog(t, testDB(t, dir), `{"version":10,"data":{"users":{},"projects":{},"ai":{"revision":1,"embeddings":true,"model":"","dimensions":0}}}`)
	c, err := Open(dir, nil, testDB(t, dir))
	if err == nil {
		c.Close()
		t.Fatal("invalid persisted AI policy was accepted")
	}
}

func TestConnectionGrantPreservesAdoptedAIPolicyAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(dir, nil, testDB(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	settings, _ := c.AISettings()
	settings.Embeddings = true
	saved, err := c.SetAISettings(Bootstrap, settings)
	if err != nil {
		c.Close()
		t.Fatal(err)
	}
	_, err = c.SetConnectionAccess(Bootstrap, ConnectionGrant{Connection: "warehouse"})
	c.Close()
	if err != nil {
		t.Fatal(err)
	}
	c, err = Open(dir, nil, testDB(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if got, managed := c.AISettings(); !managed || got != saved {
		t.Fatalf("AI policy changed after granting connection access: %+v", got)
	}
	if grant, err := c.ConnectionAccess(Bootstrap, "warehouse"); err != nil || grant.Revision != 1 {
		t.Fatalf("connection grant lost: %+v, %v", grant, err)
	}
}

// A policy an earlier release stored with hosted source OCR and descriptions
// on loads without them, and they do not turn on the chat's reading of
// uploaded images: that needs a fresh, explicit opt-in.
func TestEarlierHostedSourcePolicyDoesNotTurnOnUploadReading(t *testing.T) {
	dir := t.TempDir()
	storeCatalog(t, testDB(t, dir), `{"version":10,"data":{"users":{},"projects":{},"ai":{"revision":3,"model":"text-embedding-3-small","dimensions":1536,"writing":true,"sourceOCR":true,"sourceDescriptions":true,"sourceTranscription":true}}}`)
	c, err := Open(dir, nil, testDB(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	got, managed := c.AISettings()
	if !managed || got.Revision != 3 || !got.Writing || got.UploadReading {
		t.Fatalf("policy: %+v managed=%v", got, managed)
	}
}
