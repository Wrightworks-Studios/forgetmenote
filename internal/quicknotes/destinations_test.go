package quicknotes

import (
	"context"
	"testing"

	"forgetmenote/internal/catalog"
)

func destinationFor(t *testing.T, s *Service, subject, project string) (Destination, bool) {
	t.Helper()
	available, err := s.Destinations(context.Background(), subject)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range available {
		if d.Project == project && d.Folder == "" {
			return d, true
		}
	}
	return Destination{}, false
}

// A group grant shares a notebook as surely as a direct member does, so an
// approval of the private destination must not cover it afterwards.
func TestDestinationsCountGroupGrantsAsSharing(t *testing.T) {
	s, c, _ := fixture(t)
	stopScanWorker(t, s)
	if err := c.Create(catalog.Bootstrap, "target", "Private destination"); err != nil {
		t.Fatal(err)
	}
	if err := c.Member(catalog.Bootstrap, "target", "alice", "owner"); err != nil {
		t.Fatal(err)
	}
	if err := c.Member("alice", "target", catalog.Bootstrap, ""); err != nil {
		t.Fatal(err)
	}
	if err := c.Create(catalog.Bootstrap, "elsewhere", "Elsewhere"); err != nil {
		t.Fatal(err)
	}
	if err := c.Member(catalog.Bootstrap, "elsewhere", "bob", "read"); err != nil {
		t.Fatal(err)
	}
	private, ok := destinationFor(t, s, "alice", "target")
	if !ok || private.Shared {
		t.Fatalf("private destination fixture: %+v %v", private, ok)
	}
	settings, err := s.Settings("alice")
	if err != nil {
		t.Fatal(err)
	}
	settings.Mode, settings.CloudConsent, settings.ApprovedDestinations = "automatic", true, []Destination{private}
	if _, err = s.UpdateSettings("alice", settings); err != nil {
		t.Fatal(err)
	}
	if _, err = c.SetGroup(catalog.Bootstrap, catalog.Group{ID: "readers", Name: "Readers", Members: []string{"bob"}}); err != nil {
		t.Fatal(err)
	}
	if err = c.SetProjectGroup("alice", "target", "readers", "read"); err != nil {
		t.Fatal(err)
	}
	shared, ok := destinationFor(t, s, "alice", "target")
	if !ok || !shared.Shared {
		t.Fatalf("group-shared destination reported private: %+v %v", shared, ok)
	}
	current, err := s.Settings("alice")
	if err != nil {
		t.Fatal(err)
	}
	if approved(current, shared) {
		t.Fatal("private approval still covers the group-shared destination")
	}
}

// The instance's bootstrap identity holds Admin authority like any instance
// Admin, which does not count as sharing; its membership of the notebooks it
// created, such as the initial one, leaves them private.
func TestDestinationsIgnoreTheBootstrapIdentityAsSharing(t *testing.T) {
	s, c, _ := fixture(t)
	stopScanWorker(t, s)
	if err := c.Create(catalog.Bootstrap, "created", "Created by bootstrap"); err != nil {
		t.Fatal(err)
	}
	if err := c.Member(catalog.Bootstrap, "created", "alice", "owner"); err != nil {
		t.Fatal(err)
	}
	if d, ok := destinationFor(t, s, "alice", "created"); !ok || d.Shared {
		t.Fatalf("bootstrap membership reported as sharing: %+v %v", d, ok)
	}
	if err := c.Member("alice", "created", "bob", "read"); err != nil {
		t.Fatal(err)
	}
	if d, ok := destinationFor(t, s, "alice", "created"); !ok || !d.Shared {
		t.Fatalf("another person's membership reported private: %+v %v", d, ok)
	}
}

// Admin authority over another employee's private assistant notebook does not
// make it a filing destination; the employee may still file there.
func TestDestinationsSkipOtherEmployeesAssistantNotebooks(t *testing.T) {
	s, c, _ := fixture(t)
	stopScanWorker(t, s)
	if _, err := c.SetAssistantPolicy(catalog.Bootstrap, catalog.AssistantPolicy{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := c.SetUser(catalog.Bootstrap, catalog.User{ID: "admin", Admin: true}); err != nil {
		t.Fatal(err)
	}
	assistant, err := c.SaveAssistant("alice", catalog.AssistantSettings{Enabled: true, ExternalProcessing: true, TimeZone: "UTC"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Access("admin", assistant.Project, "owner"); err != nil {
		t.Fatal("fixture: Admin authority over the assistant notebook", err)
	}
	if d, ok := destinationFor(t, s, "admin", assistant.Project); ok {
		t.Errorf("another employee's assistant notebook offered as a filing destination: %+v", d)
	}
	if _, ok := destinationFor(t, s, "alice", assistant.Project); !ok {
		t.Error("employee's own assistant notebook is no longer a destination")
	}
	memory, err := c.ProjectMemory("alice", "billing", true)
	if err != nil {
		t.Fatal(err)
	}
	if d, ok := destinationFor(t, s, "alice", memory); ok {
		t.Errorf("project memory offered as a filing destination: %+v", d)
	}
}
