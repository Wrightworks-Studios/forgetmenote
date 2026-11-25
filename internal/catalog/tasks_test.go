package catalog

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"forgetmenote/internal/messaging"
	"forgetmenote/internal/workspace"
)

// taskCatalog has a Slack client for workspace T1 and the named people, the
// first an admin; the admin maps member UMAPPED to the person named mapped.
func taskCatalog(t *testing.T, names ...string) (*Catalog, string, map[string]string) {
	t.Helper()
	dir := t.TempDir()
	c, err := Open(dir, nil, testDB(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	ids := map[string]string{}
	for i, name := range names {
		if ids[name], err = c.CreateLocalUser(Bootstrap, name, "task-test-password"); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			admin := true
			if err = c.UpdateUser(Bootstrap, UserUpdate{ID: ids[name], Admin: &admin}); err != nil {
				t.Fatal(err)
			}
		}
	}
	users := map[string]string{}
	if ids["mapped"] != "" {
		users["UMAPPED"] = ids["mapped"]
	}
	if _, err = c.SetChatClients(Bootstrap, ChatClients{Clients: map[string]messaging.Client{"slack": {Workspace: "T1", Users: users, Channels: map[string]messaging.Binding{}}}}); err != nil {
		t.Fatal(err)
	}
	return c, dir, ids
}

func TestTaskQueuesLiveInAProtectedNotebookOnlyTaskActionsWrite(t *testing.T) {
	c, dir, ids := taskCatalog(t, "admin", "alice")
	admin, alice := ids["admin"], ids["alice"]
	if _, err := c.SetTaskQueues(alice, TaskQueues{Client: "slack"}); !errors.Is(err, workspace.ErrForbidden) {
		t.Fatalf("a person who is not an admin enabled task queues: %v", err)
	}
	if _, err := c.SetTaskQueues(admin, TaskQueues{Client: "slack", Group: "missing"}); err == nil {
		t.Fatal("task queues accepted a group that does not exist")
	}
	settings, err := c.SetTaskQueues(admin, TaskQueues{Client: "slack"})
	if err != nil || settings.Project != TaskQueuesProject || settings.Revision != 1 {
		t.Fatalf("enable: %+v %v", settings, err)
	}
	if _, err = c.SetTaskQueues(admin, TaskQueues{Client: "slack"}); !errors.Is(err, workspace.ErrConflict) {
		t.Fatalf("a stale revision replaced the settings: %v", err)
	}
	phrased := settings
	phrased.AwayStatuses, phrased.HolidayTitles = []string{"Heads down"}, []string{" Office Closed ", "office closed", "Holiday"}
	if settings, err = c.SetTaskQueues(admin, phrased); err != nil || strings.Join(settings.HolidayTitles, "|") != "holiday|office closed" || strings.Join(settings.AwayStatuses, "|") != "heads down" {
		t.Fatalf("phrases: %+v %v", settings, err)
	}
	phrased = settings
	phrased.HolidayTitles = slices.Repeat([]string{"holiday"}, 21)
	if _, err = c.SetTaskQueues(admin, phrased); err == nil {
		t.Fatal("21 holiday titles were accepted")
	}
	if !ReservedNotebookID(TaskQueuesProject) || c.CheckCreate(admin, TaskQueuesProject, "Copy") == nil {
		t.Fatal("anyone may create a notebook with the task queues ID")
	}
	if _, err = c.Access(admin, TaskQueuesProject, "read"); err != nil {
		t.Fatalf("an admin cannot read the task queues: %v", err)
	}
	for _, role := range []string{"comment", "edit", "owner"} {
		if _, err = c.Access(admin, TaskQueuesProject, role); err == nil {
			t.Fatalf("the task queues notebook gave %s access, so a write could skip its task action", role)
		}
	}
	if _, err = c.Access(alice, TaskQueuesProject, "read"); err == nil {
		t.Fatal("a colleague can read every queue")
	}
	if err = c.Member(admin, TaskQueuesProject, alice, "read"); err == nil {
		t.Fatal("the task queues notebook was shared")
	}
	for _, p := range c.List(admin) {
		if p.ID == TaskQueuesProject && (p.Capabilities.Edit || len(p.Members) != 0) {
			t.Fatalf("the notebook offers editing: %+v", p.Capabilities)
		}
	}
	got, store, ok := c.TaskQueues()
	if !ok || store == nil || got.Client != "slack" || got.Workspace != "T1" {
		t.Fatalf("task queues = %+v %v %v", got, store != nil, ok)
	}
	c.Close()
	if c, err = Open(dir, nil, testDB(t, dir)); err != nil {
		t.Fatal(err)
	}
	if _, store, ok = c.TaskQueues(); !ok || store == nil {
		t.Fatal("task queues did not persist")
	}
	disabled := settings
	disabled.Disabled = true
	if _, err = c.SetTaskQueues(admin, disabled); err != nil {
		t.Fatal(err)
	}
	if _, _, ok = c.TaskQueues(); ok {
		t.Fatal("disabled task queues still run")
	}
}

func TestTaskPeopleFollowTheMemberAndAttachAccountsByLink(t *testing.T) {
	c, _, ids := taskCatalog(t, "admin", "alice", "bob", "carol", "mapped")
	admin := ids["admin"]
	if _, err := c.TaskPersonForMember("slack", "T1", "UALICE", true); err == nil {
		t.Fatal("people were created before task queues were enabled")
	}
	if _, err := c.SetTaskQueues(admin, TaskQueues{Client: "slack"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.TaskPersonForMember("slack", "T2", "UALICE", true); err == nil {
		t.Fatal("a member of another workspace got a queue")
	}
	if _, err := c.TaskPersonForMember("slack", "T1", "UALICE", false); !errors.Is(err, ErrNoTaskPerson) {
		t.Fatalf("a lookup created a person: %v", err)
	}
	alice, err := c.TaskPersonForMember("slack", "T1", "UALICE", true)
	if err != nil || !ValidTaskPerson(alice.ID) || alice.User != "UALICE" || alice.Account != "" {
		t.Fatalf("member person: %+v %v", alice, err)
	}
	if again, _ := c.TaskPersonForMember("slack", "T1", "UALICE", true); again.ID != alice.ID {
		t.Fatal("a member got a second queue")
	}
	// A member without an account still has a queue; linking attaches the account.
	if owner, _ := c.TaskPersonAccount(alice.ID); owner != "" {
		t.Fatalf("an unlinked member has account %q", owner)
	}
	if err = c.LinkMessageUser(ids["alice"], "slack", "T1", "UALICE"); err != nil {
		t.Fatal(err)
	}
	if p, err := c.TaskPersonForAccount(ids["alice"], false); err != nil || p.ID != alice.ID {
		t.Fatalf("the linked account did not reach the member's queue: %+v %v", p, err)
	}
	if owner, _ := c.TaskPersonAccount(alice.ID); owner != ids["alice"] {
		t.Fatalf("queue owner = %q", owner)
	}
	// An admin's mapping counts as a link.
	mapped, err := c.TaskPersonForAccount(ids["mapped"], true)
	if err != nil || mapped.User != "UMAPPED" {
		t.Fatalf("mapped account: %+v %v", mapped, err)
	}

	// An account without a member gets its own queue, which moves to the
	// member when the account is linked, if that member has none yet.
	bob, err := c.TaskPersonForAccount(ids["bob"], true)
	if err != nil || bob.Account != ids["bob"] || bob.User != "" {
		t.Fatalf("account person: %+v %v", bob, err)
	}
	if c.TaskPersonMember(bob.ID) != "" {
		t.Fatal("an account without a member has one")
	}
	if err = c.LinkMessageUser(ids["bob"], "slack", "T1", "UBOB"); err != nil {
		t.Fatal(err)
	}
	// Before the queue moves, the account's member already hears about it.
	if member := c.TaskPersonMember(bob.ID); member != "UBOB" {
		t.Fatalf("the linked account's queue reaches %q", member)
	}
	if p, err := c.TaskPersonForMember("slack", "T1", "UBOB", true); err != nil || p.ID != bob.ID || p.User != "UBOB" || p.Account != "" {
		t.Fatalf("the account's queue did not move to its member: %+v %v", p, err)
	}
	if p, _ := c.TaskPersonForAccount(ids["bob"], true); p.ID != bob.ID {
		t.Fatal("the account lost its queue")
	}
	// Unlinking takes the member's queue away from the account.
	if err = c.UnlinkMessageUser(ids["bob"], "slack", "T1", "UBOB"); err != nil {
		t.Fatal(err)
	}
	if owner, _ := c.TaskPersonAccount(bob.ID); owner != "" {
		t.Fatalf("an unlinked account still owns the member's queue: %q", owner)
	}
	if _, err = c.TaskPersonForAccount(ids["bob"], false); !errors.Is(err, ErrNoTaskPerson) {
		t.Fatalf("an unlinked account still reaches the member's queue: %v", err)
	}
	// Service accounts and disabled people never get queues.
	svc, err := c.CreateService(admin, "Service", admin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.TaskPersonForAccount(svc.ID, true); err == nil {
		t.Fatal("a service account got a queue")
	}
	disabled := true
	if err = c.UpdateUser(Bootstrap, UserUpdate{ID: ids["carol"], Disabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	if _, err = c.TaskPersonForAccount(ids["carol"], true); err == nil {
		t.Fatal("a disabled account got a queue")
	}
}
