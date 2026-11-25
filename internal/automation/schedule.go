package automation

import (
	"errors"
	"fmt"
	"slices"
	"time"
	_ "time/tzdata"
)

// Schedule deliberately describes calendar slots, not an executable cron string.
// Empty hours/weekdays match every hour/day; minutes must be explicit. Repeated
// DST wall-clock slots run once. Nonexistent spring-forward slots are skipped.
type Schedule struct {
	Timezone       string `json:"timezone,omitempty"`
	Minutes        []int  `json:"minutes,omitempty"`
	Hours          []int  `json:"hours,omitempty"`
	Weekdays       []int  `json:"weekdays,omitempty"`
	Once           string `json:"once,omitempty"`
	CatchUpSeconds int    `json:"catchUpSeconds,omitempty"`
}

func (s *Schedule) validate() error {
	if s == nil {
		return nil
	}
	if s.CatchUpSeconds < 0 || s.CatchUpSeconds > 86400 {
		return errors.New("calendar catch-up must be at most one day")
	}
	if s.Once != "" {
		if _, err := time.Parse(time.RFC3339, s.Once); err != nil {
			return errors.New("one-shot schedule requires an RFC3339 time")
		}
		if s.Timezone != "" || len(s.Minutes)+len(s.Hours)+len(s.Weekdays) != 0 {
			return errors.New("one-shot and calendar schedules cannot be combined")
		}
		return nil
	}
	if s.Timezone == "" || len(s.Minutes) == 0 {
		return errors.New("calendar requires timezone and minutes")
	}
	if _, err := time.LoadLocation(s.Timezone); err != nil {
		return errors.New("invalid calendar timezone")
	}
	for _, v := range []struct {
		values []int
		max    int
	}{{s.Minutes, 59}, {s.Hours, 23}, {s.Weekdays, 6}} {
		seen := map[int]bool{}
		for _, n := range v.values {
			if n < 0 || n > v.max || seen[n] {
				return errors.New("invalid or duplicate calendar value")
			}
			seen[n] = true
		}
	}
	return nil
}
func (s Spec) Slot(now time.Time) (string, time.Time, bool) {
	if s.Disabled {
		return "", time.Time{}, false
	}
	if s.IntervalSeconds > 0 {
		t := time.Unix(now.Unix()/int64(s.IntervalSeconds)*int64(s.IntervalSeconds), 0)
		return scheduledKey(s, now), t, true
	}
	if s.Schedule == nil {
		return "", time.Time{}, false
	}
	c := s.Schedule
	window := time.Duration(max(60, c.CatchUpSeconds)) * time.Second
	if c.Once != "" {
		t, err := time.Parse(time.RFC3339, c.Once)
		return "once-" + c.Once, t, err == nil && !now.Before(t) && now.Sub(t) < window
	}
	loc, err := time.LoadLocation(c.Timezone)
	if err != nil {
		return "", time.Time{}, false
	}
	for t := now.Truncate(time.Minute); !t.Before(now.Add(-window)); t = t.Add(-time.Minute) {
		local := t.In(loc)
		if slices.Contains(c.Minutes, local.Minute()) && (len(c.Hours) == 0 || slices.Contains(c.Hours, local.Hour())) && (len(c.Weekdays) == 0 || slices.Contains(c.Weekdays, int(local.Weekday()))) {
			return fmt.Sprintf("calendar-%s-%s", c.Timezone, local.Format("2006-01-02T15:04")), t, true
		}
	}
	return "", time.Time{}, false
}
func (s Spec) Next(now time.Time) string {
	if s.Disabled {
		return ""
	}
	if s.IntervalSeconds > 0 {
		return time.Unix((now.Unix()/int64(s.IntervalSeconds)+1)*int64(s.IntervalSeconds), 0).UTC().Format(time.RFC3339)
	}
	if s.Schedule == nil {
		return ""
	}
	if s.Schedule.Once != "" {
		t, err := time.Parse(time.RFC3339, s.Schedule.Once)
		if err == nil && t.After(now) {
			return t.UTC().Format(time.RFC3339)
		}
		return ""
	}
	loc, err := time.LoadLocation(s.Schedule.Timezone)
	if err != nil {
		return ""
	}
	c := s.Schedule
	for t := now.Truncate(time.Minute).Add(time.Minute); t.Before(now.Add(8 * 24 * time.Hour)); t = t.Add(time.Minute) {
		l := t.In(loc)
		if slices.Contains(c.Minutes, l.Minute()) && (len(c.Hours) == 0 || slices.Contains(c.Hours, l.Hour())) && (len(c.Weekdays) == 0 || slices.Contains(c.Weekdays, int(l.Weekday()))) {
			return t.UTC().Format(time.RFC3339)
		}
	}
	return ""
}
