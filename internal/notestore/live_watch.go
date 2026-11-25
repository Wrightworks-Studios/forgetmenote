package notestore

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"forgetmenote/internal/gitobj"

	"github.com/jackc/pgx/v5"
)

// liveChannel is the channel gateways hear live documents change on. Its
// subject is notebook/entry/epoch/seq/base, the fields of a LiveChange.
const liveChannel = "notebook_live"

// A LiveChange tells of a live document's new update, compaction or epoch:
// the epoch and newest sequence number it then had and, for a compaction,
// the revision its snapshot is based on.
type LiveChange struct {
	EntryID string
	Epoch   int64
	Seq     int64
	Base    gitobj.ID
}

// WatchLive calls fn with every change to a live document of any notebook,
// this gateway's and other gateways' alike, as they are announced, and with
// the empty notebook and the zero LiveChange each time the listener
// (re)connects, when changes may have been missed. Calls come from one
// goroutine, after the change committed; they are hints, so fn reads the
// document again for anything it needs.
func (s *Store) WatchLive(fn func(notebook string, c LiveChange)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.live = append(s.live, fn)
}

// liveChanged announces c. A lost announcement only delays other gateways,
// which hear every change again after the listener reconnects.
func (s *Store) liveChanged(notebook string, c LiveChange) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s.db.Announce(ctx, liveChannel, fmt.Sprintf("%s/%s/%d/%d/%s", notebook, c.EntryID, c.Epoch, c.Seq, c.Base))
}

func (s *Store) heardLive(subject string) {
	var notebook string
	var c LiveChange
	if subject != "" {
		fields := strings.Split(subject, "/")
		if len(fields) != 5 || !notebookID.MatchString(fields[0]) || !validEntryID(fields[1]) || fields[1] == "" {
			return
		}
		epoch, e1 := strconv.ParseInt(fields[2], 10, 64)
		seq, e2 := strconv.ParseInt(fields[3], 10, 64)
		base, e3 := gitobj.ParseID(fields[4])
		if e1 != nil || e2 != nil || e3 != nil {
			return
		}
		notebook, c = fields[0], LiveChange{EntryID: fields[1], Epoch: epoch, Seq: seq, Base: base}
	}
	s.mu.Lock()
	watchers := slices.Clone(s.live)
	s.mu.Unlock()
	for _, fn := range watchers {
		fn(notebook, c)
	}
}

// A LiveDoc describes a stored live document without its bytes: its epoch,
// whether it has a snapshot, the revision that snapshot is based on, the
// sequence number the snapshot reaches and the newest update's.
type LiveDoc struct {
	EntryID     string
	Epoch       int64
	Snapshot    bool
	Base        gitobj.ID
	SnapshotSeq int64
	LastSeq     int64
}

// LiveDocs lists the notebook's live documents by entry ID.
func (n *Notebook) LiveDocs(ctx context.Context) ([]LiveDoc, error) {
	rows, err := n.s.db.Pool.Query(ctx, `SELECT entry_id, epoch, snapshot IS NOT NULL, base_version, snapshot_seq, last_seq FROM live_docs
		WHERE org = $1 AND notebook = $2 ORDER BY entry_id`, n.s.db.Org, n.id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (LiveDoc, error) {
		var d LiveDoc
		var base []byte
		err := row.Scan(&d.EntryID, &d.Epoch, &d.Snapshot, &base, &d.SnapshotSeq, &d.LastSeq)
		d.Base = toID(base)
		return d, err
	})
}
