package tasks

import (
	"cmp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Reminder is a task its owner should see on a working day, and why.
type Reminder struct {
	Task   Task
	Reason string
}

// Weekday reports whether day is Monday to Friday.
func Weekday(day time.Time) bool {
	return day.Weekday() != time.Saturday && day.Weekday() != time.Sunday
}

// addWorkingDays returns the date n working days after day.
func addWorkingDays(day time.Time, n int, working func(time.Time) bool) time.Time {
	for i := 0; n > 0 && i < 366; i++ {
		day = day.AddDate(0, 0, 1)
		if working(day) {
			n--
		}
	}
	return day
}

// Remind ranks the tasks to show their owner on day, a date: overdue first,
// then due within two working days, each soonest first; then the rest by
// priority (high, normal, low), oldest first. A snoozed task waits for its
// date, and a recurring task waits until its lead time before the due date:
// its own, or one working day for daily and weekly schedules and five for
// longer ones.
// working reports working days; nil means Monday to Friday.
func (q Queue) Remind(day time.Time, working func(time.Time) bool) []Reminder {
	if working == nil {
		working = Weekday
	}
	day = time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC)
	today := day.Format(time.DateOnly)
	soon := addWorkingDays(day, 2, working)
	type ranked struct {
		Reminder
		tier int
		key  string
	}
	out := []ranked{}
	for _, t := range q.Tasks {
		if t.Snoozed > today {
			continue
		}
		due, err := time.Parse(time.DateOnly, t.Due)
		hasDue := err == nil
		if t.Every != nil && hasDue {
			lead := 5
			if t.Every.Unit == "days" || t.Every.Unit == "weeks" {
				lead = 1
			}
			if t.Lead > 0 {
				lead = t.Lead
			}
			if due.After(addWorkingDays(day, lead, working)) {
				continue
			}
		}
		r := ranked{Reminder: Reminder{Task: t}, key: t.Due}
		switch {
		case hasDue && due.Before(day):
			r.Reason = "overdue since " + shortDate(due, day)
		case hasDue && !due.After(soon):
			r.tier, r.Reason = 1, "due "+relativeDate(due, day)
		default:
			r.tier, r.key = 3, t.Added
			parts := []string{}
			switch t.Priority {
			case "high":
				r.tier = 2
				parts = append(parts, "high priority")
			case "low":
				r.tier = 4
				parts = append(parts, "low priority")
			}
			if hasDue {
				parts = append(parts, "due "+shortDate(due, day))
			} else if added, err := time.Parse(time.DateOnly, t.Added); err == nil {
				parts = append(parts, waiting(int(day.Sub(added).Hours()/24)))
			}
			r.Reason = strings.Join(parts, ", ")
		}
		out = append(out, r)
	}
	slices.SortStableFunc(out, func(a, b ranked) int {
		return cmp.Or(cmp.Compare(a.tier, b.tier), cmp.Compare(a.key, b.key), cmp.Compare(a.Task.ID, b.Task.ID))
	})
	reminders := make([]Reminder, len(out))
	for i, r := range out {
		reminders[i] = r.Reminder
	}
	return reminders
}

func shortDate(d, today time.Time) string {
	if d.Year() != today.Year() {
		return d.Format("Mon Jan 2, 2006")
	}
	return d.Format("Mon Jan 2")
}

func relativeDate(d, today time.Time) string {
	switch {
	case d.Equal(today):
		return "today"
	case d.Equal(today.AddDate(0, 0, 1)):
		return "tomorrow"
	}
	return shortDate(d, today)
}

func waiting(days int) string {
	switch {
	case days <= 0:
		return "added today"
	case days == 1:
		return "waiting 1 day"
	}
	return "waiting " + strconv.Itoa(days) + " days"
}
