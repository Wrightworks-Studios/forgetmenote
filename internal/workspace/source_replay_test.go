package workspace

import (
	"testing"
)

func TestSourceImportOperationReplaysWithoutOverwritingEdits(t *testing.T) {
	store := newTestStore(t)
	capture := SourceCapture{OperationID: "drive-file-version", Title: "Reference", Original: []byte("hello"), Text: "hello", Record: SourceRecord{ID: "1234567890abcdef1234567890abcdef", MIME: "text/plain"}}
	first, err := store.ImportSource(t.Context(), capture, "", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	edited, err := store.Save(t.Context(), first.Path, append(first.Content, []byte("\nHuman addition")...), first.Revision, "test")
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := store.ImportSource(t.Context(), capture, "", "", "", nil)
	if err != nil || replayed.Revision != first.Revision {
		t.Fatal("lost committed import receipt", err, replayed.Revision)
	}
	current, err := store.Read(t.Context(), first.Path, "")
	if err != nil || string(current.Content) != string(edited.Content) {
		t.Fatal("replay replaced newer edits", err)
	}
	capture.Text = "different"
	if _, err = store.ImportSource(t.Context(), capture, "", "", "", nil); err == nil {
		t.Fatal("operation ID reused for changed content")
	}
}
