package web

import (
	"context"
	"forgetmenote/internal/catalog"
	"strings"
	"testing"
)

func TestOrganizerAdminRevocationDoesNotReviveGrant(t *testing.T) {
	app := reviewFixture(t, syntheticOrganizationAnswer)
	if e := app.catalog.Member(catalog.Bootstrap, "default", "bob", "owner"); e != nil {
		t.Fatal(e)
	}
	if e := app.catalog.Member(catalog.Bootstrap, "default", "alice", "read"); e != nil {
		t.Fatal(e)
	}
	admin := true
	if e := app.catalog.UpdateUser(catalog.Bootstrap, catalog.UserUpdate{ID: "alice", Admin: &admin}); e != nil {
		t.Fatal(e)
	}
	input := jevJobInput{Kind: "organize", OperationID: "security-old-admin-grant", Organization: &organizationInput{Mode: "apply", Scope: "selected", Paths: []string{"a.md"}, Policy: organizationPolicy{Tags: true, Vocabulary: []string{"workshop"}}}}
	job, e := app.createOrganizationJob(context.Background(), "alice", "default", input, false, nil)
	if e != nil {
		t.Fatal(e)
	}
	judge, _ := app.jevConfigured()
	admin = false
	if e := app.catalog.UpdateUser(catalog.Bootstrap, catalog.UserUpdate{ID: "alice", Admin: &admin}); e != nil {
		t.Fatal(e)
	}
	if e := app.checkOrganizationJob(context.Background(), job, judge); e == nil {
		t.Fatal("revoked admin still allowed")
	}
	admin = true
	if e := app.catalog.UpdateUser(catalog.Bootstrap, catalog.UserUpdate{ID: "alice", Admin: &admin}); e != nil {
		t.Fatal(e)
	}
	if e := app.checkOrganizationJob(context.Background(), job, judge); e == nil {
		app.runOrganizationJob(context.Background(), job)
		store, _ := app.catalog.Access("alice", "default", "read")
		doc, e := store.Read(context.Background(), "a.md", "")
		if e != nil {
			t.Fatal(e)
		}
		if strings.Contains(string(doc.Content), "tags:") {
			t.Fatal("restored administrator role executed the revoked applying grant")
		}
		t.Fatal("restored administrator role revived old applying grant")
	}
}
