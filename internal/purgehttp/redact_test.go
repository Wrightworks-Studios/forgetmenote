package purgehttp

import (
	"strings"
	"testing"

	"forgetmenote/internal/workspace"
)

// Records outside the notebook are redacted in their values only: keys and
// the strings that identify or date a record stay, so a pattern such as a
// four-digit number leaves the record readable.
func TestPurgeCleanerRedactsContentValuesOnly(t *testing.T) {
	redactor, err := workspace.NewPurgeRedactor(workspace.PurgeScope{Kind: workspace.PurgeText, Pattern: `\d{4}`})
	if err != nil {
		t.Fatal(err)
	}
	cleaner := &purgeCleaner{text: &redactor}
	raw := []byte(`{"id":"c-2024","createdAt":"2026-11-22T10:00:00Z","title":"PIN 7391","notes":{"k-1234":"v 5678"}}`)
	out, changed, err := cleaner.document(raw)
	want := `{"createdAt":"2026-11-22T10:00:00Z","id":"c-2024","notes":{"k-1234":"v [purged]"},"title":"PIN [purged]"}`
	if err != nil || !changed || string(out) != want {
		t.Fatalf("redacted: %s %v %v", out, changed, err)
	}
}

// A revision a purge rewrote is mapped wherever a record holds it, also under
// the keys that identify a record, which are never redacted.
func TestPurgeCleanerMapsRevisionsUnderEveryKey(t *testing.T) {
	old, now := strings.Repeat("a", 40), strings.Repeat("b", 40)
	redactor, err := workspace.NewPurgeRedactor(workspace.PurgeScope{Kind: workspace.PurgeText, Text: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	cleaner := &purgeCleaner{text: &redactor, remap: func(revision string) (string, bool) { return now, revision == old }}
	raw := []byte(`{"id":"` + old + `","run":"secret ` + old + `","revision":"` + old + `","citations":[{"kind":"notebook","id":"` + old + `"}]}`)
	out, changed, err := cleaner.document(raw)
	want := `{"citations":[{"id":"` + now + `","kind":"notebook"}],"id":"` + now + `","revision":"` + now + `","run":"secret ` + now + `"}`
	if err != nil || !changed || string(out) != want {
		t.Fatalf("remapped: %s %v %v", out, changed, err)
	}
}
