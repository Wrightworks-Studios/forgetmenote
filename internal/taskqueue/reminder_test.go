package taskqueue

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func TestReminderStateExpiresByDate(t *testing.T) {
	st := ReadReminderState(json.RawMessage(`{"dismissed":{"a":"2026-11-19","b":"2026-11-20"},"checked":{"p-old":"2026-11-18","p-new":"2026-11-19"},"reminded":{"p-gone":"2026-10-18","p-away":"2026-10-19"},"other":1}`))
	// At 01:00 UTC on Nov 20 it is still Nov 19 west of UTC, so that day's
	// mark stays; a dismissal lasts through its date, and a reminder as long
	// as the catch-up list looks back.
	now := time.Date(2026, 11, 20, 1, 0, 0, 0, time.UTC)
	if !st.expire(now) || fmt.Sprint(st.Dismissed, st.Checked, st.Reminded) != "map[b:2026-11-20] map[p-new:2026-11-19] map[p-away:2026-10-19]" || st.expire(now) {
		t.Fatalf("expired to %+v", st)
	}
	if st := ReadReminderState(json.RawMessage(`not json`)); st.Dismissed == nil || st.Checked == nil || st.Reminded == nil {
		t.Fatal("unreadable state gave nil maps")
	}
}
