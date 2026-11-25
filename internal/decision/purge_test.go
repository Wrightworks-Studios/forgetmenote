package decision

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"forgetmenote/internal/pg/pgtest"
)

// A purge redacts the Jev records of its notebook: those naming it as a
// project or notebook at any depth, and its jobs' results; other notebooks'
// records stay as they are.
func TestRedactNotebookRedactsOnlyThatNotebooksRecords(t *testing.T) {
	records, err := OpenRecords(pgtest.Org(t))
	if err != nil {
		t.Fatal(err)
	}
	const secret = "sk-live-jev-0123456789"
	job, other := strings.Repeat("a", 64), strings.Repeat("b", 64)
	for name, body := range map[string]any{
		"jobs/" + job + ".json":           map[string]any{"project": "keys", "message": secret},
		"results/" + job + "-0001.json":   map[string]any{"quote": secret},
		"notebooks/digest.json":           map[string]any{"notebook": "keys", "baseline": secret},
		"notifications/someone.json":      map[string]any{"items": []any{map[string]any{"project": "keys", "text": secret}}},
		"jobs/" + other + ".json":         map[string]any{"project": "other", "message": secret},
		"results/" + other + "-0001.json": map[string]any{"quote": secret},
	} {
		if err := records.Write(name, body); err != nil {
			t.Fatal(err)
		}
	}
	redact := func(raw []byte) ([]byte, bool, error) {
		if !bytes.Contains(raw, []byte(secret)) {
			return raw, false, nil
		}
		return bytes.ReplaceAll(raw, []byte(secret), []byte("[purged]")), true, nil
	}
	if n, err := records.RedactNotebook(t.Context(), "keys", redact, false); err != nil || n != 4 {
		t.Fatal("dry run:", n, err)
	}
	if n, err := records.RedactNotebook(t.Context(), "keys", redact, true); err != nil || n != 4 {
		t.Fatal("redact:", n, err)
	}
	for name, kept := range map[string]bool{"jobs/" + job + ".json": false, "results/" + job + "-0001.json": false, "notebooks/digest.json": false, "notifications/someone.json": false, "jobs/" + other + ".json": true, "results/" + other + "-0001.json": true} {
		var body map[string]any
		if err := records.Read(name, &body); err != nil {
			t.Fatal(name, err)
		}
		raw, _ := records.read(t.Context(), records.db.Pool, name)
		if bytes.Contains(raw, []byte(secret)) != kept {
			t.Fatalf("%s: %s", name, raw)
		}
	}
}

// A record the redaction refuses is left as it was and reported; the
// notebook's other records are still redacted.
func TestRedactNotebookSkipsARecordItCannotRedact(t *testing.T) {
	records, err := OpenRecords(pgtest.Org(t))
	if err != nil {
		t.Fatal(err)
	}
	const secret = "sk-live-jev-skip-0123456789"
	first, second := "jobs/"+strings.Repeat("a", 64)+".json", "jobs/"+strings.Repeat("b", 64)+".json"
	for _, name := range []string{first, second} {
		if err := records.Write(name, map[string]any{"project": "keys", "message": secret}); err != nil {
			t.Fatal(err)
		}
	}
	refused := errors.New("this record cannot be redacted")
	redact := func(raw []byte) ([]byte, bool, error) {
		if !bytes.Contains(raw, []byte(secret)) {
			return raw, false, nil
		}
		if bytes.Contains(raw, []byte(`"version"`)) {
			return nil, false, refused
		}
		return bytes.ReplaceAll(raw, []byte(secret), []byte("[purged]")), true, nil
	}
	// The first record (by name) is refused.
	if err := records.Write(first, map[string]any{"project": "keys", "message": secret, "version": 1}); err != nil {
		t.Fatal(err)
	}
	n, err := records.RedactNotebook(t.Context(), "keys", redact, true)
	if !errors.Is(err, refused) || n != 1 {
		t.Fatal("redact:", n, err)
	}
	for name, kept := range map[string]bool{first: true, second: false} {
		raw, _ := records.read(t.Context(), records.db.Pool, name)
		if bytes.Contains(raw, []byte(secret)) != kept {
			t.Fatalf("%s: %s", name, raw)
		}
	}
}
