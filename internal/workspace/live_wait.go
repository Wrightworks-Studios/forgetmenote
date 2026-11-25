package workspace

import (
	"errors"
	"strings"
)

// maxLiveWaiters bounds the long polls one notebook's store serves at once:
// each gateway's, since every gateway has its own.
const maxLiveWaiters = 128

// ErrLiveCapacity refuses a long poll past maxLiveWaiters. Nothing was read
// or changed, so the caller retries later.
var ErrLiveCapacity = errors.New("live subscription limit reached")

// liveWaiter holds the long polls waiting on one note.
type liveWaiter struct {
	readers map[*liveReader]bool
}

// liveReader is one long poll: the browser tab it serves, what the tab
// knows (a LivePoll known), and the channel a change closes to wake it.
// The notebook mutex guards known and changed.
type liveReader struct {
	tab     string
	known   string
	changed chan struct{}
}

func (r *liveReader) wake() {
	close(r.changed)
	r.changed = make(chan struct{})
}

// liveTabAnswer is the answer to a tab's update: from the live revision the
// tab named, to what it knows once it has the answer.
type liveTabAnswer struct {
	from, known string
}

// liveTabs bounds the tabs whose answers a note's record remembers.
const liveTabs = 64

// Called under the notebook mutex, after the durable write. Hints coalesce;
// LivePoll owns validation and catch-up rather than trusting notification data.
func (s *Store) notifyLive(path string) {
	for key, waiter := range s.liveWaits {
		if path == "" || key == path {
			for r := range waiter.readers {
				r.wake()
			}
		}
	}
}

// liveAnswered records that tab, which held live revision since, was
// answered with v, its note's current record, and moves tab's long polls
// that knew since on to v: their tab gets all of it in that answer, so they
// wait for what follows instead of answering the tab with its own update.
// With wake, it also wakes every other long poll on the note that does not
// know v. Called under the notebook mutex.
func (s *Store) liveAnswered(v *liveRecord, tab, since string, wake bool) {
	known, advance := v.known(), tab != "" && since != ""
	if advance {
		if v.tabs == nil {
			v.tabs = map[string]liveTabAnswer{}
		}
		if _, ok := v.tabs[tab]; !ok && len(v.tabs) >= liveTabs {
			for other := range v.tabs {
				delete(v.tabs, other)
				break
			}
		}
		v.tabs[tab] = liveTabAnswer{from: since, known: known}
	}
	waiter := s.liveWaits[v.Path]
	if waiter == nil {
		return
	}
	for r := range waiter.readers {
		if advance && r.tab == tab && liveKnownRevision(r.known) == since {
			r.known = known
		}
		if wake && r.known != known {
			r.wake()
		}
	}
}

// liveTabKnown is what tab knows once it has the answer to its last update,
// when known is what it held before: a long poll the tab started before the
// answer arrived waits for what follows it.
func (s *Store) liveTabKnown(path, tab, known string) string {
	if v := s.live[path]; v != nil && tab != "" {
		if a, ok := v.tabs[tab]; ok && a.from == liveKnownRevision(known) {
			return a.known
		}
	}
	return known
}

// liveKnownRevision is the live revision a LivePoll known names.
func liveKnownRevision(known string) string {
	revision, _, _ := strings.Cut(known, "/")
	return revision
}
