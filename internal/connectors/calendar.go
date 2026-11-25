package connectors

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"
)

// CalendarConfig names the one calendar a calendar connection reads and the
// Google account authorized to read it. The connection only reads.
type CalendarConfig struct {
	Account  string `json:"account"`
	Calendar string `json:"calendar"`
}

// calendarMaxDays bounds the window one events request covers.
const calendarMaxDays = 366

func checkCalendarID(id string) error {
	if id == "" || len(id) > 254 || !strings.Contains(id, "@") || strings.ContainsFunc(id, func(r rune) bool { return r <= ' ' || r == 0x7f || r == '/' || r == '\\' }) {
		return errors.New("Calendar requires the ID of the calendar it reads, such as name@group.calendar.google.com")
	}
	return nil
}

// calendarCall lists the configured calendar's events in a date window, one
// page at a time. Each event keeps only its ID, title and dates: an all-day
// event has dates, YYYY-MM-DD with an exclusive end; others have RFC 3339
// times. Descriptions, attendees and cancelled events are never returned.
func (r *runtime) calendarCall(ctx context.Context, token, name string, in map[string]any) (any, error) {
	if name != "events" {
		return nil, ErrInvalid
	}
	from, fromErr := time.Parse(time.DateOnly, str(in["from"]))
	to, toErr := time.Parse(time.DateOnly, str(in["to"]))
	if fromErr != nil || toErr != nil || !to.After(from) || to.Sub(from) > calendarMaxDays*24*time.Hour {
		return nil, errors.New("events need from and to dates, YYYY-MM-DD, from 1 to 366 days apart")
	}
	query := url.Values{
		"timeMin":      {from.Format(time.RFC3339)},
		"timeMax":      {to.Format(time.RFC3339)},
		"singleEvents": {"true"},
		"orderBy":      {"startTime"},
		"maxResults":   {"250"},
		"fields":       {"items(id,status,summary,start,end),nextPageToken,timeZone"},
	}
	if page := str(in["pageToken"]); page != "" {
		if len(page) > 2048 {
			return nil, errors.New("invalid page token")
		}
		query.Set("pageToken", page)
	}
	result, err := r.googleJSON(ctx, token, "GET", "/calendar/v3/calendars/"+url.PathEscape(r.config.Calendar.Calendar)+"/events", query, nil)
	if err != nil {
		return nil, err
	}
	events := []any{}
	for _, raw := range array(result["items"]) {
		item := object(raw)
		if str(item["status"]) == "cancelled" {
			continue
		}
		start, end := object(item["start"]), object(item["end"])
		event := map[string]any{"id": str(item["id"]), "title": str(item["summary"]), "allDay": false, "start": str(start["dateTime"]), "end": str(end["dateTime"])}
		if day := str(start["date"]); day != "" {
			event["allDay"], event["start"], event["end"] = true, day, str(end["date"])
		}
		events = append(events, event)
	}
	next := str(result["nextPageToken"])
	out := map[string]any{"calendar": r.config.Calendar.Calendar, "timeZone": str(result["timeZone"]), "events": events, "hasMore": next != ""}
	if next != "" {
		out["nextPageToken"] = next
	}
	return out, nil
}
