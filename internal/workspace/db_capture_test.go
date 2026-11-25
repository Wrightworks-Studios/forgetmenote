package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"forgetmenote/internal/attribution"
	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
)

// captureGateway opens s's notebook through a notes store of its own, as
// another gateway serving the same org does, while s stays open.
func captureGateway(t *testing.T, s *Store) *Store {
	t.Helper()
	env := testEnv(t, s)
	other, err := OpenDB(context.Background(), openTestNotes(t, env), s.notebook, t.TempDir(), s.browseExcluded)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { other.Close() })
	testNotebooks.Store(other, env)
	return other
}

// headCommitOf reads s's head commit.
func headCommitOf(t *testing.T, s *Store) gitobj.Commit {
	t.Helper()
	ctx := context.Background()
	h, err := s.notes.Head(ctx)
	if err != nil {
		t.Fatal(err)
	}
	info, err := s.notes.Commit(ctx, h.Commit)
	if err != nil {
		t.Fatal(err)
	}
	return info.Commit
}

// committedAs fails unless s's head commit has subject and, as the Git
// backend wrote them, by's attribution trailers and identity as its author,
// with ForgetMeNote as its committer.
func committedAs(t *testing.T, s *Store, subject string, by attribution.Identity) {
	t.Helper()
	got := headCommitOf(t, s)
	trailers := "\n\nGitnotes-Source: " + by.Source
	if by.ID != "" {
		trailers += "\nGitnotes-User: " + by.ID
	}
	if by.Client != "" {
		trailers += "\nGitnotes-Client: " + by.Client
	}
	if commitSubject(got.Message) != subject || !strings.Contains(got.Message, trailers+"\n") {
		t.Fatalf("commit message %q, want subject %q and trailers %q", got.Message, subject, trailers)
	}
	if got.Author.Name != by.Name || got.Author.Email != by.Email || got.Committer.Name != "ForgetMeNote" || got.Committer.Email != "forgetmenote@localhost" {
		t.Fatalf("signatures %+v %+v", got.Author, got.Committer)
	}
}

// Filing checks its destination, reserves its source and refuses what it
// cannot file, each for its own reason, and commits with the Git backend's
// messages, trailers and identities.
func TestDatabaseCaptureChecksReservesAndCommits(t *testing.T) {
	g := newTestStore(t)
	ctx := captureContext()
	capture := saveNote(t, g, "inbox/capture.md", "# Capture\n\nPortable text with [a site](https://example.com).\n")
	saveNote(t, g, "inbox/linked.md", "# Linked\n\nSee [existing](../notes/existing.md).\n")
	saveNote(t, g, "inbox/html.md", "# HTML\n\n<div>embedded</div>\n")
	saveNote(t, g, "inbox/referenced.md", "# Referenced\n")
	saveNote(t, g, "inbox/backlink.md", "# Backlink\n\nSee [the reference](referenced.md).\n")
	saveNote(t, g, "inbox/discussed.md", "# Discussed\n")
	saveNote(t, g, "notes/existing.md", "# Existing\n")
	saveNote(t, g, "locked/a.md", "# Locked\n")
	saveNote(t, g, "folder.md/inner.md", "# Inner\n")
	commitFixture(t, g, fixture(commentPath("inbox/discussed.md"), "[]"), fixture(commentPath("notes/orphan.md"), "[]"),
		notestore.FileChange{Path: "alias", Content: []byte("notes"), Mode: gitobj.ModeSymlink})
	if _, err := g.SetLock(context.Background(), "locked", true, headOf(t, g), "owner"); err != nil {
		t.Fatal(err)
	}
	s := g

	read := func(p string) Document {
		t.Helper()
		doc, err := s.Read(context.Background(), p, "")
		if err != nil {
			t.Fatal(err)
		}
		return doc
	}
	// expect pins each case's outcome, so a refusal for the wrong reason
	// fails.
	expect := func(what string, err, class error) {
		t.Helper()
		if class == nil && err != nil || class != nil && !errors.Is(err, class) {
			t.Errorf("%s: %v, want %v", what, err, class)
		}
	}
	base := captureMove(capture)
	for to, class := range map[string]error{"notes/new.md": nil, "filed.md": nil, "notes/existing.md": ErrConflict, "notes/orphan.md": ErrConflict, "folder.md/inner.md": ErrConflict, "folder.md": ErrConflict,
		"missing/filed.md": ErrCaptureInput, "locked/filed.md": ErrForbidden, "alias/filed.md": ErrForbidden, "Trash/filed.md": ErrForbidden, "AGENTS.md": ErrForbidden, "notes/filed.txt": ErrForbidden, "../filed.md": ErrForbidden, "comments/filed.md": ErrForbidden} {
		in := base
		in.ID, in.To = "preflight-"+to, to
		_, err := s.CapturePreflight(ctx, in)
		expect("preflight to "+to, err, class)
	}
	stale := base
	stale.ID, stale.Version = "stale", read("notes/existing.md").Version
	wrongHash := base
	wrongHash.ID, wrongHash.Hash = "wrong-hash", ContentHash([]byte("other"))
	anonymous := base
	anonymous.ID = "anonymous"
	cases := map[string]CaptureMove{"stale": stale, "wrong hash": wrongHash}
	classes := map[string]error{"stale": ErrConflict, "wrong hash": ErrConflict}
	for from, class := range map[string]error{"inbox/linked.md": ErrCaptureInput, "inbox/html.md": ErrCaptureInput, "inbox/referenced.md": ErrCaptureInput, "inbox/discussed.md": ErrCaptureInput, "locked/a.md": ErrForbidden} {
		doc := read(from)
		in := captureMove(doc)
		in.ID = "prepare-" + from
		cases[from], classes[from] = in, class
	}
	for name, in := range cases {
		_, err := s.CapturePrepare(ctx, in)
		expect("prepare "+name, err, classes[name])
	}
	if _, err := s.CapturePrepare(context.Background(), anonymous); err == nil {
		t.Error("a filing without an actor was prepared")
	}

	// The reservation refuses the note, its discussion and links to it, and
	// nothing else; a changed request under the same ID conflicts.
	if _, err := s.CapturePrepare(ctx, base); err != nil {
		t.Fatal("prepare:", err)
	}
	for p, content := range map[string]string{base.From: "# Changed\n", commentPath(base.From): "[]", "inbox/new-link.md": "[capture](capture.md)\n", "inbox/unrelated.md": "# Unrelated\n"} {
		_, err := s.Save(ctx, p, []byte(content), headOf(t, s), "user")
		if p == "inbox/unrelated.md" {
			if err != nil {
				t.Fatalf("unrelated save: %v", err)
			}
			continue
		}
		expect("save of "+p, err, ErrConflict)
	}
	changed := base
	changed.SourceProject = "elsewhere"
	_, err := s.CapturePrepare(ctx, changed)
	expect("prepare of a changed request", err, ErrConflict)

	// It receives into the notebook itself and finishes with the filing
	// commits.
	filed, err := s.CaptureReceive(ctx, base, capture.Content)
	if err != nil || filed.Path != base.To || string(filed.Content) != string(capture.Content) || filed.Version != headOf(t, s) {
		t.Fatalf("receive %+v %v", filed, err)
	}
	committedAs(t, s, "File Quicknote", attribution.From(ctx))
	receipt, err := s.CaptureFinish(ctx, base)
	if err != nil || receipt.State != "done" || receipt.Revision != headOf(t, s) {
		t.Fatalf("finish %+v %v", receipt, err)
	}
	committedAs(t, s, "File Quicknote to another notebook", attribution.From(ctx))
	if _, err = s.Read(ctx, base.From, ""); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("filed source still active", err)
	}
	// Replays answer from the receipts without committing again.
	head := headOf(t, s)
	for name, replay := range map[string]func() (any, error){
		"receive":   func() (any, error) { return s.CaptureReceive(ctx, base, capture.Content) },
		"preflight": func() (any, error) { return s.CapturePreflight(ctx, base) },
		"finish":    func() (any, error) { return s.CaptureFinish(ctx, base) },
		"prepare":   func() (any, error) { return s.CapturePrepare(ctx, base) },
		"cancel":    func() (any, error) { return s.CaptureCancel(ctx, base) },
		"abort":     func() (any, error) { return s.CaptureAbortReceive(ctx, base) },
	} {
		result, err := replay()
		if err != nil {
			t.Fatalf("%s replay: %v", name, err)
		}
		switch r := result.(type) {
		case Document:
			if string(r.Content) != string(capture.Content) {
				t.Fatalf("%s replay read %+v", name, r)
			}
		case CaptureReceipt:
			if r.Revision == "" || r.State == "ready" || r.State == "canceled" {
				t.Fatalf("%s replay answered %+v", name, r)
			}
		}
	}
	if headOf(t, s) != head {
		t.Fatal("a replay committed")
	}
}

// A reservation lives in the notes store: another gateway serving the
// notebook honours it, a restart keeps it, and cancelling releases it for
// every gateway.
func TestCaptureReservationHoldsAcrossGatewaysAndRestart(t *testing.T) {
	ctx := captureContext()
	s := newTestStore(t)
	doc, err := s.Save(ctx, "capture.md", []byte("capture"), "", "user")
	if err != nil {
		t.Fatal(err)
	}
	in := captureMove(doc)
	if _, err = s.CapturePrepare(ctx, in); err != nil {
		t.Fatal(err)
	}
	// Nothing commits from here on, so the other gateway's head stays the
	// one it opened at.
	other := captureGateway(t, s)
	filing := "note is being filed; retry after recovery: " + ErrConflict.Error()
	if _, err = other.Save(ctx, doc.Path, []byte("elsewhere"), doc.Revision, "user"); errorText(err) != filing {
		t.Fatal("another gateway escaped the reservation", err)
	}
	if _, err = other.Save(ctx, commentPath(doc.Path), []byte("[]"), doc.Revision, "user"); errorText(err) != filing {
		t.Fatal("a discussion escaped the reservation", err)
	}
	if _, err = other.Save(ctx, "linking.md", []byte("[capture](capture.md)"), "", "user"); errorText(err) != "referenced note is being filed: "+ErrConflict.Error() {
		t.Fatal("a new link escaped the reservation", err)
	}
	n := other.backend
	for _, rule := range []string{doc.Path, "capture.md"} {
		n.mu.Lock()
		lockErr := n.checkCaptureLock(ctx, rule)
		n.mu.Unlock()
		if !errors.Is(lockErr, ErrConflict) {
			t.Fatal("a lock rule escaped the reservation", lockErr)
		}
	}
	n.mu.Lock()
	lockErr := n.checkCaptureLock(ctx, "elsewhere")
	n.mu.Unlock()
	if lockErr != nil {
		t.Fatal("an unrelated lock rule was refused", lockErr)
	}
	// Comments, lock rules and live edits reach the same checks.
	if _, err = s.AddComment(ctx, doc.Path, "comment", doc.Revision, "user"); !errors.Is(err, ErrConflict) {
		t.Fatal("comment escaped reservation", err)
	}
	if _, err = s.SetLock(ctx, doc.Path, true, doc.Revision, "user"); !errors.Is(err, ErrConflict) {
		t.Fatal("lock escaped reservation", err)
	}
	live, err := s.LiveRead(ctx, doc.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.LivePatch(ctx, TargetedPatch{Path: doc.Path, Base: live.LiveRevision, OperationID: "pending-edit", Edits: []TextEdit{{Start: 0, End: 0, Text: "new "}}}); !errors.Is(err, ErrConflict) {
		t.Fatal("live patch escaped reservation", err)
	}

	s = reopenTestStore(t, s)
	if _, err = s.Save(ctx, doc.Path, []byte("overwrite"), doc.Revision, "user"); errorText(err) != filing {
		t.Fatal("restart lost reservation", err)
	}
	if receipt, err := s.CaptureCancel(ctx, in); err != nil || receipt.State != "canceled" {
		t.Fatal(receipt, err)
	}
	if _, err = other.Save(ctx, doc.Path, []byte("new"), doc.Revision, "user"); err != nil {
		t.Fatal("cancellation did not release", err)
	}
	if _, err = s.CaptureFinish(ctx, in); !errors.Is(err, ErrConflict) {
		t.Fatal("a cancelled filing finished", err)
	}
}

// CaptureSaved finds a Quicknote's creation by the operation its edit group
// committed, reads the note as it was created, and refuses another payload.
func TestCaptureSavedRecoversTheCommittedCreate(t *testing.T) {
	ctx := captureContext()
	s := newTestStore(t)
	created := func(id, path, content string, group EditGroup) string {
		t.Helper()
		raw, _ := json.Marshal(group)
		r, err := s.notes.Apply(context.Background(), notestore.ChangeSet{Subject: "Save Quicknote", Author: fixtureAuthor, Files: []notestore.FileChange{fixture(path, content)},
			OperationID: ContentHash([]byte("capture-user\x00quicknote-create:" + id)), RequestID: ContentHash(raw)})
		if err != nil {
			t.Fatal(err)
		}
		return r.Head.Commit.String()
	}
	first := created("capture-one", "quicknote-one.md", "capture", captureCreateGroup("capture-one", "quicknote-one.md", []byte("capture")))
	if _, err := s.Save(ctx, "quicknote-one.md", []byte("newer"), first, "user"); err != nil {
		t.Fatal(err)
	}
	saved, err := s.CaptureSaved(ctx, "capture-one", "quicknote-one.md", ContentHash([]byte("capture")))
	if err != nil || saved.Revision != first || saved.Version != first || string(saved.Content) != "capture" {
		t.Fatal("capture acknowledgement not recovered", saved, err)
	}
	if _, err = s.CaptureSaved(ctx, "capture-one", "quicknote-one.md", ContentHash([]byte("newer"))); !errors.Is(err, ErrConflict) {
		t.Fatal("another hash recovered", err)
	}
	other := attribution.With(context.Background(), attribution.Identity{ID: "someone-else", Source: "browser"})
	for name, call := range map[string]func() error{
		"unknown capture": func() error {
			_, err := s.CaptureSaved(ctx, "capture-two", "quicknote-one.md", saved.Version)
			return err
		},
		"another actor": func() error {
			_, err := s.CaptureSaved(other, "capture-one", "quicknote-one.md", ContentHash([]byte("capture")))
			return err
		},
	} {
		if err := call(); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, call := range map[string]func() error{
		"no actor": func() error {
			_, err := s.CaptureSaved(context.Background(), "capture-one", "quicknote-one.md", "x")
			return err
		},
		"nested path": func() error { _, err := s.CaptureSaved(ctx, "capture-one", "notes/quicknote-one.md", "x"); return err },
		"other name":  func() error { _, err := s.CaptureSaved(ctx, "capture-one", "one.md", "x"); return err },
	} {
		if err := call(); !errors.Is(err, ErrForbidden) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// An operation committed for another group payload is not this capture.
	created("capture-three", "quicknote-three.md", "three", captureCreateGroup("capture-three", "quicknote-three.md", []byte("different")))
	if _, err = s.CaptureSaved(ctx, "capture-three", "quicknote-three.md", ContentHash([]byte("three"))); !errors.Is(err, ErrConflict) {
		t.Fatal("a mismatched creation recovered", err)
	}
}

// Finished filings are done receipts, which the notes store sweeps; only
// filings in flight count towards the capacity, so it never fills for good.
// Once swept, a retry is answered from the revision the filing committed.
func TestCaptureReceiptsBoundOnlyFilingsInFlight(t *testing.T) {
	defer func(limit int) { maxPendingCaptures = limit }(maxPendingCaptures)
	maxPendingCaptures = 2
	ctx := captureContext()
	source, target := newTestStore(t), newTestStore(t)
	saveNote(t, target, "notes/existing.md", "# Existing\n")
	moves := map[string]CaptureMove{}
	for _, name := range []string{"a", "b", "c"} {
		in := captureMove(saveNote(t, source, name+".md", "# "+name+"\n"))
		in.ID, in.To = "filing-"+name, "notes/"+name+".md"
		moves[name] = in
	}
	for _, name := range []string{"a", "b"} {
		if _, err := source.CapturePrepare(ctx, moves[name]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := source.CapturePrepare(ctx, moves["c"]); errorText(err) != "capture receipt capacity reached" {
		t.Fatal("a third filing in flight was accepted", err)
	}
	filed, err := target.CaptureReceive(ctx, moves["a"], []byte("# a\n"))
	if err != nil {
		t.Fatal(err)
	}
	finished, err := source.CaptureFinish(ctx, moves["a"])
	if err != nil || finished.State != "done" {
		t.Fatal(finished, err)
	}
	if _, err = source.CapturePrepare(ctx, moves["c"]); err != nil {
		t.Fatal("a finished filing still counts towards the capacity", err)
	}
	states := func(s *Store) map[string]string {
		t.Helper()
		receipts, err := s.notes.Receipts(ctx, captureReceiptKind)
		if err != nil {
			t.Fatal(err)
		}
		result := map[string]string{}
		for _, r := range receipts {
			if (r.State == notestore.ReceiptDone) == r.Done.IsZero() {
				t.Fatalf("receipt %s is %s, done at %v", r.Operation, r.State, r.Done)
			}
			var record captureRecord
			if err := json.Unmarshal(r.Result, &record); err != nil {
				t.Fatal(err)
			}
			result[record.Request.ID] = r.State + " " + record.State
		}
		return result
	}
	if got := states(source); len(got) != 3 || got["filing-a"] != "done done" || got["filing-b"] != "pending prepared" || got["filing-c"] != "pending prepared" {
		t.Fatal("source receipts", got)
	}
	if got := states(target); len(got) != 1 || got["filing-a"] != "done received" {
		t.Fatal("target receipts", got)
	}

	later := time.Now().Add(notestore.ReceiptTTL + time.Minute)
	for _, s := range []*Store{source, target} {
		if _, err := s.noteStore.SweepReceipts(ctx, later); err != nil {
			t.Fatal(err)
		}
	}
	if got := states(source); len(got) != 2 || got["filing-a"] != "" {
		t.Fatal("the sweep kept a finished filing or lost one in flight", got)
	}
	if got := states(target); len(got) != 0 {
		t.Fatal("the sweep kept a finished receive", got)
	}
	head := headOf(t, source)
	if again, err := source.CaptureFinish(ctx, moves["a"]); err != nil || again != finished {
		t.Fatal("a swept finish did not replay", again, err)
	}
	if again, err := source.CapturePrepare(ctx, moves["a"]); err != nil || again.Version != moves["a"].Version || string(again.Content) != "# a\n" {
		t.Fatal("a swept prepare did not replay", again, err)
	}
	if again, err := target.CaptureReceive(ctx, moves["a"], []byte("# a\n")); err != nil || again.Version != filed.Version {
		t.Fatal("a swept receive did not replay", again, err)
	}
	if again, err := target.CapturePreflight(ctx, moves["a"]); err != nil || again.State != "received" || again.Revision != filed.Revision {
		t.Fatal("a swept preflight did not find the receive", again, err)
	}
	if headOf(t, source) != head {
		t.Fatal("a replay committed")
	}
	// Recording an outcome is never refused for capacity.
	maxPendingCaptures = 0
	if outcome, err := target.CaptureAbortReceive(ctx, moves["b"]); err != nil || outcome.State != "canceled" {
		t.Fatal("a full notebook refused to fence a receive", outcome, err)
	}
}

// A capture receipt the notebook cannot read stops writes rather than
// letting one past a reservation it cannot see, as the Git backend refuses
// to open a notebook with an invalid record file.
func TestCaptureInvalidReceiptStopsWrites(t *testing.T) {
	ctx := captureContext()
	in := CaptureMove{ID: "filing", From: "capture.md", Version: strings.Repeat("a", 40), Hash: strings.Repeat("b", 64), To: "notes/filed.md", SourceProject: "inbox", TargetProject: "target"}
	r := captureRecord{Version: 1, Actor: "capture-user", Kind: "send", Request: in, CaptureReceipt: CaptureReceipt{State: "prepared"}}
	raw, _ := json.Marshal(r)
	key, request := captureKey(r.Actor, r.Kind, in.ID), captureOperation(r).Request
	for name, c := range map[string]struct {
		receipt notestore.Receipt
		want    string
	}{
		"another key":              {notestore.Receipt{Operation: "elsewhere", RequestHash: request, Result: raw}, "invalid capture receipt identity"},
		"another request":          {notestore.Receipt{Operation: key, RequestHash: "other", Result: raw}, "invalid capture receipt identity"},
		"an unknown state":         {notestore.Receipt{Operation: key, RequestHash: request, Result: []byte(strings.Replace(string(raw), `"prepared"`, `"lost"`, 1))}, "invalid capture receipt state"},
		"an unknown field":         {notestore.Receipt{Operation: key, RequestHash: request, Result: []byte(strings.Replace(string(raw), `{`, `{"extra":1,`, 1))}, "invalid capture receipt"},
		"no revision":              {notestore.Receipt{Operation: key, RequestHash: request, Result: []byte(strings.Replace(string(raw), `"prepared"`, `"done"`, 1))}, "invalid committed capture receipt"},
		"a final record in flight": {notestore.Receipt{Operation: key, RequestHash: request, Result: []byte(strings.Replace(string(raw), `"prepared"`, `"canceled"`, 1))}, "invalid capture receipt state"},
	} {
		t.Run(name, func(t *testing.T) {
			s := newTestStore(t)
			c.receipt.Kind = captureReceiptKind
			if err := s.notes.PutReceipt(ctx, c.receipt); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Save(ctx, "unrelated.md", []byte("unrelated"), "", "user"); errorText(err) != c.want {
				t.Fatalf("a write passed an unreadable receipt: %v, want %s", err, c.want)
			}
		})
	}
}
