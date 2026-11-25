package notestore_test

import (
	"errors"
	"testing"

	"forgetmenote/internal/notestore"
)

// A done receipt never becomes pending again, and PendingReceipts reads only
// the receipts still pending.
func TestDoneReceiptsStayDone(t *testing.T) {
	e := newEnv(t, notestore.Options{})
	n := e.create("notes")
	done := notestore.Receipt{Kind: "capture", Operation: "a", RequestHash: "r", State: notestore.ReceiptDone, Result: []byte(`"canceled"`)}
	if err := n.PutReceipt(e.ctx, done); err != nil {
		t.Fatal(err)
	}
	if err := n.PutReceipt(e.ctx, notestore.Receipt{Kind: "capture", Operation: "a", RequestHash: "r", Result: []byte(`"receiving"`)}); !errors.Is(err, notestore.ErrOperationMismatch) {
		t.Fatal("a pending receipt replaced a done one:", err)
	}
	if err := n.PutReceipt(e.ctx, done); err != nil {
		t.Fatal("repeating a done receipt:", err)
	}
	if err := n.PutReceipt(e.ctx, notestore.Receipt{Kind: "capture", Operation: "b", RequestHash: "r", Result: []byte(`"prepared"`)}); err != nil {
		t.Fatal(err)
	}
	pending, err := n.PendingReceipts(e.ctx, "capture")
	if err != nil || len(pending) != 1 || pending[0].Operation != "b" {
		t.Fatalf("pending receipts: %+v %v", pending, err)
	}
	if r, _, err := n.Receipt(e.ctx, "capture", "a"); err != nil || string(r.Result) != `"canceled"` || r.State != notestore.ReceiptDone {
		t.Fatalf("the done receipt: %+v %v", r, err)
	}
}

// A change set fenced on a receipt commits only while that receipt is
// pending, and one fenced on the authority record only while the record is
// at the version it names.
func TestApplyFencesOnReceiptsAndTheAuthorityRecord(t *testing.T) {
	e := newEnv(t, notestore.Options{})
	n := e.create("notes")
	key := &notestore.ReceiptKey{Kind: "capture", Operation: "op"}
	none := int64(0)
	if _, err := n.Apply(e.ctx, notestore.ChangeSet{Subject: "x", Author: ada, Pending: key, Files: []notestore.FileChange{write("a.md", "a")}}); !errors.Is(err, notestore.ErrConflict) {
		t.Fatal("a commit fenced on a missing receipt:", err)
	}
	if err := n.PutReceipt(e.ctx, notestore.Receipt{Kind: "capture", Operation: "op", RequestHash: "r"}); err != nil {
		t.Fatal(err)
	}
	apply(t, n, notestore.ChangeSet{Pending: key, ExpectAuthority: &none, Files: []notestore.FileChange{write("a.md", "a")}})
	err := n.SettleReceipt(e.ctx, "", func(s notestore.Settlement) (notestore.Receipt, error) {
		return notestore.Receipt{Kind: "capture", Operation: "op", RequestHash: "r", State: notestore.ReceiptDone}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := n.Apply(e.ctx, notestore.ChangeSet{Subject: "x", Author: ada, Pending: key, Files: []notestore.FileChange{write("b.md", "b")}}); !errors.Is(err, notestore.ErrConflict) {
		t.Fatal("a commit fenced on a settled receipt:", err)
	}
	version, err := n.PutAuthority(e.ctx, notestore.AuthorityWrite{State: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := n.Apply(e.ctx, notestore.ChangeSet{Subject: "x", Author: ada, ExpectAuthority: &none, Files: []notestore.FileChange{write("b.md", "b")}}); !errors.Is(err, notestore.ErrAuthorityChanged) {
		t.Fatal("a commit fenced on a replaced authority record:", err)
	}
	apply(t, n, notestore.ChangeSet{ExpectAuthority: &version, Files: []notestore.FileChange{write("b.md", "b")}})
}

// An authority record that refuses pending receipts is not stored while one
// is; SettleReceipt sees the record's version and the revision an operation
// committed, under the head's lock.
func TestAuthorityRecordsAndSettledReceiptsSeeEachOther(t *testing.T) {
	e := newEnv(t, notestore.Options{})
	n := e.create("notes")
	err := n.SettleReceipt(e.ctx, "op", func(s notestore.Settlement) (notestore.Receipt, error) {
		if s.Committed != nil || s.Authority != 0 {
			t.Errorf("settlement of a new notebook: %+v", s)
		}
		return notestore.Receipt{Kind: "capture", Operation: "op", RequestHash: "r"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := n.PutAuthority(e.ctx, notestore.AuthorityWrite{State: []byte(`{}`), RefusePending: "capture"}); !errors.Is(err, notestore.ErrPending) {
		t.Fatal("an authority record stored while a receipt was pending:", err)
	}
	committed := apply(t, n, notestore.ChangeSet{OperationID: "op", RequestID: "req", Files: []notestore.FileChange{write("a.md", "a")}})
	refused := errors.New("refused")
	err = n.SettleReceipt(e.ctx, "op", func(s notestore.Settlement) (notestore.Receipt, error) {
		if s.Committed == nil || s.Committed.ID != committed.Head.Commit {
			t.Errorf("the settlement does not see the operation's revision: %+v", s.Committed)
		}
		return notestore.Receipt{}, refused
	})
	if !errors.Is(err, refused) {
		t.Fatal(err)
	}
	if r, _, err := n.Receipt(e.ctx, "capture", "op"); err != nil || r.State != notestore.ReceiptPending {
		t.Fatalf("a refused settlement stored its receipt: %+v %v", r, err)
	}
	err = n.SettleReceipt(e.ctx, "", func(s notestore.Settlement) (notestore.Receipt, error) {
		return notestore.Receipt{Kind: "capture", Operation: "op", RequestHash: "r", State: notestore.ReceiptDone}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	version, err := n.PutAuthority(e.ctx, notestore.AuthorityWrite{State: []byte(`{}`), RefusePending: "capture"})
	if err != nil {
		t.Fatal(err)
	}
	err = n.SettleReceipt(e.ctx, "", func(s notestore.Settlement) (notestore.Receipt, error) {
		if s.Authority != version {
			t.Errorf("settlement sees authority version %d, not %d", s.Authority, version)
		}
		return notestore.Receipt{Kind: "capture", Operation: "other", RequestHash: "r"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
