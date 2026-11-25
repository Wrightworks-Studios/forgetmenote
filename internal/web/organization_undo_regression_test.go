package web

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"forgetmenote/internal/attribution"
	"forgetmenote/internal/knowledge"
	"forgetmenote/internal/notestore"
	"forgetmenote/internal/workspace"
)

func TestOrganizerUndoLargeRunIgnoresUnrelatedLaterReceipts(t *testing.T) {
	app := reviewFixture(t, func(http.ResponseWriter, *http.Request) { t.Error("Undo must not invoke inference") })
	ctx := context.Background()
	store, err := app.catalog.Access("alice", "default", "owner")
	if err != nil {
		t.Fatal(err)
	}
	paths := make([]string, 1002)
	files := make([]notestore.FileChange, len(paths))
	for i := range paths {
		paths[i] = fmt.Sprintf("large-%04d.md", i)
		files[i] = notestore.FileChange{Path: paths[i], Content: []byte("# Workshop\nCalibrate the clock.\n")}
	}
	commitElsewhere(t, app, store, notestore.ChangeSet{Subject: "Synthetic large run", Author: fixtureAuthor, Files: files})
	job, err := app.createOrganizationJob(ctx, "alice", "default", jevJobInput{Kind: "organize", OperationID: "large-undo", Organization: &organizationInput{Mode: "apply", Scope: "selected", Paths: paths, Policy: organizationPolicy{Tags: true, Vocabulary: []string{"workshop"}}}}, false, nil)
	if err != nil || job.Total != 1002 {
		t.Fatal("large run must be admitted", job.Total, err)
	}
	account, err := app.catalog.Account("alice")
	if err != nil {
		t.Fatal(err)
	}
	first := *job.Work[0].Organization
	_, evidence, err := app.prepareOrganizationAction(ctx, store, job, &first)
	if err != nil {
		t.Fatal(err)
	}
	first.Request.Tags = []string{"workshop"}
	result, err := store.Organize(attribution.With(ctx, account.Identity()), *first.Request, true)
	if err != nil {
		t.Fatal(err)
	}
	first.Result = &result
	first.Status = "applied"
	record := jevJobResult{ID: digest(job.ID + "0"), Kind: job.Work[0].Kind, Status: "applied", Evidence: evidence, Details: organizationDetails(first)}
	if err = app.saveOrganizationResult(job, 0, nil, record); err != nil {
		t.Fatal(err)
	}
	// Seed the already-undone independent activity rows directly. The only live
	// transaction under test is index zero; 1001 unrelated Git commits add no
	// evidence for restoring its file and must never be sent to the owner.
	for i := 1; i < job.Total; i++ {
		action := *job.Work[i].Organization
		action.Status = "undone"
		action.Request = &workspace.OrganizationAction{ID: fmt.Sprintf("%s:%d", job.ID, i), Kind: "tags", Path: action.Path}
		action.Result = &workspace.OrganizationResult{Revision: result.Revision, Files: []workspace.OrganizationFile{{Path: action.Path}}}
		action.Undo = &workspace.OrganizationUndo{ID: action.Request.ID, Action: *action.Request, Actor: account.ID, Executor: account.ID, Revision: result.Revision}
		action.UndoRevision = result.Revision
		record = jevJobResult{ID: digest(job.ID + fmt.Sprint(i)), Kind: job.Work[i].Kind, Status: "undone", Evidence: []knowledge.JudgmentEvidence{{Path: action.Path}}, Details: organizationDetails(action)}
		if err = app.saveOrganizationResult(job, i, nil, record); err != nil {
			t.Fatal(err)
		}
	}
	job.Cursor = job.Total
	job.State = "completed"
	if err = app.saveJevJob(&job); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	response := proxyCall(app, "alice", "POST", "/p/default/api/jev/jobs/"+job.ID+"/organization/undo", `{"index":0}`)
	if response.Code != 200 || strings.Contains(response.Body.String(), `"error"`) {
		t.Fatal("independent earlier action became permanently un-undoable", response.Code, response.Body.String())
	}
	doc, err := store.Read(ctx, first.Path, "")
	if err != nil || strings.Contains(string(doc.Content), "tags:") {
		t.Fatal("large-run Undo did not restore original note", err)
	}
	response = proxyCall(app, "alice", "POST", "/p/default/api/jev/jobs/"+job.ID+"/organization/undo", `{}`)
	if response.Code != 200 || strings.Contains(response.Body.String(), `"error"`) {
		t.Fatal("retrying whole-run Undo failed", response.Code, response.Body.String())
	}
	settings, err := app.loadJevNotebook("default")
	if err != nil || settings.Organization == nil || len(settings.Organization.Suppressed) < len(paths) {
		t.Fatal("restored evidence was not durably suppressed", err)
	}
	t.Logf("individual Undo and whole-run retry across %d actions: %s", len(paths), time.Since(started))
}
