package notestore_test

import (
	"testing"
	"time"

	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
)

// Every gateway hears each update, compaction and new epoch of a live
// document, and the notebook lists its live documents without their bytes.
func TestLiveChangesReachOtherGatewaysAndDocumentsAreListed(t *testing.T) {
	e := newEnv(t, notestore.Options{})
	n := e.create("notes")
	other := e.open(e.db, notestore.Options{})
	listening := make(chan struct{}, 1)
	heard := make(chan notestore.LiveChange, 16)
	other.WatchLive(func(notebook string, c notestore.LiveChange) {
		if notebook == "" {
			select {
			case listening <- struct{}{}:
			default:
			}
			return
		}
		if notebook == "notes" {
			heard <- c
		}
	})
	select {
	case <-listening:
	case <-time.After(10 * time.Second):
		t.Fatal("the other gateway never listened")
	}
	next := func() notestore.LiveChange {
		t.Helper()
		select {
		case c := <-heard:
			return c
		case <-time.After(10 * time.Second):
			t.Fatal("the other gateway heard no change")
			return notestore.LiveChange{}
		}
	}
	const entry = "0123456789abcdef0123456789abcdef"
	seq, err := n.LiveAppend(e.ctx, entry, 0, []byte("u1"), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if c := next(); c != (notestore.LiveChange{EntryID: entry, Epoch: 0, Seq: seq}) {
		t.Fatalf("append: %+v", c)
	}
	base := gitobj.BlobID([]byte("base"))
	if err := n.LiveCompact(e.ctx, entry, 0, []byte("snapshot"), seq, base); err != nil {
		t.Fatal(err)
	}
	if c := next(); c != (notestore.LiveChange{EntryID: entry, Epoch: 0, Seq: seq, Base: base}) {
		t.Fatalf("compaction: %+v", c)
	}
	docs, err := n.LiveDocs(e.ctx)
	if err != nil || len(docs) != 1 || docs[0] != (notestore.LiveDoc{EntryID: entry, Epoch: 0, Snapshot: true, Base: base, SnapshotSeq: seq, LastSeq: seq}) {
		t.Fatalf("listed: %+v %v", docs, err)
	}
	epoch, err := n.LiveReset(e.ctx, entry)
	if err != nil {
		t.Fatal(err)
	}
	if c := next(); c != (notestore.LiveChange{EntryID: entry, Epoch: epoch, Seq: seq}) {
		t.Fatalf("reset: %+v", c)
	}
	docs, err = n.LiveDocs(e.ctx)
	if err != nil || len(docs) != 1 || docs[0] != (notestore.LiveDoc{EntryID: entry, Epoch: epoch, SnapshotSeq: seq, LastSeq: seq}) {
		t.Fatalf("listed after a reset: %+v %v", docs, err)
	}
	if docs, err = e.create("empty").LiveDocs(e.ctx); err != nil || len(docs) != 0 {
		t.Fatalf("a notebook without live documents: %+v %v", docs, err)
	}
}
