package benchfixture

import (
	"slices"
	"time"
)

type moment struct {
	at    int64 // seconds after start
	actor int
}

// timeline returns n strictly increasing commit times within [start, end) and
// the acting actor of each. Commits cluster into sessions of one actor, mostly
// on weekdays during that actor's working hours, with gaps of seconds to
// minutes inside a session and hours to days between sessions.
func timeline(r *rng, n int, start, end time.Time, weights []int, maxSession int) ([]time.Time, []int) {
	span := int64(end.Sub(start) / time.Second)
	days := int(span / 86400)
	if days < 1 || span < int64(n) {
		panic("benchfixture: timeline span too short")
	}
	var moments []moment
	for len(moments) < n {
		actor := r.weighted(weights)
		_, offset := start.In(Actors[actor].Zone).Zone()
		var day int
		for {
			day = r.intn(days)
			weekday := (int(start.Weekday()) + day) % 7
			if weekday != 0 && weekday != 6 || r.permille(300) {
				break
			}
		}
		hour := r.between(9, 17)
		if p := r.intn(10); p < 2 {
			hour = r.between(19, 22)
		} else if p == 2 {
			hour = r.between(6, 8)
		}
		at := int64(day)*86400 + int64(hour*3600+r.intn(3600)-offset)
		size := r.between(1, maxSession)
		for range size {
			moments = append(moments, moment{at, actor})
			gap := int64(r.between(15, 400))
			if r.permille(100) {
				gap = int64(r.between(400, 1500))
			}
			at += gap
		}
	}
	slices.SortStableFunc(moments, func(a, b moment) int {
		switch {
		case a.at < b.at:
			return -1
		case a.at > b.at:
			return 1
		}
		return 0
	})
	moments = moments[:n]
	times, actors := make([]time.Time, n), make([]int, n)
	last := int64(-1)
	for i, m := range moments {
		at := min(max(m.at, last+1, 0), span-int64(n-i))
		last = at
		times[i], actors[i] = start.Add(time.Duration(at)*time.Second), m.actor
	}
	return times, actors
}
