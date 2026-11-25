package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"forgetmenote/internal/filestate"
	"forgetmenote/internal/pg"
	"forgetmenote/internal/pg/pgtest"

	"github.com/jackc/pgx/v5"
)

// The accounts and conversations the file-based release's own store left in
// testdata/release-a: alice's answered question, her external conversation and
// a run that was streaming when the gateway stopped; bob's queued run in a full
// record, interrupted by a recovery overlay, a deletion receipt and a run
// awaiting his confirmation.
const (
	fixtureAlice   = "0f6c1d0e8a3b4c5d9e7f112233445566"
	fixtureBob     = "7a8b9c0d1e2f30415263748596a7b8c9"
	aliceAnswered  = "74ce5b05a030c0b12be3cb55783a0cdd"
	aliceBound     = "8b19c6e408ddf6a1c549bb2d03dae2a3"
	aliceStreaming = "ab38f5e6210d7b2c98c2e61d3d12dda6"
	bobOverlaid    = "e6b5e3fec57da0f316343d4939d1098e"
	bobConfirming  = "a21d222bef9a7b3f4463717cef32ce0c"
)

func fixturePath(data, user, id, name string) string {
	return filepath.Join(data, "chat", "users", Hash([]byte(user)), "conversations", id, name)
}

// releaseA copies testdata/release-a into a data directory of the test's own.
func releaseA(t *testing.T) string {
	t.Helper()
	data := t.TempDir()
	if err := os.CopyFS(data, os.DirFS("testdata/release-a")); err != nil {
		t.Fatal(err)
	}
	return data
}

// stateFiles reads every file under dir's chat directory by its slash path,
// leaving out the lock, which holds no state.
func stateFiles(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	root := os.DirFS(dir)
	err := fs.WalkDir(root, chatFiles, func(rel string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || rel == "chat/.lock" {
			return err
		}
		files[rel], err = fs.ReadFile(root, rel)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func importFiles(t *testing.T, db *pg.DB, data string) (filestate.Sum, error) {
	t.Helper()
	src, err := filestate.OpenSource(data)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	ctx := context.Background()
	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	sum, err := ImportFiles(ctx, db, tx, src)
	if err == nil {
		err = tx.Commit(ctx)
	}
	return sum, err
}

func exportFiles(t *testing.T, db *pg.DB) (filestate.Sum, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "export")
	out, err := filestate.CreateTarget(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	ctx := context.Background()
	tx, err := db.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	sum, err := ExportFiles(ctx, db, tx, out)
	if err != nil {
		t.Fatal(err)
	}
	return sum, dir
}

// Imported conversations read as the file-based release read them, export back
// to its files byte for byte, and recover on open as it would have on restart.
func TestChatFilesRoundTrip(t *testing.T) {
	data := releaseA(t)
	// A crash's temporary beside a record is not state.
	if err := os.WriteFile(fixturePath(data, fixtureBob, bobOverlaid, ".chat-4242"), []byte(`{"version":2`), 0600); err != nil {
		t.Fatal(err)
	}
	db := pgtest.Org(t)
	imported, err := importFiles(t, db, data)
	if err != nil {
		t.Fatal(err)
	}
	want := stateFiles(t, "testdata/release-a")
	if imported.Files != len(want) || len(want) != 7 {
		t.Fatalf("import read %d of %d files", imported.Files, len(want))
	}
	exported, dir := exportFiles(t, db)
	if exported != imported {
		t.Fatalf("export %+v differs from import %+v", exported, imported)
	}
	if got := stateFiles(t, dir); !maps.EqualFunc(got, want, bytes.Equal) {
		t.Fatalf("the files changed in the round trip: %d exported, %d originals", len(got), len(want))
	}

	s, err := OpenStore(db)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if list, err := s.List(fixtureAlice, "work"); err != nil || len(list) != 3 {
		t.Fatalf("alice's conversations: %d %v", len(list), err)
	}
	answered, err := s.Read(fixtureAlice, "work", aliceAnswered)
	if err != nil || !strings.HasPrefix(answered.Title, "Grüße aus 東京") || len(answered.Turns) != 2 || answered.Turns[1].Content != "Zwei Änderungen: 一 the index, 二 the <em>sync</em> & export 🌸." {
		t.Fatalf("alice's answer was lost: %v", err)
	}
	if bound, err := s.OpenBound(fixtureAlice, "work", "slack:C0FIXTURE/1700000000.000100", Options{Mode: "ask"}); err != nil || bound.ID != aliceBound {
		t.Fatalf("the external conversation lost its binding: %v", err)
	}
	streaming, err := s.Read(fixtureAlice, "work", aliceStreaming)
	if err != nil || streaming.Runs[0].State != "interrupted" || streaming.Runs[0].Usage.UncertainTokens != 2400 {
		t.Fatalf("the streaming run was not interrupted with its tokens settled: %+v %v", streaming.Runs, err)
	}
	overlaid, err := s.Read(fixtureBob, "research", bobOverlaid)
	if err != nil || overlaid.Runs[0].State != "interrupted" {
		t.Fatalf("the recovery overlay was lost: %+v %v", overlaid.Runs, err)
	}
	if list, err := s.List(fixtureBob, "research"); err != nil || len(list) != 1 {
		t.Fatalf("bob's visible conversations: %d %v", len(list), err)
	}
	if usage, err := s.PersonalUsage(fixtureBob); err != nil || usage.Total.InputTokens != 1834 {
		t.Fatalf("the deleted conversation's usage was lost: %+v %v", usage, err)
	}
	confirming, err := s.Read(fixtureBob, "work", bobConfirming)
	if err != nil || confirming.Runs[0].State != "awaiting_confirmation" {
		t.Fatalf("bob's run no longer awaits his confirmation: %+v %v", confirming.Runs, err)
	}
	run := Run{ID: NewID(), SubmissionID: "next", State: "queued", RequestHash: "hash", CreatedAt: Now(), Options: Options{Corpus: testCorpus()}}
	turn := Turn{ID: NewID(), Role: "user", Content: "Und jetzt?"}
	if _, _, err = s.Start(fixtureBob, "research", bobOverlaid, run, turn); err == nil {
		t.Fatal("admission ignored the run bob has yet to confirm")
	}
	if _, _, err = s.Start(fixtureAlice, "work", aliceAnswered, run, turn); err != nil {
		t.Fatal("alice's interrupted run still holds admission:", err)
	}

	// The run this gateway now holds exports as the file-based release reads
	// it, and the export imports again.
	_, later := exportFiles(t, db)
	raw, err := os.ReadFile(fixturePath(later, fixtureAlice, aliceAnswered, recordFile))
	if err != nil || bytes.Contains(raw, []byte(`"holder"`)) || !bytes.Contains(raw, []byte(`"state":"queued"`)) {
		t.Fatalf("the held run did not export as the file-based release writes it: %v", err)
	}
	if _, err = importFiles(t, pgtest.Org(t), later); err != nil {
		t.Fatal(err)
	}
}

// A full record whose run a stopped gateway held exports without its holder,
// and the overlay that interrupts the run stays bound to the exported bytes.
func TestChatExportRebindsRecoveryWithoutHolders(t *testing.T) {
	db := pgtest.Org(t)
	other, leave := pgtest.Gateway(t, db)
	elsewhere, err := OpenStore(other)
	if err != nil {
		t.Fatal(err)
	}
	c, err := elsewhere.Create("alice", "alpha", Options{Mode: "ask"})
	if err != nil {
		t.Fatal(err)
	}
	run := Run{ID: NewID(), SubmissionID: "one", State: "queued", RequestHash: "hash", CreatedAt: Now(), Options: Options{Corpus: testCorpus()}}
	if _, _, err = elsewhere.Start("alice", "alpha", c.ID, run, Turn{ID: "turn", Role: "user", Content: "hello"}); err != nil {
		t.Fatal(err)
	}
	s, err := OpenStore(db)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	elsewhere.Close()
	leave()
	// The record has no room for the interruption's event.
	if err = s.SetLimits(Limits{MaxEvents: 2}, nil); err != nil {
		t.Fatal(err)
	}
	if _, recovery, _, err := s.fetch("alice", c.ID); err != nil || recovery == nil {
		t.Fatalf("recovery left no overlay: %v", err)
	}
	_, dir := exportFiles(t, db)
	record, err := os.ReadFile(fixturePath(dir, "alice", c.ID, recordFile))
	if err != nil || bytes.Contains(record, []byte(`"holder"`)) {
		t.Fatalf("the export kept the run's holder: %v", err)
	}
	raw, err := os.ReadFile(fixturePath(dir, "alice", c.ID, recoveryFile))
	var overlay recoveryRecord
	if err != nil || json.Unmarshal(raw, &overlay) != nil || overlay.Hash != Hash(record) || len(overlay.Runs) != 1 || overlay.Runs[0] != run.ID {
		t.Fatalf("the overlay is not bound to the exported record: %s %v", raw, err)
	}
	var reader Store
	read, err := reader.decode("alice", "alpha", c.ID, record, raw)
	if err != nil || read.Runs[0].State != "interrupted" {
		t.Fatalf("the exported run does not read as interrupted: %v", err)
	}
}

// An import refuses what the file-based release could not read and writes
// nothing.
func TestChatImportRefusesUnreadableFiles(t *testing.T) {
	cases := map[string]func(data string){
		"trailing data": func(data string) {
			path := fixturePath(data, fixtureAlice, aliceAnswered, recordFile)
			raw, _ := os.ReadFile(path)
			os.WriteFile(path, append(raw, `{"trailing":true}`...), 0600)
		},
		"corrupt overlay": func(data string) {
			os.WriteFile(fixturePath(data, fixtureBob, bobOverlaid, recoveryFile), []byte("{"), 0600)
		},
		"another conversation's record": func(data string) {
			raw, _ := os.ReadFile(fixturePath(data, fixtureAlice, aliceAnswered, recordFile))
			os.WriteFile(fixturePath(data, fixtureAlice, aliceBound, recordFile), raw, 0600)
		},
		"another account's record": func(data string) {
			os.Rename(filepath.Dir(fixturePath(data, fixtureAlice, aliceBound, recordFile)), filepath.Dir(fixturePath(data, fixtureBob, aliceBound, recordFile)))
		},
		"a run naming a gateway": func(data string) {
			path := fixturePath(data, fixtureAlice, aliceStreaming, recordFile)
			raw, _ := os.ReadFile(path)
			os.WriteFile(path, bytes.Replace(raw, []byte(`"state":"running"`), []byte(`"holder":"gateway","state":"running"`), 1), 0600)
		},
		"missing record": func(data string) {
			os.Remove(fixturePath(data, fixtureAlice, aliceBound, recordFile))
		},
		"unknown file": func(data string) {
			os.WriteFile(filepath.Join(data, "chat", "users", Hash([]byte(fixtureAlice)), "notes.txt"), []byte("unread"), 0600)
		},
	}
	for name, corrupt := range cases {
		t.Run(name, func(t *testing.T) {
			data := releaseA(t)
			corrupt(data)
			db := pgtest.Org(t)
			_, err := importFiles(t, db, data)
			if err == nil || !strings.Contains(err.Error(), "chat/users/") || strings.Contains(err.Error(), "Grüße") {
				t.Fatalf("the import did not refuse it by its path alone: %v", err)
			}
			var n int
			if err = db.Pool.QueryRow(context.Background(), "SELECT count(*) FROM chat_conversations WHERE org = $1", db.Org).Scan(&n); err != nil || n != 0 {
				t.Fatal("a refused import wrote conversations", n, err)
			}
		})
	}
	if sum, err := importFiles(t, pgtest.Org(t), t.TempDir()); err != nil || sum.Files != 0 {
		t.Fatal("an instance without chat failed to import", sum, err)
	}
}
