package quicknotes

import (
	"context"
	"testing"

	"forgetmenote/internal/pg/pgtest"
	"forgetmenote/internal/workspace"
)

func TestScanLeavesAnotherLiveGatewaysSaveUntilItIsGone(t *testing.T) {
	s, c, _ := fixture(t)
	stopScanWorker(t, s)
	ctx, account, err := s.actor(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	inbox, err := c.EnsureInbox("alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Access("alice", inbox.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	restore := pgtest.FailTable(t, s.db, "revisions")
	const operation = "another-gateway-save"
	if _, err = s.Capture(ctx, "alice", operation, "synthetic draft"); err == nil {
		t.Fatal("capture saved while the notes store refused commits")
	}
	restore()
	id := workspace.ContentHash([]byte(account.ID + "\x00" + operation))[:32]
	saving, err := s.readRecord(account.ID, id)
	if err != nil || saving.Status != "saving" {
		t.Fatal("fixture capture is not saving", saving.Status, err)
	}
	// The save is another gateway's, still in progress there.
	holder, gone := pgtest.Gateway(t, s.db)
	saving.Holder = holder.Holder
	if err = s.writeRecord(&saving); err != nil {
		t.Fatal(err)
	}
	s.scan(s.ctx)
	if got, err := s.readRecord(account.ID, id); err != nil || got.AwaitingClient || got.RetryFailures != 0 {
		t.Fatal("scan recovered a save another live gateway holds", err)
	}
	gone()
	s.scan(s.ctx)
	if got, err := s.readRecord(account.ID, id); err != nil || !got.AwaitingClient {
		t.Fatal("scan left the save of a gateway that is gone", err)
	}
}
