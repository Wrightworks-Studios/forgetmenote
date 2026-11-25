package catalog

import (
	"bytes"
	"errors"
	"sync"
	"testing"
	"time"

	"forgetmenote/internal/branding"
	"forgetmenote/internal/pg"
	"forgetmenote/internal/pg/pgtest"
)

// gateways opens two catalogs on one database, each with its own data
// directory and notes store, as two gateway processes do.
func gateways(t *testing.T) (*Catalog, *Catalog) {
	t.Helper()
	db := pgtest.Org(t)
	open := func() *Catalog {
		c, err := Open(t.TempDir(), nil, db)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(c.Close)
		return c
	}
	a := open()
	return a, open()
}

func TestCatalogChangesReachAnotherGatewayWithinItsFreshness(t *testing.T) {
	a, b := gateways(t)
	if err := a.Member(Bootstrap, "default", "bob", "read"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(catalogFreshness)
	if _, err := b.Access("bob", "default", "read"); err != nil {
		t.Fatal("a grant on one gateway is unknown to another:", err)
	}
	granted, _ := a.Organization(Bootstrap)
	if err := b.Member(Bootstrap, "default", "bob", ""); err != nil {
		t.Fatal(err)
	}
	time.Sleep(catalogFreshness)
	if _, err := a.Access("bob", "default", "read"); err == nil {
		t.Fatal("a revocation on one gateway left access on another")
	}
	revoked, err := a.Organization(Bootstrap)
	if err != nil || len(revoked.Activity) != len(granted.Activity)+1 || revoked.Activity[0].Details["role"] != "" || revoked.Activity[0].Details["before"] != "read" {
		t.Fatalf("the other gateway's receipt is missing: %+v %v", revoked.Activity, err)
	}
}

func TestStaleGatewayChangeConflictsAndItsRetrySeesTheOther(t *testing.T) {
	a, b := gateways(t)
	b.freshness = time.Hour
	if err := a.Member(Bootstrap, "default", "alice", "edit"); err != nil {
		t.Fatal(err)
	}
	if err := b.Member(Bootstrap, "default", "bob", "read"); !errors.Is(err, ErrStorageUnavailable) || !errors.Is(err, pg.ErrConflict) {
		t.Fatalf("a change computed from a stale catalog was stored: %v", err)
	}
	if _, err := b.Access("bob", "default", "read"); err == nil {
		t.Fatal("a refused change granted access")
	}
	if err := b.Member(Bootstrap, "default", "bob", "read"); err != nil {
		t.Fatal("the retry failed:", err)
	}
	a.freshness = 0
	for _, user := range []string{"alice", "bob"} {
		if _, err := a.Access(user, "default", "read"); err != nil {
			t.Fatalf("%s lost access after the conflict: %v", user, err)
		}
	}
}

func TestCatalogGrantsNothingWhileTheDatabaseIsUnreachable(t *testing.T) {
	db := pgtest.Database(t)
	c, err := Open(t.TempDir(), nil, db)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err = c.Member(Bootstrap, "default", "bob", "read"); err != nil {
		t.Fatal(err)
	}
	db.Pool.Close()
	c.freshness = 0
	if _, err = c.Access("bob", "default", "read"); err == nil {
		t.Fatal("an unreachable database left a cached grant valid")
	}
	if c.IsAdmin(Bootstrap) || c.CanCreateProjects(Bootstrap) {
		t.Fatal("an unreachable database left instance authority")
	}
	if err = c.Member(Bootstrap, "default", "carol", "read"); err == nil {
		t.Fatal("a change was accepted without its database")
	}
}

func TestRefusedCatalogWriteKeepsStoredPermissionsAndAudit(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(dir, nil, testDB(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	before, _ := c.Organization(Bootstrap)
	restore := pgtest.FailWrites(t, c.db, catalogKind)
	if err = c.Member(Bootstrap, "default", "bob", "read"); !errors.Is(err, ErrStorageUnavailable) {
		t.Fatalf("a refused write = %v", err)
	}
	if _, err = c.Access("bob", "default", "read"); err == nil {
		t.Fatal("a refused write granted access")
	}
	restore()
	after, _ := c.Organization(Bootstrap)
	if len(after.Activity) != len(before.Activity) {
		t.Fatal("a refused write left an audit receipt")
	}
	if err = c.Member(Bootstrap, "default", "bob", "read"); err != nil {
		t.Fatal(err)
	}
	c.Close()
	if c, err = Open(dir, nil, testDB(t, dir)); err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err = c.Access("bob", "default", "read"); err != nil {
		t.Fatal("the retried grant was not durable:", err)
	}
}

func TestLostCommitAcknowledgementReloadsWhatTheDatabaseKept(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(dir, nil, testDB(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.commitFault = func() error { return errors.New("injected lost acknowledgement") }
	if err = c.Member(Bootstrap, "default", "bob", "read"); !errors.Is(err, errCommitUnknown) {
		t.Fatalf("an unacknowledged commit = %v", err)
	}
	c.commitFault = nil
	if _, err = c.Access("bob", "default", "read"); err != nil {
		t.Fatal("the catalog kept its rolled-back view of a committed change:", err)
	}
}

func TestCatalogActivityAndLogoSurviveRestartAndOldLogosRetire(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(dir, nil, testDB(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	logo := branding.DefaultIcon()
	if err = c.SetInstanceBranding(Bootstrap, "Research", &logo); err != nil {
		t.Fatal(err)
	}
	if err = c.Member(Bootstrap, "default", "bob", "read"); err != nil {
		t.Fatal(err)
	}
	before, _ := c.Organization(Bootstrap)
	saved := c.InstanceLogo()
	c.Close()
	if c, err = Open(dir, nil, testDB(t, dir)); err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	after, _ := c.Organization(Bootstrap)
	if len(after.Activity) != len(before.Activity) || after.Activity[0].ID != before.Activity[0].ID || !bytes.Equal(c.InstanceLogo(), saved) {
		t.Fatal("audit receipts or logo lost after restart")
	}
	empty := []byte{}
	if err = c.SetInstanceBranding(Bootstrap, "Research", &empty); err != nil {
		t.Fatal(err)
	}
	var logos int
	if err = c.db.Pool.QueryRow(t.Context(), "SELECT count(*) FROM catalog_logos WHERE org = $1", c.db.Org).Scan(&logos); err != nil || logos != 0 {
		t.Fatalf("a replaced logo stayed stored: %d %v", logos, err)
	}
}

func TestCatalogRefusesAnUnsupportedDocumentVersion(t *testing.T) {
	db := pgtest.Org(t)
	storeCatalog(t, db, `{"version": 999, "data": {"users": {}, "projects": {}}}`)
	if c, err := Open(t.TempDir(), nil, db); err == nil {
		c.Close()
		t.Fatal("a future catalog was accepted")
	}
	if version, _, err := db.ReadDocument(t.Context(), db.Pool, catalogKind, catalogID, -1); err != nil || version != 1 {
		t.Fatalf("refusing a future catalog changed it: version %d %v", version, err)
	}
}

func TestNULTextIsRefusedAsInputAndChangesNothing(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(dir, nil, testDB(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err = c.Create(Bootstrap, "nul", "a\x00b"); !errors.Is(err, pg.ErrNUL) || errors.Is(err, ErrStorageUnavailable) {
		t.Fatalf("a NUL notebook name = %v", err)
	}
	if _, err = c.Access(Bootstrap, "nul", "read"); err == nil {
		t.Fatal("a refused notebook exists")
	}
	if err = c.Member(Bootstrap, "default", "bob", "read"); err != nil {
		t.Fatal("the catalog stayed unavailable after bad input:", err)
	}
}

func TestFirstUseAccountOnAStaleGatewayAdoptsTheStoredOne(t *testing.T) {
	a, b := gateways(t)
	b.freshness = time.Hour
	for _, subject := range []string{Bootstrap, "carol"} {
		created, err := a.Account(subject)
		if err != nil {
			t.Fatal(err)
		}
		adopted, err := b.Account(subject)
		if err != nil {
			t.Fatalf("%s's first request on another gateway failed: %v", subject, err)
		}
		if adopted.ID != created.ID {
			t.Fatalf("%s has two accounts: %s and %s", subject, created.ID, adopted.ID)
		}
	}
}

// Every data call records receipts, reads included. A receipt decides nothing
// from the catalog, so a stale gateway records it over the stored document
// instead of failing the call.
func TestAStaleGatewayRecordsActivityOverTheStoredCatalog(t *testing.T) {
	a, b := gateways(t)
	b.freshness = time.Hour
	if err := a.Member(Bootstrap, "default", "bob", "read"); err != nil {
		t.Fatal(err)
	}
	if err := b.RecordActivity("bob", "data.call", "metrics", "pending", map[string]string{"operation": "read"}); err != nil {
		t.Fatalf("a read's receipt on a stale gateway failed: %v", err)
	}
	if _, err := b.Access("bob", "default", "read"); err != nil {
		t.Fatal("the receipt lost the other gateway's grant:", err)
	}
	a.freshness = 0
	org, err := a.Organization(Bootstrap)
	if err != nil || len(org.Activity) < 2 || org.Activity[0].Action != "data.call" || org.Activity[1].Action != "notebook.member" {
		t.Fatalf("both gateways' receipts are not stored in order: %+v %v", org.Activity, err)
	}
}

// Scheduled jobs on several gateways start in the same minute, and each data
// call records a pending receipt and its outcome.
func TestSimultaneousDataCallReceiptsOnTwoGatewaysAreAllRecorded(t *testing.T) {
	a, b := gateways(t)
	before, _ := a.Organization(Bootstrap)
	const runs = 8
	var wg sync.WaitGroup
	failures := make(chan error, 4*runs)
	for range runs {
		for _, gateway := range []*Catalog{a, b} {
			wg.Go(func() {
				for _, outcome := range []string{"pending", "succeeded"} {
					if err := gateway.RecordActivity(Bootstrap, "data.call", "metrics", outcome, map[string]string{"operation": "read"}); err != nil {
						failures <- err
					}
				}
			})
		}
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Error("a receipt failed:", err)
	}
	a.freshness = 0
	after, err := a.Organization(Bootstrap)
	if err != nil || len(after.Activity) != len(before.Activity)+4*runs {
		t.Fatalf("stored %d receipts, want %d: %v", len(after.Activity)-len(before.Activity), 4*runs, err)
	}
}
