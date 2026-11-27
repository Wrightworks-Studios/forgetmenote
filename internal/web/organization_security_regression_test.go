package web

import (
	"context"
	"forgetmenote/internal/catalog"
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
	id := organizerTagJob(t, app, "alice", "security-old-admin-grant")
	admin = false
	if e := app.catalog.UpdateUser(catalog.Bootstrap, catalog.UserUpdate{ID: "alice", Admin: &admin}); e != nil {
		t.Fatal(e)
	}
	if e := app.judgments.CheckJob(context.Background(), id); e == nil {
		t.Fatal("revoked admin still allowed")
	}
	admin = true
	if e := app.catalog.UpdateUser(catalog.Bootstrap, catalog.UserUpdate{ID: "alice", Admin: &admin}); e != nil {
		t.Fatal(e)
	}
	if e := app.judgments.CheckJob(context.Background(), id); e == nil {
		t.Fatal("restored administrator role revived old applying grant")
	}
}
