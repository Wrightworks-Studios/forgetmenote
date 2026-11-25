package catalog

import (
	"fmt"
	"forgetmenote/internal/pg/pgtest"
	"testing"
)

func TestNotebookCreationBeyondLegacyCount(t *testing.T) {
	c, err := Open(t.TempDir(), nil, pgtest.Org(t))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// Existing catalog entries need no checked-out workspace until accessed.
	for i := 0; i < 256; i++ {
		id := fmt.Sprintf("existing-%d", i)
		c.state.Projects[id] = Project{ID: id, Name: id, Members: map[string]string{}}
	}
	if err := c.Create(Bootstrap, "next", "Next notebook"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Access(Bootstrap, "next", "edit"); err != nil {
		t.Fatal(err)
	}
}
