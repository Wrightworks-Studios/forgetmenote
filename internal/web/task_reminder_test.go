package web

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"forgetmenote/internal/automation"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/connectors"
	"forgetmenote/internal/taskqueue"
)

func TestWeekdayRemindersReachOwnersWhoAreIn(t *testing.T) {
	f := newTaskFixture(t)
	app := f.app
	// Tuesday 16:30 UTC is 09:30 in Denver.
	now := time.Date(2026, 11, 17, 16, 30, 0, 0, time.UTC)
	app.tasks.Clock = func() time.Time { return now }
	f.members = []map[string]any{
		{"id": "UALICE", "team_id": "T123", "tz": "America/Denver", "profile": map[string]any{"status_text": "In a meeting"}},
		{"id": "UBOB", "team_id": "T123", "tz": "America/Denver", "profile": map[string]any{"status_text": "Vacationing", "status_emoji": ":palm_tree:"}},
	}
	clients := taskClients(t, app, "alice", "bob")
	alice, bob := clients[0], clients[1]
	copyTask := decodeTask[taskqueue.View](t, actionCall(t, bob, "add_task", map[string]any{"person": "<@UALICE>", "text": "Draft holiday copy"}, false))
	deck := decodeTask[taskqueue.View](t, actionCall(t, bob, "add_task", map[string]any{"person": "<@UALICE>", "text": "Review the Q4 pricing deck", "due": "2026-11-13"}, false))
	actionCall(t, alice, "add_task", map[string]any{"person": "<@UBOB>", "text": "Send the venue list"}, false)

	spec := automation.Spec{ID: "task-reminder", Kind: "native", Actor: f.service, Project: "default", TimeoutSeconds: 60, Native: &automation.NativeTask{Task: "task-reminder", Client: "slack"}}
	if err := app.validateAutomationDefinition(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	other := spec
	other.Actor = catalog.Bootstrap
	if err := app.validateAutomationDefinition(t.Context(), other); err == nil {
		t.Fatal("the reminder ran as an actor other than the client's members actor")
	}
	remind := func(id string, dry bool) []map[string]any {
		t.Helper()
		before := len(f.posts)
		run := automation.Run{ID: automation.Hash(id), DryRun: dry, Spec: spec}
		if err := app.runAutomation(t.Context(), &run, func() error { return nil }); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if strings.Contains(run.Result, "Review the Q4") {
			t.Fatalf("the report carries task text:\n%s", run.Result)
		}
		return f.posts[before:]
	}

	if posts := remind("preview", true); len(posts) != 0 {
		t.Fatalf("a test run sent %v", posts)
	}
	// Alice hears at 09:30; bob is on vacation.
	posts := remind("tuesday", false)
	if len(posts) != 1 || posts[0]["channel"] != "UALICE" {
		t.Fatalf("Tuesday's reminders: %v", posts)
	}
	blocks := fmt.Sprint(posts[0]["blocks"])
	first, second := strings.Index(blocks, "Review the Q4 pricing deck"), strings.Index(blocks, "Draft holiday copy")
	if first < 0 || second < first || !strings.Contains(blocks, "overdue since Fri Nov 13") {
		t.Fatalf("the overdue task does not lead:\n%s", blocks)
	}
	buttons := postButtons(posts[0])
	actions := []string{}
	for _, b := range buttons {
		actions = append(actions, b["action_id"].(string))
	}
	if strings.Join(actions, " ") != "task_done task_snooze task_not_applicable task_done task_snooze task_not_applicable" {
		t.Fatalf("the reminder offers %v", actions)
	}
	// Once a day, on working days, from nine.
	if posts := remind("tuesday-again", false); len(posts) != 0 {
		t.Fatalf("a second run on Tuesday sent %v", posts)
	}
	now = time.Date(2026, 11, 21, 16, 30, 0, 0, time.UTC)
	if posts := remind("saturday", false); len(posts) != 0 {
		t.Fatalf("Saturday's run sent %v", posts)
	}
	now = time.Date(2026, 11, 23, 15, 30, 0, 0, time.UTC)
	if posts := remind("monday-early", false); len(posts) != 0 {
		t.Fatalf("a run at 08:30 sent %v", posts)
	}

	// Snooze hides a task for a week.
	reply := f.press(t, "UALICE", "2.1", buttons[4])
	if !strings.Contains(reply, "Snoozed until Tue Nov 24") {
		t.Fatalf("snooze reply = %q", reply)
	}
	mine := decodeTask[taskqueue.List](t, actionCall(t, alice, "list_tasks", map[string]any{}, false))
	if len(mine.Mine) != 2 || mine.Mine[0].ID != copyTask.ID || mine.Mine[0].Snoozed != "2026-11-24" {
		t.Fatalf("alice's queue after snoozing = %+v", mine.Mine)
	}
	now = time.Date(2026, 11, 23, 16, 5, 0, 0, time.UTC)
	posts = remind("monday", false)
	if len(posts) != 1 || strings.Contains(fmt.Sprint(posts[0]["blocks"]), "Draft holiday copy") {
		t.Fatalf("Monday's reminder: %v", posts)
	}

	// Not applicable asks why, in a modal, and tells the requester.
	w := f.interact(t, buttonPress("UALICE", "3.1", buttons[2], f.upstream+"/actions/T123/2/hook"))
	if w.Code != 200 || len(f.views) != 1 {
		t.Fatalf("not applicable: HTTP %d %s, views %v", w.Code, w.Body, f.views)
	}
	view := f.views[0]["view"].(map[string]any)
	if f.views[0]["trigger_id"] != "trigger-3.1" || view["callback_id"] != taskqueue.NotApplicableView || strings.Contains(fmt.Sprint(view), "Review the Q4") {
		t.Fatalf("the modal = %v", f.views[0])
	}
	submit := func(user, reason string) map[string]any {
		t.Helper()
		w := f.interact(t, map[string]any{"type": "view_submission", "team": map[string]any{"id": "T123"}, "user": map[string]any{"id": user, "team_id": "T123"},
			"view": map[string]any{"id": "V" + user, "callback_id": view["callback_id"], "private_metadata": view["private_metadata"],
				"state": map[string]any{"values": map[string]any{"reason": map[string]any{"reason": map[string]any{"type": "plain_text_input", "value": reason}}}}}})
		var out map[string]any
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatalf("submission: HTTP %d %s", w.Code, w.Body)
		}
		return out
	}
	if out := submit("UBOB", "Not mine to close"); out["response_action"] != "errors" {
		t.Fatalf("the requester closed the task: %v", out)
	}
	before := len(f.postsTo("UBOB"))
	if out := submit("UALICE", "The deck was cancelled"); out["response_action"] != "update" {
		t.Fatalf("the owner's submission: %v", out)
	}
	mine = decodeTask[taskqueue.List](t, actionCall(t, alice, "list_tasks", map[string]any{}, false))
	if len(mine.Mine) != 1 || mine.Mine[0].ID == deck.ID {
		t.Fatalf("alice's queue after not applicable = %+v", mine.Mine)
	}
	if told := f.postsTo("UBOB")[before:]; len(told) != 1 || !strings.Contains(told[0]["text"].(string), "not applicable") || !strings.Contains(told[0]["text"].(string), "The deck was cancelled") {
		t.Fatalf("bob was told %v", told)
	}

	// "Heads down" is not away until an administrator says so.
	f.members[0]["profile"] = map[string]any{"status_text": "Heads down"}
	now = time.Date(2026, 11, 25, 16, 30, 0, 0, time.UTC)
	if posts := remind("wednesday", false); len(posts) != 1 || !strings.Contains(fmt.Sprint(posts[0]["blocks"]), "Draft holiday copy") {
		t.Fatalf("Wednesday's reminders: %v", posts)
	}
	settings, _, _ := app.catalog.TaskQueues()
	settings.AwayStatuses = []string{"Heads down"}
	if _, err := app.catalog.SetTaskQueues(catalog.Bootstrap, settings); err != nil {
		t.Fatal(err)
	}
	now = time.Date(2026, 11, 26, 16, 30, 0, 0, time.UTC)
	if posts := remind("thursday", false); len(posts) != 0 {
		t.Fatalf("a member heads down was reminded: %v", posts)
	}
}

func TestATaskNoticeLostInTransitHoldsBackNoLaterNotice(t *testing.T) {
	f := newTaskFixture(t)
	bob := taskClients(t, f.app, "bob")[0]
	f.failPosts = 1
	added := decodeTask[taskqueue.View](t, actionCall(t, bob, "add_task", map[string]any{"person": "<@UALICE>", "text": "Book the offsite", "due": "2026-12-10"}, false))
	if len(f.postsTo("UALICE")) != 0 {
		t.Fatalf("the failed notice arrived: %v", f.replies)
	}
	actionCall(t, bob, "update_task", map[string]any{"id": added.ID, "revision": added.Revision, "due": "2026-12-03"}, false)
	if posts := f.postsTo("UALICE"); len(posts) != 1 || !strings.Contains(posts[0]["text"].(string), "earlier") {
		t.Fatalf("after a lost notice, alice heard %v", f.replies)
	}
}

func TestTheFirstReminderAfterTimeAwayListsWhatArrived(t *testing.T) {
	f := newTaskFixture(t)
	app := f.app
	now := time.Date(2026, 11, 17, 16, 30, 0, 0, time.UTC)
	app.tasks.Clock = func() time.Time { return now }
	f.members = []map[string]any{{"id": "UALICE", "team_id": "T123", "tz": "America/Denver", "profile": map[string]any{}}}
	bob := taskClients(t, app, "bob")[0]
	spec := automation.Spec{ID: "task-reminder", Kind: "native", Actor: f.service, Project: "default", TimeoutSeconds: 60, Native: &automation.NativeTask{Task: "task-reminder", Client: "slack"}}
	remind := func(id string) []map[string]any {
		t.Helper()
		before := len(f.posts)
		run := automation.Run{ID: automation.Hash(id), Spec: spec}
		if err := app.runAutomation(t.Context(), &run, func() error { return nil }); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		return f.posts[before:]
	}
	actionCall(t, bob, "add_task", map[string]any{"person": "<@UALICE>", "text": "Renew the parking permit"}, false)
	if posts := remind("tuesday"); len(posts) != 1 || strings.Contains(fmt.Sprint(posts[0]["blocks"]), "since your last reminder") {
		t.Fatalf("Tuesday's reminder: %v", posts)
	}
	// Alice is away for the rest of the week while six tasks arrive.
	f.members[0]["profile"] = map[string]any{"status_text": "Vacationing"}
	now = time.Date(2026, 11, 18, 16, 30, 0, 0, time.UTC)
	items := []string{}
	for i := range 6 {
		items = append(items, fmt.Sprintf("Arrived item %d", i+1))
		actionCall(t, bob, "add_task", map[string]any{"person": "<@UALICE>", "text": items[i]}, false)
	}
	if posts := remind("wednesday"); len(posts) != 0 {
		t.Fatalf("an away member was reminded: %v", posts)
	}
	f.members[0]["profile"] = map[string]any{}
	now = time.Date(2026, 11, 23, 16, 30, 0, 0, time.UTC)
	posts := remind("monday")
	if len(posts) != 1 {
		t.Fatalf("Monday's reminders: %v", posts)
	}
	blocks := fmt.Sprint(posts[0]["blocks"])
	if !strings.Contains(blocks, "6 tasks arrived since your last reminder on Tue Nov 17") {
		t.Fatalf("Monday's reminder does not say what arrived:\n%s", blocks)
	}
	for _, item := range items {
		if strings.Count(blocks, item) != 1 {
			t.Fatalf("%q appears %d times:\n%s", item, strings.Count(blocks, item), blocks)
		}
	}
	// A task that arrives in the evening is simply in the next day's reminder:
	// there is nothing to catch up on.
	now = time.Date(2026, 11, 24, 5, 0, 0, 0, time.UTC)
	actionCall(t, bob, "add_task", map[string]any{"person": "<@UALICE>", "text": "Sign the late lease addendum"}, false)
	now = time.Date(2026, 11, 24, 16, 30, 0, 0, time.UTC)
	if posts := remind("tuesday-after"); len(posts) != 1 || strings.Contains(fmt.Sprint(posts[0]["blocks"]), "arrived since") {
		t.Fatalf("the next day's reminder: %v", posts)
	}
}

func TestRemindersSkipCompanyHolidays(t *testing.T) {
	f := newTaskFixture(t)
	app := f.app
	var reads atomic.Int32
	var down atomic.Bool
	google := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/token":
			w.Write([]byte(`{"access_token":"synthetic-access","expires_in":3600,"token_type":"Bearer"}`))
		case "/oauth2/v3/userinfo":
			w.Write([]byte(`{"email":"assistant@example.test","email_verified":true}`))
		case "/calendar/v3/calendars/c_holidays@group.calendar.google.com/events":
			reads.Add(1)
			if down.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			// Only an all-day event whose title starts with the word Holiday is
			// a day off for everyone; one person's time off is not.
			w.Write([]byte(`{"items":[
				{"id":"a","status":"confirmed","summary":"Holiday - Thanksgiving","start":{"date":"2026-11-26"},"end":{"date":"2026-11-27"}},
				{"id":"e","status":"confirmed","summary":"Alex - holiday in Spain","start":{"date":"2026-11-27"},"end":{"date":"2026-11-28"}},
				{"id":"b","status":"confirmed","summary":"Holidays party planning","start":{"date":"2026-11-27"},"end":{"date":"2026-11-28"}},
				{"id":"c","status":"confirmed","summary":"Holiday lunch","start":{"dateTime":"2026-11-27T12:00:00-07:00"},"end":{"dateTime":"2026-11-27T13:00:00-07:00"}},
				{"id":"d","status":"confirmed","summary":"Team offsite","start":{"date":"2026-11-30"},"end":{"date":"2026-12-01"}}]}`))
		default:
			t.Errorf("unexpected Google request %s", r.URL)
		}
	}))
	t.Cleanup(google.Close)
	t.Setenv("FMN_CALENDAR_CLIENT", "synthetic-secret")
	t.Setenv("FMN_CALENDAR_REFRESH", "synthetic-refresh")
	err := app.ConfigureDataConnectionsConfig(connectors.Config{Version: 1, Connections: []connectors.Connection{{
		ID: "holidays", Label: "Company holidays", Kind: "calendar", Endpoint: google.URL,
		OAuthRefresh: &connectors.OAuthRefresh{TokenURL: google.URL + "/token", ClientID: "fixture", ClientSecretEnv: "FMN_CALENDAR_CLIENT", RefreshTokenEnv: "FMN_CALENDAR_REFRESH"},
		Calendar:     &connectors.CalendarConfig{Account: "assistant@example.test", Calendar: "c_holidays@group.calendar.google.com"},
		Operations:   []connectors.Operation{{Name: "events", ReadOnly: true}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	spec := automation.Spec{ID: "task-reminder", Kind: "native", Actor: f.service, Project: "default", TimeoutSeconds: 60, Native: &automation.NativeTask{Task: "task-reminder", Client: "slack"}, Connections: []string{"holidays"}}
	if err := app.validateAutomationDefinition(t.Context(), spec); err == nil {
		t.Fatal("the reminder was set to read a calendar its actor may not read")
	}
	grant, err := app.catalog.ConnectionAccess(catalog.Bootstrap, "holidays")
	if err != nil {
		t.Fatal(err)
	}
	grant.Users = map[string][]string{f.service: {"events"}}
	if _, err = app.catalog.SetConnectionAccess(catalog.Bootstrap, grant); err != nil {
		t.Fatal(err)
	}
	if err := app.validateAutomationDefinition(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 23, 16, 30, 0, 0, time.UTC)
	app.tasks.Clock = func() time.Time { return now }
	f.members = []map[string]any{{"id": "UALICE", "team_id": "T123", "tz": "America/Denver", "profile": map[string]any{}}}
	bob := taskClients(t, app, "bob")[0]
	remind := func(id string) ([]map[string]any, string) {
		t.Helper()
		before := len(f.posts)
		run := automation.Run{ID: automation.Hash(id), Spec: spec}
		if err := app.runAutomation(t.Context(), &run, func() error { return nil }); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		return f.posts[before:], run.Result
	}
	actionCall(t, bob, "add_task", map[string]any{"person": "<@UALICE>", "text": "Old request"}, false)

	// Wednesday: with Thursday off, Monday is two working days away, so the
	// task due Monday leads the older request.
	now = time.Date(2026, 11, 25, 16, 30, 0, 0, time.UTC)
	actionCall(t, bob, "add_task", map[string]any{"person": "<@UALICE>", "text": "Send the board pack", "due": "2026-11-30"}, false)
	posts, _ := remind("wednesday")
	if len(posts) != 1 {
		t.Fatalf("Wednesday's reminders: %v", posts)
	}
	blocks := fmt.Sprint(posts[0]["blocks"])
	if pack, old := strings.Index(blocks, "Send the board pack"), strings.Index(blocks, "Old request"); pack < 0 || old < pack || !strings.Contains(blocks, "due Mon Nov 30") {
		t.Fatalf("Wednesday's reminder before a holiday:\n%s", blocks)
	}
	// Thanksgiving: no reminder, and a task that arrives does not make Friday
	// a catch-up.
	now = time.Date(2026, 11, 26, 16, 30, 0, 0, time.UTC)
	actionCall(t, bob, "add_task", map[string]any{"person": "<@UALICE>", "text": "Book the venue"}, false)
	if posts, result := remind("thanksgiving"); len(posts) != 0 || !strings.Contains(result, `"holiday": 1`) {
		t.Fatalf("Thanksgiving's run sent %v and reported %s", posts, result)
	}
	now = time.Date(2026, 11, 27, 16, 30, 0, 0, time.UTC)
	posts, _ = remind("friday")
	if len(posts) != 1 || strings.Contains(fmt.Sprint(posts[0]["blocks"]), "arrived since") || !strings.Contains(fmt.Sprint(posts[0]["blocks"]), "Book the venue") {
		t.Fatalf("Friday's reminder after a holiday: %v", posts)
	}
	// A weekend run reads nothing from the calendar.
	before := reads.Load()
	now = time.Date(2026, 11, 28, 16, 30, 0, 0, time.UTC)
	if posts, _ := remind("saturday"); len(posts) != 0 || reads.Load() != before {
		t.Fatalf("Saturday's run sent %v and read the calendar %d times", posts, reads.Load()-before)
	}
	// An unreadable calendar does not stop the reminders; the run says so.
	down.Store(true)
	now = time.Date(2026, 11, 30, 16, 30, 0, 0, time.UTC)
	if posts, result := remind("monday"); len(posts) != 1 || !strings.Contains(result, "holidayCalendar") {
		t.Fatalf("Monday's run with the calendar down sent %v and reported %s", posts, result)
	}
}

func TestRemindersSkipHolidaysOnEveryCalendarTheyName(t *testing.T) {
	f := newTaskFixture(t)
	app := f.app
	var usDown atomic.Bool
	google := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/token":
			w.Write([]byte(`{"access_token":"synthetic-access","expires_in":3600,"token_type":"Bearer"}`))
		case "/oauth2/v3/userinfo":
			w.Write([]byte(`{"email":"assistant@example.test","email_verified":true}`))
		case "/calendar/v3/calendars/c_holidays@group.calendar.google.com/events":
			w.Write([]byte(`{"items":[{"id":"a","status":"confirmed","summary":"Holiday - Company day","start":{"date":"2026-11-30"},"end":{"date":"2026-12-01"}}]}`))
		case "/calendar/v3/calendars/en.usa.official#holiday@group.v.calendar.google.com/events":
			if usDown.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			// A public holiday counts only when the holiday titles name it.
			w.Write([]byte(`{"items":[
				{"id":"t","status":"confirmed","summary":"Thanksgiving Day","start":{"date":"2026-11-26"},"end":{"date":"2026-11-27"}},
				{"id":"n","status":"confirmed","summary":"Native American Heritage Day","start":{"date":"2026-11-27"},"end":{"date":"2026-11-28"}}]}`))
		default:
			t.Errorf("unexpected Google request %s", r.URL)
		}
	}))
	t.Cleanup(google.Close)
	t.Setenv("FMN_CALENDAR_CLIENT", "synthetic-secret")
	t.Setenv("FMN_CALENDAR_REFRESH", "synthetic-refresh")
	calendar := func(id, calendarID string) connectors.Connection {
		return connectors.Connection{
			ID: id, Label: id, Kind: "calendar", Endpoint: google.URL,
			OAuthRefresh: &connectors.OAuthRefresh{TokenURL: google.URL + "/token", ClientID: "fixture", ClientSecretEnv: "FMN_CALENDAR_CLIENT", RefreshTokenEnv: "FMN_CALENDAR_REFRESH"},
			Calendar:     &connectors.CalendarConfig{Account: "assistant@example.test", Calendar: calendarID},
			Operations:   []connectors.Operation{{Name: "events", ReadOnly: true}},
		}
	}
	err := app.ConfigureDataConnectionsConfig(connectors.Config{Version: 1, Connections: []connectors.Connection{
		calendar("company-holidays", "c_holidays@group.calendar.google.com"),
		calendar("us-holidays", "en.usa.official#holiday@group.v.calendar.google.com"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	spec := automation.Spec{ID: "task-reminder", Kind: "native", Actor: f.service, Project: "default", TimeoutSeconds: 60, Native: &automation.NativeTask{Task: "task-reminder", Client: "slack"}, Connections: []string{"company-holidays", "us-holidays"}}
	for _, id := range spec.Connections {
		if err := app.validateAutomationDefinition(t.Context(), spec); err == nil {
			t.Fatal("the reminder was set to read a calendar its actor may not read")
		}
		grant, err := app.catalog.ConnectionAccess(catalog.Bootstrap, id)
		if err != nil {
			t.Fatal(err)
		}
		grant.Users = map[string][]string{f.service: {"events"}}
		if _, err = app.catalog.SetConnectionAccess(catalog.Bootstrap, grant); err != nil {
			t.Fatal(err)
		}
	}
	if err := app.validateAutomationDefinition(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	tooMany := spec
	tooMany.Connections = []string{"company-holidays", "us-holidays", "a", "b"}
	if err := app.validateAutomationDefinition(t.Context(), tooMany); err == nil {
		t.Fatal("the reminder was set to read four calendars")
	}
	settings, _, _ := app.catalog.TaskQueues()
	settings.HolidayTitles = []string{"holiday", "thanksgiving"}
	if _, err := app.catalog.SetTaskQueues(catalog.Bootstrap, settings); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 11, 26, 16, 30, 0, 0, time.UTC)
	app.tasks.Clock = func() time.Time { return now }
	f.members = []map[string]any{{"id": "UALICE", "team_id": "T123", "tz": "America/Denver", "profile": map[string]any{}}}
	bob := taskClients(t, app, "bob")[0]
	actionCall(t, bob, "add_task", map[string]any{"person": "<@UALICE>", "text": "Old request"}, false)
	remind := func(id string) ([]map[string]any, string) {
		t.Helper()
		before := len(f.posts)
		run := automation.Run{ID: automation.Hash(id), Spec: spec}
		if err := app.runAutomation(t.Context(), &run, func() error { return nil }); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		return f.posts[before:], run.Result
	}
	if posts, result := remind("thanksgiving"); len(posts) != 0 || !strings.Contains(result, `"holiday": 1`) {
		t.Fatalf("Thanksgiving, from the US calendar, sent %v and reported %s", posts, result)
	}
	now = time.Date(2026, 11, 27, 16, 30, 0, 0, time.UTC)
	if posts, _ := remind("friday"); len(posts) != 1 {
		t.Fatalf("a public holiday the titles do not name stopped Friday's reminder: %v", posts)
	}
	// One calendar down leaves the other's holidays in place, and the run
	// names the calendar it could not read.
	usDown.Store(true)
	now = time.Date(2026, 11, 30, 16, 30, 0, 0, time.UTC)
	if posts, result := remind("monday"); len(posts) != 0 || !strings.Contains(result, `"holiday": 1`) || !strings.Contains(result, "us-holidays") || strings.Contains(result, "company-holidays") {
		t.Fatalf("Monday's company holiday with the US calendar down sent %v and reported %s", posts, result)
	}
}

func TestRemindersListSlackMessagesWaitingOnAReply(t *testing.T) {
	f := newTaskFixture(t)
	app := f.app
	denver, _ := time.LoadLocation("America/Denver")
	// Tuesday 09:30 in Denver.
	now := time.Date(2026, 11, 17, 9, 30, 0, 0, denver)
	app.tasks.Clock = func() time.Time { return now }
	f.members = []map[string]any{
		{"id": "UALICE", "team_id": "T123", "tz": "America/Denver", "profile": map[string]any{"display_name": "Alice <A>"}},
		{"id": "UBOB", "team_id": "T123", "tz": "America/Denver"},
	}
	clients := taskClients(t, app, "alice", "bob")
	alice := clients[0]

	// Bob's own Slack: a direct message from alice on Monday and a mention
	// in a channel are unanswered; one sent this morning has not had a
	// working morning yet; alice's own reply in a thread is answered.
	ts := func(at time.Time) string { return fmt.Sprintf("%d.000100", at.Unix()) }
	monday := time.Date(2026, 11, 16, 10, 0, 0, 0, denver)
	var searches, reads atomic.Int32
	var busy, quiet atomic.Bool
	var window, grantee atomic.Value
	grantee.Store("UBOB")
	message := func(channel string, at time.Time, author, content string) map[string]any {
		return map[string]any{"channel_id": channel, "message_ts": ts(at), "author_user_id": author, "content": content, "is_author_bot": false, "permalink": "https://example.slack.com/archives/" + channel + "/p" + strings.ReplaceAll(ts(at), ".", "")}
	}
	connectPersonalProviderFixture(t, app, "bob", "slack", func(r *http.Request) string {
		switch r.URL.Path {
		case "/api/oauth.v2.access":
			return `{"ok":true,"team":{"id":"T123"},"authed_user":{"id":"` + grantee.Load().(string) + `","token_type":"user","access_token":"fixture-user","scope":"search:read.im,search:read.mpim,search:read.public,search:read.private,im:history,mpim:history,channels:history,groups:history,chat:write"}}`
		case "/api/auth.test":
			return `{"ok":true,"user_id":"` + grantee.Load().(string) + `","team_id":"T123"}`
		case "/api/assistant.search.context":
			searches.Add(1)
			if busy.Load() {
				return `{"ok":false,"error":"ratelimited"}`
			}
			var in struct {
				Query         string `json:"query"`
				After, Before int64
			}
			json.NewDecoder(r.Body).Decode(&in)
			window.Store([2]int64{in.After, in.Before})
			messages := []any{message("D1", monday, "UALICE", "Can you send the proofs?"), message("D2", now.Add(-time.Hour), "UALICE", "Morning question")}
			if in.Query == "<@UBOB>" {
				messages = []any{message("C1", monday.Add(time.Hour), "UALICE", "<@UBOB> please check the proofs")}
			}
			if quiet.Load() {
				messages = []any{}
				if in.Query != "<@UBOB>" {
					messages = []any{message("D1", monday, "UALICE", "Can you send the proofs?")}
				}
			}
			raw, _ := json.Marshal(map[string]any{"ok": true, "results": map[string]any{"messages": messages}})
			return string(raw)
		case "/api/conversations.history":
			reads.Add(1)
			return `{"ok":true,"messages":[]}`
		default:
			t.Errorf("unexpected personal Slack call %s", r.URL.Path)
			return `{}`
		}
	})
	session := func(user string) *http.Cookie {
		w := httptest.NewRecorder()
		if err := app.createSession(w, user); err != nil {
			t.Fatal(err)
		}
		return w.Result().Cookies()[0]
	}
	bob := session("bob")
	save := func(body string) *httptest.ResponseRecorder {
		return localRequest(app, "/api/assistant/action/save_assistant_settings", body, bob)
	}
	// Reply reminders need the instance to offer personal assistants, and
	// turning them on gives bob a queue for the morning reminder.
	if w := save(`{"revision":0,"replyReminders":true,"timeZone":"America/Denver"}`); w.Code == 200 {
		t.Fatal("reply reminders turned on without the instance policy")
	}
	if _, err := app.catalog.SetAssistantPolicy(catalog.Bootstrap, catalog.AssistantPolicy{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := app.catalog.TaskPersonForAccount("bob", false); err == nil {
		t.Fatal("bob already has a queue")
	}
	if w := save(`{"revision":0,"replyReminders":true,"timeZone":"America/Denver"}`); w.Code != 200 {
		t.Fatalf("turning reply reminders on: %d %s", w.Code, w.Body)
	}
	person, err := app.catalog.TaskPersonForAccount("bob", false)
	if err != nil {
		t.Fatal("turning reply reminders on gave bob no queue", err)
	}
	// Bob may connect Slack without AI consent; alice, with neither, may not.
	connect := func(cookie *http.Cookie) int {
		return localRequest(app, "/account/connections/slack/connect", "", cookie).Code
	}
	if code := connect(bob); code != http.StatusSeeOther {
		t.Fatalf("bob could not connect Slack for reply reminders: %d", code)
	}
	if code := connect(session("alice")); code == http.StatusSeeOther {
		t.Fatal("alice connected Slack without the assistant or reply reminders")
	}
	if _, err = app.catalog.SaveAssistant("alice", catalog.AssistantSettings{Revision: 1, TimeZone: "UTC", MaintainMemory: true, ReplyReminders: true}); err != nil {
		t.Fatal(err)
	}
	actionCall(t, clients[1], "add_task", map[string]any{"person": "<@UALICE>", "text": "Draft holiday copy"}, false)

	spec := automation.Spec{ID: "task-reminder", Kind: "native", Actor: f.service, Project: "default", TimeoutSeconds: 60, Native: &automation.NativeTask{Task: "task-reminder", Client: "slack"}}
	if err = app.automations.Definitions.SetFiles([]automation.Spec{spec}); err != nil {
		t.Fatal(err)
	}
	remind := func(id string, dry bool) (map[string]any, []map[string]any) {
		t.Helper()
		before := len(f.posts)
		run := automation.Run{ID: automation.Hash(id), DryRun: dry, Spec: spec}
		if err := app.runAutomation(t.Context(), &run, func() error { return nil }); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if strings.Contains(run.Result, "proofs") {
			t.Fatalf("the report carries message text:\n%s", run.Result)
		}
		var out map[string]any
		report := strings.TrimSuffix(strings.SplitN(run.Result, "```json\n", 2)[1], "\n```\n")
		if err := json.Unmarshal([]byte(report), &out); err != nil {
			t.Fatal(err, run.Result)
		}
		return out, f.posts[before:]
	}
	alicePerson, _ := app.catalog.TaskPersonForAccount("alice", false)

	// A test run reads no Slack.
	out, posts := remind("preview", true)
	checking := fmt.Sprint(out["wouldCheckReplies"])
	if len(posts) != 0 || searches.Load() != 0 || !strings.Contains(checking, alicePerson.ID) || !strings.Contains(checking, person.ID) || fmt.Sprint(out["wouldRemind"]) != "["+alicePerson.ID+"]" {
		t.Fatalf("test run: %v, posts %v, %d searches", out, posts, searches.Load())
	}
	// A busy Slack holds bob's reminder; alice, who has not connected Slack,
	// hears about her task and how to connect.
	busy.Store(true)
	out, posts = remind("busy", false)
	if len(posts) != 1 || posts[0]["channel"] != "UALICE" || fmt.Sprint(out["skipped"].(map[string]any)["slackBusy"]) != "1" {
		t.Fatalf("busy run: %v, posts %v", out, posts)
	}
	text := fmt.Sprint(posts[0])
	if !strings.Contains(text, "Draft holiday copy") || !strings.Contains(text, "couldn't check Slack") || fmt.Sprint(out["repliesUnread"]) != "["+alicePerson.ID+"]" {
		t.Fatalf("alice's reminder: %s\n%v", text, out)
	}
	busy.Store(false)
	now = now.Add(15 * time.Minute)
	searched := searches.Load()
	out, posts = remind("tuesday", false)
	if len(posts) != 1 || posts[0]["channel"] != "UBOB" {
		t.Fatalf("Tuesday: %v, posts %v", out, posts)
	}
	text = fmt.Sprint(posts[0])
	// Thursday 00:00 to Tuesday 00:00, three working days back.
	from, to := time.Date(2026, 11, 12, 0, 0, 0, 0, denver), time.Date(2026, 11, 17, 0, 0, 0, 0, denver)
	if window.Load() != [2]int64{from.Unix(), to.Unix()} || searches.Load()-searched != 2 || reads.Load() != 2 {
		t.Fatalf("searched %v with %d searches and %d reads", window.Load(), searches.Load()-searched, reads.Load())
	}
	for _, want := range []string{"Waiting on your reply", "|Direct message> from Alice &lt;A&gt;, Mon Nov 16 10:00 AM", "|Mention> from Alice &lt;A&gt; in <#C1>, Mon Nov 16 11:00 AM", "https://example.slack.com/archives/D1/"} {
		if !strings.Contains(text, want) {
			t.Fatalf("bob's reminder lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "proofs") || strings.Contains(text, "archives/D2") || strings.Contains(text, "Your tasks") {
		t.Fatalf("bob's reminder: %s", text)
	}
	// Dismiss leaves the reminder in place and drops that message for good.
	var dismiss []map[string]any
	for _, b := range postButtons(posts[0]) {
		if b["action_id"] == taskqueue.ReplyDismissAction {
			dismiss = append(dismiss, b)
		}
	}
	if len(dismiss) != 2 || !strings.Contains(fmt.Sprint(dismiss[0]["value"]), "D1") {
		t.Fatalf("bob's dismiss buttons: %v", dismiss)
	}
	if reply := f.press(t, "UBOB", "4.1", dismiss[0]); !strings.Contains(reply, "Dismissed") || !f.asides[len(f.asides)-1] {
		t.Fatalf("dismissing replied %q, aside %v", reply, f.asides)
	}
	// The job keeps a hash of the dismissal and when it expires, never the
	// message's channel or timestamp.
	kept := func() taskqueue.ReminderState {
		state, err := app.automations.Queue.JobState(spec)
		if err != nil {
			t.Fatal(err)
		}
		return taskqueue.ReadReminderState(state.Data)
	}
	if got := kept().Dismissed; len(got) != 1 || strings.Contains(fmt.Sprint(got), "D1") || !slices.Contains(slices.Collect(maps.Values(got)), "2026-12-01") {
		t.Fatalf("kept dismissals: %v", got)
	}
	forged := map[string]any{"action_id": taskqueue.ReplyDismissAction, "value": `{"channel":"D1","ts":"not a time"}`}
	if w := f.interact(t, buttonPress("UBOB", "4.2", forged, f.upstream+"/actions/T123/1/hook")); w.Code != http.StatusBadRequest {
		t.Fatalf("a malformed dismiss: HTTP %d", w.Code)
	}

	// Once a day: a later run neither sends nor searches.
	searched = searches.Load()
	if _, posts = remind("tuesday-again", false); len(posts) != 0 || searches.Load() != searched {
		t.Fatalf("a second run sent %v after %d searches", posts, searches.Load()-searched)
	}

	// Wednesday: in the last hour a busy Slack no longer holds a reminder
	// with tasks, and with nothing due it stays quiet.
	venue := decodeTask[taskqueue.View](t, actionCall(t, alice, "add_task", map[string]any{"person": "<@UBOB>", "text": "Send the venue list"}, false))
	told := len(f.postsTo("UBOB"))
	busy.Store(true)
	now = time.Date(2026, 11, 18, 10, 15, 0, 0, denver)
	if _, posts = remind("wednesday-busy", false); len(f.postsTo("UBOB")) != told {
		t.Fatalf("a busy Slack before the last hour sent %v", posts)
	}
	now = time.Date(2026, 11, 18, 11, 15, 0, 0, denver)
	remind("wednesday-late", false)
	if sent := f.postsTo("UBOB")[told:]; len(sent) != 1 || !strings.Contains(fmt.Sprint(sent[0]), "Send the venue list") || !strings.Contains(fmt.Sprint(sent[0]), "Slack was busy") {
		t.Fatalf("Wednesday's late reminder: %v", sent)
	}

	// Thursday, with nothing due and only the dismissed message waiting, bob
	// hears nothing, it is not even checked, and Slack is searched once that
	// morning.
	busy.Store(false)
	quiet.Store(true)
	actionCall(t, clients[1], "update_task", map[string]any{"id": venue.ID, "revision": venue.Revision, "outcome": "done"}, false)
	told = len(f.postsTo("UBOB"))
	now = time.Date(2026, 11, 19, 9, 15, 0, 0, denver)
	searched, read := searches.Load(), reads.Load()
	remind("thursday", false)
	if len(f.postsTo("UBOB")) != told || searches.Load()-searched != 2 || reads.Load() != read {
		t.Fatalf("Thursday sent bob %v after %d searches", f.postsTo("UBOB")[told:], searches.Load()-searched)
	}
	now = now.Add(15 * time.Minute)
	if remind("thursday-again", false); searches.Load()-searched != 2 {
		t.Fatalf("Slack was searched again on Thursday: %d searches", searches.Load()-searched)
	}
	// The job keeps only the day of that search, by person.
	if got := kept().Checked; len(got) != 1 || got[person.ID] != "2026-11-19" {
		t.Fatalf("Thursday's search mark: %v", got)
	}

	// Friday, Slack connected as another member is not read for bob, and he
	// hears how to fix it.
	grantee.Store("UOTHER")
	account, _ := app.catalog.Account("bob")
	target, err := app.personalConnections.Begin(account.ID, "slack", "browser", "http://localhost/callback")
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := url.Parse(target)
	if _, err = app.personalConnections.Finish(t.Context(), account.ID, "slack", "browser", parsed.Query().Get("state"), "code", func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	told = len(f.postsTo("UBOB"))
	now = time.Date(2026, 11, 20, 9, 15, 0, 0, denver)
	searched = searches.Load()
	remind("friday", false)
	if sent := f.postsTo("UBOB")[told:]; len(sent) != 1 || !strings.Contains(fmt.Sprint(sent[0]), "couldn't check Slack") || searches.Load() != searched {
		t.Fatalf("Friday, with Slack connected as another member: %v after %d searches", sent, searches.Load()-searched)
	}

	// Turning reply reminders off stops the searches.
	if w := save(`{"revision":1,"replyReminders":false,"timeZone":"America/Denver"}`); w.Code != 200 {
		t.Fatalf("turning reply reminders off: %d %s", w.Code, w.Body)
	}
	now = time.Date(2026, 11, 23, 9, 15, 0, 0, denver)
	searched = searches.Load()
	if remind("monday", false); searches.Load() != searched {
		t.Fatal("Slack was searched after reply reminders were turned off")
	}
	// Two weeks on, the dismissal and Thursday's mark are forgotten.
	now = time.Date(2026, 12, 2, 9, 15, 0, 0, denver)
	if remind("expired", false); len(kept().Dismissed)+len(kept().Checked) != 0 {
		t.Fatalf("expired reply state was kept: %+v", kept())
	}
}

func TestTheCatchUpListStartsFromTheReminderTheJobRecorded(t *testing.T) {
	f := newTaskFixture(t)
	app := f.app
	now := time.Date(2026, 11, 17, 16, 30, 0, 0, time.UTC)
	app.tasks.Clock = func() time.Time { return now }
	f.members = []map[string]any{{"id": "UALICE", "team_id": "T123", "tz": "America/Denver", "profile": map[string]any{}}}
	bob := taskClients(t, app, "bob")[0]
	added := decodeTask[taskqueue.View](t, actionCall(t, bob, "add_task", map[string]any{"person": "<@UALICE>", "text": "Renew the parking permit"}, false))
	person := added.Owner.ID
	spec := automation.Spec{ID: "task-reminder", Kind: "native", Actor: f.service, Project: "default", TimeoutSeconds: 60, Native: &automation.NativeTask{Task: "task-reminder", Client: "slack"}}
	reminded := func() string {
		t.Helper()
		state, err := app.automations.Queue.JobState(spec)
		if err != nil {
			t.Fatal(err)
		}
		return taskqueue.ReadReminderState(state.Data).Reminded[person]
	}
	// The job last reminded alice a week ago, when no reminder effect was
	// kept, as after an upgrade.
	if err := app.tasks.ChangeReminderState(spec, now, func(st taskqueue.ReminderState) { st.Reminded[person] = "2026-11-10" }); err != nil {
		t.Fatal(err)
	}
	run := automation.Run{ID: automation.Hash("tuesday"), Spec: spec}
	if err := app.runAutomation(t.Context(), &run, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	posts := f.postsTo("UALICE")
	if len(posts) != 2 || !strings.Contains(fmt.Sprint(posts[1]["blocks"]), "1 task arrived since your last reminder on Tue Nov 10") {
		t.Fatalf("Tuesday's reminder: %v", posts)
	}
	if day := reminded(); day != "2026-11-17" {
		t.Fatalf("the job recorded the reminder as %q", day)
	}
	// A test run records nothing.
	now = time.Date(2026, 11, 18, 16, 30, 0, 0, time.UTC)
	run = automation.Run{ID: automation.Hash("preview"), DryRun: true, Spec: spec}
	if err := app.runAutomation(t.Context(), &run, func() error { return nil }); err != nil || reminded() != "2026-11-17" {
		t.Fatalf("a test run: %v, recorded %q", err, reminded())
	}
}
