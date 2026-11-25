package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"forgetmenote/internal/blob"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/gitexport"
	"forgetmenote/internal/notestore"
	"forgetmenote/internal/pg/pgtest"
	"forgetmenote/internal/web"
	"forgetmenote/internal/workspace"
)

// An operator purges through the running instance with its access key: the
// text comes from a file, the plan goes to a private file that only execute
// reads and then deletes, and the receipt names the operator and reason.
func TestAdminPurgeRunsThroughTheRunningInstance(t *testing.T) {
	data := t.TempDir()
	projects, err := catalog.Open(data, nil, pgtest.Dir(t, data))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { projects.Close() })
	store, err := projects.Access(catalog.Bootstrap, "default", "edit")
	if err != nil {
		t.Fatal(err)
	}
	const secret = "sk-live-operator-0123456789abcdef"
	if _, err := store.Save(context.Background(), "notes/keys.md", []byte("# Keys\n\n"+secret+"\n"), "", "test"); err != nil {
		t.Fatal(err)
	}
	token := web.RandomToken()
	if err := os.WriteFile(filepath.Join(data, "admin.token"), []byte(token+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewUnstartedServer(nil)
	ts.Start()
	t.Cleanup(ts.Close)
	app, err := web.New(projects, pgtest.Org(t), data, ts.URL, token)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	if err := app.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	ts.Config.Handler = app
	t.Setenv("INSTANCE_CONFIG_FILE", "")
	text := filepath.Join(t.TempDir(), "secret.txt")
	os.WriteFile(text, []byte(secret+"\n"), 0600)
	plan := filepath.Join(t.TempDir(), "purge.plan")
	common := []string{"-data", data, "-server", ts.URL, "-origin", ts.URL, "-notebook", "default", "-plan", plan}

	var out bytes.Buffer
	if err := runAdmin(append([]string{"purge", "prepare", "-kind", "text", "-text-file", text}, common...), strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), secret) || strings.Contains(out.String(), "confirmation") || !strings.Contains(out.String(), "notes/keys.md") {
		t.Fatalf("prepare printed: %s", out.String())
	}
	if st, err := os.Stat(plan); err != nil || st.Mode().Perm() != 0600 {
		t.Fatal("the plan file is not private:", err)
	}
	if err := runAdmin(append([]string{"purge", "prepare", "-kind", "text", "-text", secret}, common...), strings.NewReader(""), &out); err == nil {
		t.Fatal("the text was accepted on the command line")
	}
	if err := runAdmin(append([]string{"purge", "execute", "-reason", "Leaked staging key"}, common...), strings.NewReader(""), &out); err == nil {
		t.Fatal("a purge ran without naming its operator")
	}
	// A refused reason keeps the plan file, which can still run, and says so.
	err = runAdmin(append([]string{"purge", "execute", "-reason", "Remove " + secret, "-operator", "Ops person"}, common...), strings.NewReader(""), &out)
	if err == nil || !strings.Contains(err.Error(), plan) || strings.Contains(err.Error(), secret) {
		t.Fatal("a refused reason:", err)
	}
	if _, err := os.Stat(plan); err != nil {
		t.Fatal("the plan file was removed:", err)
	}
	// A plan the notebook has moved past can never run, so its file, which
	// holds the text, is deleted.
	if _, err := store.Save(context.Background(), "notes/later.md", []byte("# Later\n"), "", "test"); err != nil {
		t.Fatal(err)
	}
	err = runAdmin(append([]string{"purge", "execute", "-reason", "Leaked staging key", "-operator", "Ops person"}, common...), strings.NewReader(""), &out)
	if err == nil || !strings.Contains(err.Error(), "deleted") {
		t.Fatal("a stale plan:", err)
	}
	if _, err := os.Stat(plan); !os.IsNotExist(err) {
		t.Fatal("the stale plan file, which holds the text, remains:", err)
	}
	if err := runAdmin(append([]string{"purge", "prepare", "-kind", "text", "-text-file", text}, common...), strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runAdmin(append([]string{"purge", "execute", "-reason", "Leaked staging key", "-operator", "Ops person"}, common...), strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	var receipt struct {
		Purge, Actor, Reason string
		Rewritten            int
	}
	if json.Unmarshal(out.Bytes(), &receipt) != nil || receipt.Purge == "" || !strings.Contains(receipt.Actor, "operator: Ops person") || receipt.Reason != "Leaked staging key" || strings.Contains(out.String(), secret) {
		t.Fatalf("receipt: %s", out.String())
	}
	if _, err := os.Stat(plan); !os.IsNotExist(err) {
		t.Fatal("the plan file, which holds the text, remains:", err)
	}
	doc, err := store.Read(context.Background(), "notes/keys.md", "")
	if err != nil || strings.Contains(string(doc.Content), secret) {
		t.Fatal("not purged:", err)
	}
	out.Reset()
	if err := runAdmin([]string{"purge", "list", "-data", data, "-server", ts.URL, "-origin", ts.URL, "-notebook", "default"}, strings.NewReader(""), &out); err != nil || !strings.Contains(out.String(), receipt.Purge) {
		t.Fatalf("list: %s %v", out.String(), err)
	}
}

// A purge whose export rebuild failed leaves the export behind the notebook;
// with the instance stopped, rebuild replaces it for that purge and flags it
// for the backup worker.
func TestAdminPurgeRebuildReplacesTheExportForAPurge(t *testing.T) {
	t.Setenv("INSTANCE_CONFIG_FILE", "")
	db := instanceDatabase(t, databaseURLFile(t, true))
	data := t.TempDir()
	ctx := context.Background()
	blobs, err := blob.OpenDir(data)
	if err != nil {
		t.Fatal(err)
	}
	notes, err := notestore.Open(ctx, db, blobs, notestore.Options{ImportEntryID: workspace.ImportEntryID})
	if err != nil {
		t.Fatal(err)
	}
	store, err := workspace.CreateDB(ctx, notes, "alpha", filepath.Join(data, "projects", "alpha"), nil)
	if err != nil {
		t.Fatal(err)
	}
	const secret = "sk-live-rebuild-0123456789"
	if _, err := store.Save(ctx, "keys.md", []byte("# Keys\n\n"+secret+"\n"), "", "test"); err != nil {
		t.Fatal(err)
	}
	exporter, err := gitexport.New(notes, filepath.Join(data, "exports"), gitexport.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exporter.Sync(ctx, "alpha"); err != nil {
		t.Fatal(err)
	}
	exporter.Close()
	plan, err := store.PreparePurge(ctx, workspace.PurgeScope{Kind: workspace.PurgeText, Text: secret}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// No export in this process: the purge cannot rebuild it.
	result, err := store.ExecutePurge(ctx, plan, plan.Confirmation, "operator", "leaked", nil)
	if err != nil || result.Export {
		t.Fatal(result, err)
	}
	store.Close()
	notes.Close()
	blobs.Close()
	var out bytes.Buffer
	if err := runAdmin([]string{"purge", "rebuild", "-data", data, "-notebook", "alpha", "-purge", result.Purge}, nil, &out); err != nil {
		t.Fatal(err, out.String())
	}
	var rebuilt struct{ Head string }
	if json.Unmarshal(out.Bytes(), &rebuilt) != nil || rebuilt.Head != result.Revision {
		t.Fatalf("rebuilt at %s, the notebook is at %s", out.String(), result.Revision)
	}
	flag, ok, err := gitexport.ReadPurge(filepath.Join(data, "exports", "alpha.git"))
	if err != nil || !ok || flag.Purge != result.Purge {
		t.Fatalf("purge flag: %+v %v %v", flag, ok, err)
	}
}
