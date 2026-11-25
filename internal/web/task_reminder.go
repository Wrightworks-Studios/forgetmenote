package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"forgetmenote/internal/automation"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/connectors"
	"forgetmenote/internal/messaging"
	"forgetmenote/internal/tasks"
	"forgetmenote/internal/workspace"
)

// The weekday reminder is one task-reminder automation job, run every few
// minutes, that sends each queue's owner one direct message on a working day,
// from nine in their own time zone, with the tasks that need them most. It
// skips people whose status says they are away, people with nothing due, and
// holidays read from the calendar connections the job names. People who turned
// reply reminders on also see the Slack messages waiting on their reply.
const (
	taskSnoozeAction        = "task_snooze"
	taskNotApplicableAction = "task_not_applicable"
	taskNotApplicableView   = "task_not_applicable"
	taskReplyDismissAction  = "task_reply_dismiss"

	taskReminderHour = 9
	// A run that missed nine still reminds within this many hours.
	taskReminderHours = 3
	taskReminderShown = 5
	// Tasks that arrived while someone was away are listed up to this many.
	taskArrivalsShown = 10
	// Holidays are read from this many days back, for the catch-up list, to
	// this many ahead, past the longest lead time of 60 working days.
	taskHolidaysBack, taskHolidaysAhead = 40, 120
	taskHolidayCalendars                = 3
	// Reply reminders look this many working days back. A message counts once
	// a working morning has passed without a reply or reaction.
	taskReplyDays = 3
	// A dismissal is kept this many days, past the furthest the reply window
	// reaches back over weekends and holidays, and then forgotten.
	taskReplyDismissDays = 14
)

// Statuses that mean away when an administrator names none. Slack's own
// "Vacationing" and "Out sick" statuses carry these emoji too.
var (
	defaultAwayStatuses  = []string{"vacationing", "on vacation", "vacation", "out sick", "sick", "out of office", "ooo", "pto", "on leave", "holiday", "day off"}
	awayEmoji            = []string{":palm_tree:", ":face_with_thermometer:"}
	defaultHolidayTitles = []string{"holiday"}
)

// taskReminder returns the transport that lists and messages the members of
// the client task queues use. The job runs as that client's members actor, so
// the administrator who configured the client decides who it reaches.
func (s *Server) taskReminder(spec automation.Spec) (messaging.Transport, error) {
	settings, _, err := s.taskQueues()
	if err != nil {
		return nil, err
	}
	if spec.Native == nil || spec.Native.Client != settings.Client {
		return nil, errors.New("task-reminder names the messaging client task queues use")
	}
	c, transport, ok := s.messageClient(settings.Client)
	if !ok {
		return nil, errors.New("the messaging client is not configured or is disabled")
	}
	if c.Members == nil || c.Members.Actor != spec.Actor {
		return nil, errors.New("task-reminder runs as the client's members actor")
	}
	if _, ok := transport.(messaging.MemberLister); !ok {
		return nil, errors.New("this messaging client cannot list its members")
	}
	// The job may name calendar connections to read holidays from, such as a
	// company calendar and a public holiday calendar. A disabled draft may
	// name one an operator has not mounted yet.
	if len(spec.Connections) > taskHolidayCalendars {
		return nil, fmt.Errorf("task-reminder reads holidays from at most %d calendar connections", taskHolidayCalendars)
	}
	for _, id := range spec.Connections {
		info, ok := s.dataConnections.Info(id)
		if ok && info.Kind != "calendar" {
			return nil, errors.New("task-reminder reads holidays from a calendar connection")
		}
		if _, configured := s.dataConnections.ConfiguredOperation(id, "events"); ok && (!configured || !s.catalog.ConnectionAllowed(spec.Actor, id, "events")) {
			return nil, fmt.Errorf("%w: the reminder's actor needs the calendar's events operation", workspace.ErrForbidden)
		}
	}
	return transport, nil
}

// taskHolidays returns the company holidays on the job's calendars, by date:
// the days of each all-day event whose title begins with one of the holiday
// titles. A job that names no calendar has none. A calendar that cannot be
// read adds no holidays and is named in the error; the others still count.
func (s *Server) taskHolidays(ctx context.Context, spec automation.Spec, titles []string, now time.Time) (map[string]bool, error) {
	holidays := map[string]bool{}
	if len(titles) == 0 {
		titles = defaultHolidayTitles
	}
	holiday := phraseMatcher(titles, true)
	from, to := now.AddDate(0, 0, -taskHolidaysBack), now.AddDate(0, 0, taskHolidaysAhead)
	unread := []string{}
	for _, id := range spec.Connections {
		if err := s.readHolidays(ctx, id, holiday, from, to, holidays); err != nil {
			unread = append(unread, id+": "+err.Error())
		}
	}
	if len(unread) > 0 {
		return holidays, errors.New(strings.Join(unread, "; "))
	}
	return holidays, nil
}

// readHolidays adds the holidays on one calendar connection between two dates.
func (s *Server) readHolidays(ctx context.Context, id string, holiday *regexp.Regexp, from, to time.Time, holidays map[string]bool) error {
	args := map[string]any{"from": from.Format(time.DateOnly), "to": to.Format(time.DateOnly)}
	days := map[string]bool{}
	for range 10 {
		result, err := s.dataExecute(ctx, dataCall{Connection: id, Operation: "events", Arguments: maps.Clone(args)})
		if err != nil {
			return err
		}
		var page struct {
			Events []struct {
				Title      string
				AllDay     bool
				Start, End string
			}
			NextPageToken string
			HasMore       bool
		}
		raw, _ := json.Marshal(result.Data)
		if err = json.Unmarshal(raw, &page); err != nil {
			return err
		}
		for _, e := range page.Events {
			start, startErr := time.Parse(time.DateOnly, e.Start)
			end, endErr := time.Parse(time.DateOnly, e.End)
			if !e.AllDay || startErr != nil || endErr != nil || !holiday.MatchString(strings.ToLower(e.Title)) {
				continue
			}
			for d := start; d.Before(end) && d.Before(start.AddDate(0, 0, 31)); d = d.AddDate(0, 0, 1) {
				days[d.Format(time.DateOnly)] = true
			}
		}
		if !page.HasMore {
			maps.Copy(holidays, days)
			return nil
		}
		args["pageToken"] = page.NextPageToken
	}
	return errors.New("the calendar has more events than the reminder reads")
}

// awayStatus returns whether a member's status says they are away: one of the
// phrases as whole words, or a vacation or sick emoji.
func awayStatus(phrases []string) func(messaging.Member) bool {
	if len(phrases) == 0 {
		phrases = defaultAwayStatuses
	}
	words := phraseMatcher(phrases, false)
	return func(m messaging.Member) bool {
		return slices.Contains(awayEmoji, m.StatusEmoji) || words.MatchString(strings.ToLower(m.Status))
	}
}

// phraseMatcher matches any of the lowercase phrases as whole words in
// lowercase text, or only at its start when leading.
func phraseMatcher(phrases []string, leading bool) *regexp.Regexp {
	quoted := []string{}
	for _, phrase := range phrases {
		quoted = append(quoted, regexp.QuoteMeta(strings.ToLower(phrase)))
	}
	before := `(^|[^\pL\pN])`
	if leading {
		before = `^\s*`
	}
	return regexp.MustCompile(before + `(` + strings.Join(quoted, "|") + `)($|[^\pL\pN])`)
}

// runTaskReminder sends today's reminders that are due. A dry run reports who
// would be reminded now. Each person is reminded at most once a day, and the
// report names people by person ID only.
func (s *Server) runTaskReminder(ctx context.Context, r *automation.Run) (map[string]any, automation.Incident, error) {
	incident := automation.Incident{Severity: "warning", Summary: "Task reminders could not all be sent"}
	transport, err := s.taskReminder(r.Spec)
	if err != nil {
		return nil, incident, err
	}
	settings, store, err := s.taskQueues()
	if err != nil {
		return nil, incident, err
	}
	members, err := transport.(messaging.MemberLister).Members(ctx)
	if err != nil {
		return nil, incident, err
	}
	// The reminders name people from this list, not another read of it.
	s.memberNames.seed(members)
	byUser := map[string]messaging.Member{}
	for _, m := range members {
		byUser[m.User] = m
	}
	book, err := readTaskBook(ctx, store)
	if err != nil {
		return nil, incident, err
	}
	if !r.DryRun {
		if err = s.checkAutomationAuth(ctx); err != nil {
			return nil, incident, err
		}
	}
	now, away := s.taskTime(), awayStatus(settings.AwayStatuses)
	// Holidays are read once, when the first person is due a reminder now. A
	// calendar that cannot be read adds no holidays.
	var holidays map[string]bool
	var holidaysErr error
	working := func(day time.Time) bool {
		if !tasks.Weekday(day) {
			return false
		}
		if holidays == nil {
			if holidays, holidaysErr = s.taskHolidays(ctx, r.Spec, settings.HolidayTitles, now); holidaysErr != nil {
				s.logAutomationError("Task reminder holidays", holidaysErr)
			}
		}
		return !holidays[day.Format(time.DateOnly)]
	}
	// Dismissals, the day's search marks and the day each person was last
	// reminded live in the job's own state until they expire; a run drops the
	// expired ones.
	state, err := s.automations.Queue.JobState(r.Spec)
	if err != nil {
		return nil, incident, err
	}
	kept := readReminderState(state.Data)
	if kept.expire(now) && !r.DryRun {
		if err = s.changeReminderState(r.Spec, now, nil); err != nil {
			s.logAutomationError("Task reminder replies", err)
		}
	}
	skipped := map[string]int{}
	reminded, failed, checking, unread := []string{}, []string{}, []string{}, []string{}
	remindedOn := map[string]string{}
	// Everyone with a task, and everyone who turned reply reminders on.
	people := map[string]bool{}
	for person, q := range book.queues {
		people[person] = len(q.Tasks) > 0
	}
	replies := map[string]bool{}
	for _, user := range s.catalog.ReplyReminderAccounts() {
		if p, err := s.catalog.TaskPersonForAccount(user, false); err == nil {
			replies[user], people[p.ID] = true, true
		}
	}
	for _, person := range slices.Sorted(maps.Keys(people)) {
		if !people[person] {
			continue
		}
		q := book.queues[person]
		owner := s.taskPersonView(person, taskCaller{})
		m, ok := byUser[owner.SlackUser]
		if owner.SlackUser == "" || !ok {
			skipped["noMember"]++
			continue
		}
		loc, err := time.LoadLocation(m.TimeZone)
		if m.TimeZone == "" || err != nil {
			skipped["noTimeZone"]++
			continue
		}
		local := now.In(loc)
		day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC)
		if !tasks.Weekday(day) || local.Hour() < taskReminderHour || local.Hour() >= taskReminderHour+taskReminderHours {
			skipped["notNow"]++
			continue
		}
		if !working(day) {
			skipped["holiday"]++
			continue
		}
		if away(m) {
			skipped["away"]++
			continue
		}
		due := q.Remind(day, working)
		account, _ := s.catalog.TaskPersonAccount(person)
		check := replies[account]
		if len(due) == 0 && !check {
			skipped["nothingDue"]++
			continue
		}
		date := day.Format(time.DateOnly)
		scope := automation.Hash([]string{"task-reminder", person, date})
		// Checked before reading Slack, so a reminded person costs no searches.
		// A reminder Slack refused reached nobody and is sent again.
		earlier, err := s.automations.Queue.Effects(scope)
		if err != nil {
			return nil, incident, err
		}
		if slices.ContainsFunc(earlier, func(e automation.Effect) bool { return e.State != "rejected" }) {
			skipped["alreadyReminded"]++
			continue
		}
		// Slack is searched at most once a person a day: a search that found
		// nothing to send marks the day in the job state.
		if check && kept.Checked[person] == date {
			check = false
		}
		if len(due) == 0 && !check {
			skipped["nothingDue"]++
			continue
		}
		// A dry run reads no Slack, so it names whose replies it would check.
		if r.DryRun {
			if check {
				checking = append(checking, person)
			}
			if len(due) > 0 {
				reminded = append(reminded, person)
			}
			continue
		}
		var waiting taskWaiting
		if check {
			dismissed := func(channel, ts string) bool {
				_, ok := kept.Dismissed[replyDismissal(person, channel, ts)]
				return ok
			}
			waiting = s.taskWaitingFor(ctx, account, m.User, settings.Workspace, local, working, dismissed)
			busy := errors.Is(waiting.err, connectors.ErrProviderBusy)
			// Slack limits searches, so a busy Slack holds the reminder for a
			// later run until the last hour of the reminder window.
			if busy && local.Hour() < taskReminderHour+taskReminderHours-1 {
				skipped["slackBusy"]++
				continue
			}
			if waiting.err != nil {
				s.logAutomationError("Task reminder replies", waiting.err)
				unread = append(unread, person)
			}
			// With no tasks due, an empty list or a busy Slack sends nothing.
			if len(due) == 0 && (busy || waiting.err == nil && len(waiting.messages) == 0 && waiting.unchecked == 0) {
				if !busy {
					if err := s.changeReminderState(r.Spec, now, func(st reminderState) { st.Checked[person] = date }); err != nil {
						s.logAutomationError("Task reminder replies", err)
					}
				}
				skipped["nothingDue"]++
				continue
			}
		}
		address := messaging.Address{Client: settings.Client, Workspace: settings.Workspace, User: m.User, Channel: m.User, Direct: true}
		var since time.Time
		var arrived []tasks.Task
		if len(due) > 0 {
			since, arrived = taskArrivals(kept.Reminded[person], q, day, working)
		}
		reply := s.taskReminderReply(person, due, day, since, arrived)
		if check {
			reply = s.taskWaitingReply(reply, waiting, local.Location(), byUser)
		}
		sent, err := s.sendOnce(ctx, automation.Effect{Scope: scope, Action: date, InputHash: automation.Hash([]string{person, date}), Actor: person}, transport, address, reply, dmSendTimeout, nil)
		if err != nil {
			s.logAutomationError("Task reminder", err)
		}
		switch {
		case sent:
			reminded, remindedOn[person] = append(reminded, person), date
		case err != nil:
			failed = append(failed, person)
		default:
			skipped["alreadyReminded"]++
		}
	}
	// A lost record only lists more tasks as arrived in the next reminder.
	if len(remindedOn) > 0 {
		if err = s.changeReminderState(r.Spec, now, func(st reminderState) { maps.Copy(st.Reminded, remindedOn) }); err != nil {
			s.logAutomationError("Task reminder", err)
		}
	}
	key := "reminded"
	if r.DryRun {
		key = "wouldRemind"
	}
	out := map[string]any{key: reminded, "failed": failed, "skipped": skipped}
	if r.DryRun && len(checking) > 0 {
		out["wouldCheckReplies"] = checking
	}
	if len(unread) > 0 {
		out["repliesUnread"] = unread
	}
	incident.Active = len(failed) > 0
	if holidaysErr != nil {
		out["holidayCalendar"] = "unreadable, so its holidays were not skipped: " + holidaysErr.Error()
		if !incident.Active {
			incident.Active, incident.Summary = true, "Task reminders could not read a holiday calendar"
		}
	}
	return out, incident, nil
}

// taskArrivals returns the open tasks added since the person's last reminder,
// on the local date lastReminded, when they missed at least one working day's
// reminder since then, such as after time away. It looks back a month.
func taskArrivals(lastReminded string, q tasks.Queue, day time.Time, working func(time.Time) bool) (time.Time, []tasks.Task) {
	last, err := time.Parse(time.DateOnly, lastReminded)
	if err != nil || !last.Before(day) || last.Before(day.AddDate(0, -1, 0)) {
		return time.Time{}, nil
	}
	missed := false
	for d := last.AddDate(0, 0, 1); d.Before(day); d = d.AddDate(0, 0, 1) {
		missed = missed || working(d)
	}
	if !missed {
		return last, nil
	}
	since, today := last.Format(time.DateOnly), day.Format(time.DateOnly)
	arrived := []tasks.Task{}
	for _, t := range q.Tasks {
		if t.Added > since && t.Snoozed <= today {
			arrived = append(arrived, t)
		}
	}
	return last, arrived
}

// taskReminderReply lists the most pressing tasks, each with Done, Snooze and
// Not applicable buttons, then any that arrived since the last reminder and
// are not among them.
func (s *Server) taskReminderReply(person string, due []tasks.Reminder, day, since time.Time, arrived []tasks.Task) messaging.Reply {
	if len(due) == 0 {
		return messaging.Reply{}
	}
	caller := taskCaller{Person: catalog.TaskPerson{ID: person}}
	shown := due[:min(len(due), taskReminderShown)]
	text := "Your tasks for " + day.Format("Monday, January 2") + ":"
	if len(due) > len(shown) {
		text = fmt.Sprintf("Your tasks for %s: the %d most pressing of %d. Send `tasks` to see them all.", day.Format("Monday, January 2"), len(shown), len(due))
	}
	if len(arrived) > 0 {
		text = fmt.Sprintf("%d %s since your last reminder on %s.\n%s", len(arrived), plural(len(arrived), "task arrived", "tasks arrived"), since.Format("Mon Jan 2"), text)
	}
	reply := messaging.Reply{Text: text}
	until := day.AddDate(0, 0, 7).Format(time.DateOnly)
	for _, r := range shown {
		view := s.taskView(r.Task, person, caller)
		value := func(until string) string {
			raw, _ := json.Marshal(taskButtonValue{Task: r.Task.ID, Revision: r.Task.Revision, Until: until})
			return string(raw)
		}
		details := []string{r.Reason}
		if r.Task.Every != nil {
			details = append(details, "every "+strings.TrimSuffix(r.Task.Every.String(), " from "+r.Task.Every.Start))
		}
		if !view.Requester.You {
			details = append(details, "from "+taskName(view.Requester))
		}
		line := "• " + mrkdwnEscape.Replace(r.Task.Text) + " _(" + mrkdwnEscape.Replace(strings.Join(details, ", ")) + ")_" + taskSourceSuffix(view)
		reply.Sections = append(reply.Sections, messaging.Section{Text: line, Buttons: []messaging.Button{
			{Action: taskDoneAction, Label: "Done", Value: value(""), Style: "primary"},
			{Action: taskSnoozeAction, Label: "Snooze a week", Value: value(until)},
			{Action: taskNotApplicableAction, Label: "Not applicable", Value: value("")},
		}})
	}
	rest := slices.DeleteFunc(slices.Clone(arrived), func(t tasks.Task) bool {
		return slices.ContainsFunc(shown, func(r tasks.Reminder) bool { return r.Task.ID == t.ID })
	})
	if len(rest) > 0 {
		reply.Sections = append(reply.Sections, messaging.Section{Text: "Also new since " + since.Format("Mon Jan 2") + ":"})
	}
	for _, t := range rest[:min(len(rest), taskArrivalsShown)] {
		line := "• " + mrkdwnEscape.Replace(t.Text)
		if view := s.taskView(t, person, caller); !view.Requester.You {
			line += " _(from " + mrkdwnEscape.Replace(taskName(view.Requester)) + ")_"
		}
		reply.Sections = append(reply.Sections, messaging.Section{Text: line})
	}
	if len(rest) > taskArrivalsShown {
		reply.Sections = append(reply.Sections, messaging.Section{Text: fmt.Sprintf("…and %d more. Send `tasks` to see them all.", len(rest)-taskArrivalsShown)})
	}
	return reply
}

// taskWaiting is what a person's Slack said about the messages waiting on
// their reply this morning.
type taskWaiting struct {
	messages  []connectors.SlackWaiting
	unchecked int
	err       error
}

var errReplySlack = errors.New("connect Slack on your account page")

// taskWaitingFor reads the Slack messages waiting on a person's reply with
// their own grant: those sent from the start of the third working day back
// until today began, so each has had a working morning to be answered, less
// any they dismissed. The grant must be the member the reminder goes to, in
// the task workspace.
func (s *Server) taskWaitingFor(ctx context.Context, user, member, workspaceID string, local time.Time, working func(time.Time) bool, dismissed func(channel, ts string) bool) taskWaiting {
	account, err := s.replyAuthority(user)
	if err != nil {
		return taskWaiting{err: err}
	}
	connections, err := s.personalConnections.List(account.ID)
	if err != nil {
		return taskWaiting{err: err}
	}
	i := slices.IndexFunc(connections, func(c connectors.PersonalConnection) bool {
		return c.Provider == "slack" && c.State == "connected"
	})
	if i < 0 {
		return taskWaiting{err: errReplySlack}
	}
	if c := connections[i]; c.Subject != member || c.Workspace != workspaceID {
		return taskWaiting{err: errors.New("Slack is connected as another member or workspace; reconnect it on your account page")}
	}
	to := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, local.Location())
	from := to
	for n, tries := 0, 0; n < taskReplyDays && tries < taskHolidaysBack; tries++ {
		from = from.AddDate(0, 0, -1)
		if working(time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, time.UTC)) {
			n++
		}
	}
	messages, unchecked, err := s.personalConnections.SlackWaitingFor(ctx, account.ID, connections[i].Generation, from, to, dismissed, func(ctx context.Context) error {
		if _, err := s.replyAuthority(user); err != nil {
			return err
		}
		return s.checkAutomationAuth(ctx)
	})
	return taskWaiting{messages: messages, unchecked: unchecked, err: err}
}

// taskWaitingReply adds the messages waiting on the person's reply to their
// reminder: where and from whom, linked to Slack, never what they say. Authors
// are named, not mentioned, since the transport never sends mentions.
func (s *Server) taskWaitingReply(reply messaging.Reply, waiting taskWaiting, loc *time.Location, members map[string]messaging.Member) messaging.Reply {
	sections := []messaging.Section{}
	switch {
	case errors.Is(waiting.err, connectors.ErrProviderBusy):
		sections = append(sections, messaging.Section{Text: "_Slack was busy, so messages waiting on your reply are not listed today._"})
	case waiting.err != nil:
		sections = append(sections, messaging.Section{Text: "_I couldn't check Slack for messages waiting on your reply. Reconnect Slack on <" + s.origin + "/account/assistant|your account page>, or turn reply reminders off there._"})
	case len(waiting.messages) > 0 || waiting.unchecked > 0:
		sections = append(sections, messaging.Section{Text: "Waiting on your reply:"})
		for _, m := range waiting.messages {
			label := map[string]string{"dm": "Direct message", "group": "Group message", "mention": "Mention"}[m.Kind]
			if link, err := url.Parse(m.Permalink); err == nil && link.Scheme == "https" && strings.HasSuffix(link.Hostname(), ".slack.com") && !strings.ContainsAny(m.Permalink, "<>|") {
				label = "<" + m.Permalink + "|" + label + ">"
			}
			author := "someone"
			if name := members[m.Author].Name; name != "" {
				author = mrkdwnEscape.Replace(name)
			}
			line := "• " + label + " from " + author
			if m.Kind == "mention" {
				line += " in <#" + m.Channel + ">"
			}
			section := messaging.Section{Text: line + ", " + m.Sent.In(loc).Format("Mon Jan 2 3:04 PM")}
			value := taskReplyButton{Channel: m.Channel, TS: m.TS}
			if raw, _ := json.Marshal(value); value.valid() {
				section.Buttons = []messaging.Button{{Action: taskReplyDismissAction, Label: "Dismiss", Value: string(raw)}}
			}
			sections = append(sections, section)
		}
		if waiting.unchecked > 0 {
			sections = append(sections, messaging.Section{Text: fmt.Sprintf("…and %d more I didn't check. Open Slack to see them.", waiting.unchecked)})
		}
	}
	if reply.Text == "" && len(sections) > 0 {
		reply.Text, sections = sections[0].Text, sections[1:]
	}
	reply.Sections = append(reply.Sections, sections...)
	return reply
}

// taskReplyButton names the Slack message a Dismiss button drops.
type taskReplyButton struct {
	Channel string `json:"channel"`
	TS      string `json:"ts"`
}

var slackMessageTS = regexp.MustCompile(`^[0-9]{1,12}\.[0-9]{1,8}$`)

func (v taskReplyButton) valid() bool {
	return messaging.ValidID(v.Channel) && slackMessageTS.MatchString(v.TS)
}

// replyDismissal is the key of one person's dismissal of one message: a
// one-way hash, so the record names neither the message nor its conversation.
func replyDismissal(person, channel, ts string) string {
	return automation.Hash([]string{"task-reply-dismissed", person, channel, ts})
}

// reminderState is what the reminder job keeps: for reply reminders, the date
// each dismissal expires, by key, and the local day each person's Slack was
// last searched with nothing to send, by person ID; and the local day each
// person was last reminded, by person ID. All expire, so nothing builds up per
// message or per day.
type reminderState struct {
	Dismissed map[string]string `json:"dismissed"`
	Checked   map[string]string `json:"checked"`
	Reminded  map[string]string `json:"reminded"`
}

func readReminderState(raw json.RawMessage) reminderState {
	var st reminderState
	if json.Unmarshal(raw, &st) != nil {
		st = reminderState{}
	}
	if st.Dismissed == nil {
		st.Dismissed = map[string]string{}
	}
	if st.Checked == nil {
		st.Checked = map[string]string{}
	}
	if st.Reminded == nil {
		st.Reminded = map[string]string{}
	}
	return st
}

// expire drops dismissals past their date, marks for days that have ended
// everywhere and reminders older than taskArrivals looks back, and reports
// whether it dropped any. A local date is at most a day behind UTC, so a mark
// older than yesterday in UTC is for a past day.
func (st reminderState) expire(now time.Time) bool {
	today, yesterday := now.UTC().Format(time.DateOnly), now.UTC().AddDate(0, 0, -1).Format(time.DateOnly)
	lastMonth := now.UTC().AddDate(0, -1, -1).Format(time.DateOnly)
	n := len(st.Dismissed) + len(st.Checked) + len(st.Reminded)
	maps.DeleteFunc(st.Dismissed, func(_, until string) bool { return until < today })
	maps.DeleteFunc(st.Checked, func(_, day string) bool { return day < yesterday })
	maps.DeleteFunc(st.Reminded, func(_, day string) bool { return day < lastMonth })
	return len(st.Dismissed)+len(st.Checked)+len(st.Reminded) < n
}

// changeReminderState drops the expired entries, then applies change when it is
// set, and keeps the rest of the job's state.
func (s *Server) changeReminderState(spec automation.Spec, now time.Time, change func(reminderState)) error {
	_, err := s.automations.Queue.ChangeJobState(spec, func(raw json.RawMessage) (json.RawMessage, error) {
		state := map[string]json.RawMessage{}
		if err := json.Unmarshal(raw, &state); err != nil {
			return nil, err
		}
		st := readReminderState(raw)
		st.expire(now)
		if change != nil {
			change(st)
		}
		for key, entries := range map[string]map[string]string{"dismissed": st.Dismissed, "checked": st.Checked, "reminded": st.Reminded} {
			delete(state, key)
			if len(entries) > 0 {
				state[key], _ = json.Marshal(entries)
			}
		}
		return json.Marshal(state)
	})
	return err
}

// slackReplyDismiss drops one message from the presser's reply reminders. It
// changes only the presser's own list, keeps the dismissal for
// taskReplyDismissDays, and answers beside the reminder so the rest of it
// stays.
func (s *Server) slackReplyDismiss(w http.ResponseWriter, r *http.Request, settings catalog.TaskQueues, user, raw, responseURL string) {
	var value taskReplyButton
	if json.Unmarshal([]byte(raw), &value) != nil || !value.valid() {
		http.Error(w, "Invalid interaction", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	text := "Dismissed. It won't be listed again."
	caller, err := s.slackTaskMember(ctx, settings, user, false)
	specs := s.taskReminderSpecs(settings.Client)
	switch {
	case errors.Is(err, errNotTaskMember):
		text = "Only members of this workspace can dismiss messages."
	case err != nil:
		text = "Reply reminders are not set up for you."
	case len(specs) == 0:
		text = "Reply reminders are not set up here."
	}
	for _, spec := range specs {
		if err != nil {
			break
		}
		key, now := replyDismissal(caller.Person.ID, value.Channel, value.TS), s.taskTime()
		if e := s.changeReminderState(spec, now, func(st reminderState) {
			st.Dismissed[key] = now.AddDate(0, 0, taskReplyDismissDays).Format(time.DateOnly)
		}); e != nil {
			s.logAutomationError("Slack reply dismiss", e)
			text = "I couldn't dismiss that just now; try again in a moment."
		}
	}
	if err := s.slack.RespondAside(ctx, responseURL, text); err != nil {
		s.logAutomationError("Slack reply dismiss response", err)
	}
	w.WriteHeader(http.StatusOK)
}

// taskReminderSpecs are the enabled task-reminder jobs for a client, each of
// which keeps its own dismissals.
func (s *Server) taskReminderSpecs(client string) []automation.Spec {
	out := []automation.Spec{}
	for _, spec := range s.automations.Definitions.Specs() {
		if !spec.Disabled && spec.Native != nil && spec.Native.Task == "task-reminder" && spec.Native.Client == client {
			out = append(out, spec)
		}
	}
	return out
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
