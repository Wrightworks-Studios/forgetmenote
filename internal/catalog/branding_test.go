package catalog

import (
	"bytes"
	"testing"

	"forgetmenote/internal/branding"
	"forgetmenote/internal/pg/pgtest"
)

func TestInstanceBrandingPersistsAndFailedSavePreservesPrevious(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(dir, nil, testDB(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	logo := branding.DefaultIcon()
	if err := c.SetInstanceBranding(Bootstrap, "Research", &logo); err != nil {
		t.Fatal(err)
	}
	saved := c.InstanceLogo()
	c.Close()
	c, err = Open(dir, nil, testDB(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.InstanceName() != "Research" || !bytes.Equal(c.InstanceLogo(), saved) {
		t.Fatal("branding lost after restart")
	}
	copy := c.InstanceLogo()
	copy[0] ^= 1
	if !bytes.Equal(c.InstanceLogo(), saved) {
		t.Fatal("caller can mutate the saved logo")
	}
	pgtest.FailWrites(t, c.db, "catalog")
	before, _ := c.Organization(Bootstrap)
	empty := []byte{}
	if err = c.SetInstanceBranding(Bootstrap, "Failed change", &empty); err == nil {
		t.Fatal("save unexpectedly succeeded")
	}
	after, _ := c.Organization(Bootstrap)
	if c.InstanceName() != "Research" || !bytes.Equal(c.InstanceLogo(), saved) || len(before.Activity) != len(after.Activity) {
		t.Fatal("failed save changed branding or audit history")
	}
}
