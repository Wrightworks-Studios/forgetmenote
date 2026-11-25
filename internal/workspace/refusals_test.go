package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// Refusals of what a caller asked for are classified, so every transport
// answers them as the caller's to correct (ErrInvalid) or as a limit of the
// notebook (ErrCapacity), and they keep the messages they had.
func TestRefusalsAreClassifiedWithTheirMessages(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	doc, err := s.Save(ctx, "notes/a.md", []byte("# A\n"), "", "test")
	if err != nil {
		t.Fatal(err)
	}
	head := doc.Revision
	snapshot := func(index string) error {
		return ValidateNotebookSnapshot(ctx, NotebookSnapshot{Revision: head, Files: map[string][]byte{trashIndexPath: []byte(index)}})
	}
	record := func(id, history string) string {
		return `{"version":1,"records":[{"id":"` + id + `","path":"Trash/` + id + `.md","originalPath":"a.md","title":"","deletedAt":"","deletedBy":"","history":` + history + `}]}`
	}
	id := strings.Repeat("ab", 16)
	tags := make([]string, 51)
	for i := range tags {
		tags[i] = fmt.Sprint("tag", i)
	}
	for _, test := range []struct {
		message string
		err     error
	}{
		{"notes must be UTF-8 text", second(s.Save(ctx, "notes/b.md", []byte{0xff}, "", "test"))},
		{"choose 1–50 notes", second(s.TrashNotes(ctx, nil, head, "test"))},
		{"choose a different destination outside this folder", second(s.Move(ctx, "notes", "notes/inner", head, "owner"))},
		{"use at most 50 tags per note", second(s.SetMetadata(ctx, doc.Path, Metadata{Tags: tags}, head, "test"))},
		{"choose Notes, Drafts, or Archives", second(s.SetContainer(ctx, doc.Path, "box", head, "test"))},
		{"comments must contain 1–10000 bytes", second(s.AddComment(ctx, doc.Path, " ", "", "reader"))},
		{"project instructions are limited to 64 KiB", second(s.WriteInstructions(ctx, make([]byte, 65537), "", "owner"))},
		{"group needs an ID, a summary (up to 300 bytes), and 1–50 edits", second(s.ApplyGroup(ctx, EditGroup{}))},
		{"search is limited to 200 characters", second(s.Find(ctx, strings.Repeat("x", 201), "", "", ""))},
		{"unknown note collection", second(s.ListCollection(ctx, "elsewhere"))},
		{"invalid Trash entry", snapshot(record("zz", "{}"))},
		{"invalid Trash history", snapshot(record(id, "null"))},
		{"invalid Trash revision", snapshot(record(id, `{"a.md":["HEAD"]}`))},
	} {
		if !errors.Is(test.err, ErrInvalid) || errors.Is(test.err, ErrCapacity) || errorText(test.err) != test.message {
			t.Errorf("%s: %v (invalid %t)", test.message, test.err, errors.Is(test.err, ErrInvalid))
		}
	}
	if now := headOf(t, s); now != head {
		t.Fatalf("a refusal wrote %s", now)
	}
}

// Limits a notebook reaches are capacity refusals: a Trash index full of
// notes in the Trash, lock rules, a discussion's threads and the
// subscriptions to a notebook.
func TestNotebookLimitsAreCapacityRefusals(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	doc, err := s.Save(ctx, "notes/a.md", []byte("# A\n"), "", "test")
	if err != nil {
		t.Fatal(err)
	}
	full := TrashIndex{Version: 1, Records: make([]TrashRecord, 10000)}
	for i := range full.Records {
		id := fmt.Sprintf("%032x", i)
		full.Records[i] = TrashRecord{TrashEntry: TrashEntry{ID: id, Path: "Trash/" + id + ".md", OriginalPath: "old/" + id + ".md"}, History: map[string][]string{}}
	}
	rawIndex, _ := json.Marshal(full)
	rules := make([]string, 256)
	for i := range rules {
		rules[i] = fmt.Sprintf("p%03d", i)
	}
	threads := make([]Comment, 1000)
	for i := range threads {
		threads[i] = Comment{ID: fmt.Sprint(i), Author: "reader", Text: "thread", Created: "2026-11-23T00:00:00Z"}
	}
	rawThreads, _ := json.Marshal(threads)
	head := commitFiles(t, s, map[string]string{
		trashIndexPath:          string(rawIndex),
		locksPath:               `{"version":1,"paths":["` + strings.Join(rules, `","`) + `"]}`,
		commentPath(doc.Path):   string(rawThreads),
		"notes/to-trash.md":     "# Bin\n",
		"notes/second-trash.md": "# Bin\n",
	})
	discussion, err := s.Comments(ctx, doc.Path)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		message string
		err     error
	}{
		{"Trash index capacity reached; empty the Trash or restore notes first", second(s.TrashNotes(ctx, []string{"notes/to-trash.md"}, head, "test"))},
		{"project lock limit reached", second(s.SetLock(ctx, "one-more", true, head, "owner"))},
		{"discussion limit reached", second(s.AddComment(ctx, doc.Path, "one more", discussion.Revision, "reader"))},
	} {
		if !errors.Is(test.err, ErrCapacity) || errors.Is(test.err, ErrInvalid) || errorText(test.err) != test.message {
			t.Errorf("%s: %v (capacity %t)", test.message, test.err, errors.Is(test.err, ErrCapacity))
		}
	}
	for range 128 {
		_, cancel, err := s.Watch()
		if err != nil {
			t.Fatal(err)
		}
		defer cancel()
	}
	if _, _, err := s.Watch(); !errors.Is(err, ErrCapacity) || errorText(err) != "project subscription limit reached" {
		t.Errorf("a subscription past the limit: %v", err)
	}
}

// second is an operation's error, for tables of refusals.
func second[T any](_ T, err error) error { return err }

// lowerTrashLimits sets the Trash's bounds for one test.
func lowerTrashLimits(t *testing.T, records, bytes, revisions, names int) {
	t.Helper()
	saved := trashLimits
	trashLimits.records, trashLimits.bytes, trashLimits.revisions, trashLimits.names = records, bytes, revisions, names
	t.Cleanup(func() { trashLimits = saved })
}

// Limits that belong to a note or to the whole notebook, which no retry
// lifts, are their own class: a note's history too long for the Trash to
// record, and a fresh change set listing more files than one may. A change
// set the caller can narrow stays invalid input.
func TestPermanentLimitsAreNeitherCapacityNorInput(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	first := saveNote(t, s, "long.md", "# One\n")
	saveNote(t, s, "long.md", "# Two\n")
	saveNote(t, s, "long.md", "# Three\n")
	saveNote(t, s, "other.md", "# Other\n")
	saveNote(t, s, "renamed.md", "# Renamed\n")
	if _, err := s.Move(ctx, "renamed.md", "twice.md", headOf(t, s), "owner"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Move(ctx, "twice.md", "thrice.md", headOf(t, s), "owner"); err != nil {
		t.Fatal(err)
	}
	head := headOf(t, s)
	saved := changeSetPaths
	changeSetPaths = 2
	t.Cleanup(func() { changeSetPaths = saved })

	lowerTrashLimits(t, 10000, 8<<20, 2, 1000)
	_, longHistory := s.TrashNotes(ctx, []string{"long.md"}, head, "owner")
	lowerTrashLimits(t, 10000, 8<<20, 100000, 2)
	_, manyNames := s.TrashNotes(ctx, []string{"thrice.md"}, head, "owner")
	_, fresh := s.ChangesSince(ctx, "", head)
	for _, test := range []struct {
		message string
		err     error
	}{
		{"note history exceeds Trash capacity", longHistory},
		{"note history exceeds Trash capacity", manyNames},
		{"change set exceeds 2 paths", fresh},
	} {
		if !errors.Is(test.err, ErrLimit) || errors.Is(test.err, ErrCapacity) || errors.Is(test.err, ErrInvalid) || errorText(test.err) != test.message {
			t.Errorf("%s: %v (limit %t)", test.message, test.err, errors.Is(test.err, ErrLimit))
		}
	}
	if _, err := s.ChangesSince(ctx, first.Revision, head); !errors.Is(err, ErrInvalid) || errors.Is(err, ErrLimit) || errorText(err) != "change set exceeds 2 paths; use a fresh snapshot" {
		t.Errorf("a change set from a checkpoint: %v", err)
	}
	if now := headOf(t, s); now != head {
		t.Fatalf("a refusal wrote %s", now)
	}
}
