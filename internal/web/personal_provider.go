package web

import "forgetmenote/internal/connectors"

// Personal provider results arrive as decoded JSON maps
// (connectors.PersonalConnections.Call): Gmail views shaped by the email
// connector, and Slack and Google Calendar responses as the providers send
// them. These views read the fields collection depends on, each in one
// place. A missing or mistyped field reads as its zero value; the caller
// decides which absences are errors.

// emailPage is one page of email.messages or email.changes.
type emailPage struct {
	expired bool     // the change cursor expired, so the mailbox needs a rescan
	ids     []string // message IDs; "" where an entry omitted its ID
	next    string   // the next page of the same listing
	cursor  string   // the change cursor after the last page (changes only)
}

func readEmailPage(data map[string]any, changes bool) emailPage {
	p := emailPage{expired: data["expired"] == true, next: personalString(data["nextPageToken"]), cursor: personalString(data["cursor"])}
	entries := personalArray(data["messages"])
	if changes {
		entries = personalArray(data["changes"])
	}
	for _, v := range entries {
		m := personalObject(v)
		id := personalString(m["id"])
		if id == "" {
			id = personalString(m["messageId"])
		}
		p.ids = append(p.ids, id)
	}
	return p
}

// emailStatusCursor is the change cursor that email.status reports.
func emailStatusCursor(data map[string]any) string {
	return personalString(data["cursor"])
}

// emailMessage is an email.message result: the message's thread, or that the
// message is gone.
type emailMessage struct {
	unavailable bool
	thread      string
}

func readEmailMessage(data map[string]any) emailMessage {
	return emailMessage{unavailable: data["unavailable"] == true, thread: personalString(data["threadId"])}
}

// emailThread is an email.thread result, as far as deciding whether it waits
// for its owner.
type emailThread struct {
	complete bool
	messages int
	last     emailThreadMessage // the latest message, when there is one
}

type emailThreadMessage struct {
	sender, account string
	flags           []string
	listID          bool   // sent through a mailing list
	autoSubmitted   string // the Auto-Submitted header
}

func readEmailThread(data map[string]any) emailThread {
	messages := personalArray(data["messages"])
	t := emailThread{complete: data["complete"] == true, messages: len(messages)}
	if len(messages) == 0 {
		return t
	}
	last := personalObject(messages[len(messages)-1])
	headers := personalObject(last["headers"])
	t.last = emailThreadMessage{sender: personalString(last["sender"]), account: personalString(last["account"]), listID: headers["list-id"] != nil, autoSubmitted: personalString(headers["auto-submitted"])}
	for _, flag := range personalArray(last["flags"]) {
		t.last.flags = append(t.last.flags, personalString(flag))
	}
	return t
}

// slackList is a slack.conversations page.
type slackList struct {
	listed bool     // the page carried a conversation list
	ids    []string // conversation IDs; "" where one omitted its ID
	next   string
}

func readSlackList(data map[string]any) slackList {
	channels, ok := data["channels"].([]any)
	l := slackList{listed: ok, next: slackNextCursor(data)}
	for _, v := range channels {
		l.ids = append(l.ids, personalString(personalObject(v)["id"]))
	}
	return l
}

// slackHistory is a slack.history or slack.thread page.
type slackHistory struct {
	listed   bool // the page carried a message list
	messages []slackMessage
	next     string
	hasMore  bool // more messages exist; with no next cursor, the page is truncated
}

type slackMessage struct {
	user, ts, thread string
	automated        bool // posted by a bot or as a subtype, such as a join
}

func readSlackHistory(data map[string]any) slackHistory {
	messages, ok := data["messages"].([]any)
	h := slackHistory{listed: ok, next: slackNextCursor(data), hasMore: data["has_more"] == true}
	for _, v := range messages {
		m := personalObject(v)
		h.messages = append(h.messages, slackMessage{user: personalString(m["user"]), ts: personalString(m["ts"]), thread: personalString(m["thread_ts"]), automated: m["bot_id"] != nil || m["subtype"] != nil})
	}
	return h
}

func slackNextCursor(data map[string]any) string {
	return personalString(personalObject(data["response_metadata"])["next_cursor"])
}

// calendarPage is a calendar.events page, or a calendar.event result as a
// page of one.
type calendarPage struct {
	malformed bool // events were present but not a list
	events    []calendarEvent
	next      string
}

type calendarEvent struct {
	id        string
	cancelled bool
	link      string
	data      map[string]any // the event as the provider returned it
}

func readCalendarPage(data map[string]any, single bool) calendarPage {
	items, ok := data["items"].([]any)
	if single {
		items, ok = []any{data}, true
	}
	p := calendarPage{malformed: !ok && data["items"] != nil, next: personalString(data["nextPageToken"])}
	for _, v := range items {
		event := personalObject(v)
		p.events = append(p.events, calendarEvent{id: personalString(event["id"]), cancelled: event["status"] == "cancelled", link: personalString(event["htmlLink"]), data: event})
	}
	return p
}

// personalArgument is a string argument of a personal read the server queued.
func personalArgument(call connectors.PersonalOperation, name string) string {
	return personalString(call.Arguments[name])
}
