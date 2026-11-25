package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"forgetmenote/internal/automation"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/connectors"
)

func personalObject(v any) map[string]any { out, _ := v.(map[string]any); return out }
func personalArray(v any) []any           { out, _ := v.([]any); return out }
func personalString(v any) string         { out, _ := v.(string); return out }
func personalData(result connectors.Result) (map[string]any, error) {
	raw, err := json.Marshal(result.Data)
	var out map[string]any
	if err == nil {
		err = json.Unmarshal(raw, &out)
	}
	if out == nil && err == nil {
		err = errors.New("personal provider omitted structured data")
	}
	return out, err
}
func personalPayload(data map[string]any) json.RawMessage {
	// Volatile fetch timestamps and transport receipts must not trigger new work.
	var strip func(any)
	strip = func(v any) {
		switch o := v.(type) {
		case map[string]any:
			delete(o, "retrievedAt")
			delete(o, "response_metadata")
			for _, child := range o {
				strip(child)
			}
		case []any:
			for _, child := range o {
				strip(child)
			}
		}
	}
	strip(data)
	raw, _ := json.Marshal(data)
	return raw
}

// personalCollector is one collection pass for a personal assistant: its
// item budget, lookback horizon and clock.
type personalCollector struct {
	s        *Server
	r        *automation.Run
	state    *personalState
	persist  func() error
	settings catalog.AssistantSettings
	batch    int
	lookback time.Duration
	now      time.Time
}

func (s *Server) collectPersonal(ctx context.Context, r *automation.Run, state *personalState, persist func() error) error {
	if len(state.Pending) > 0 {
		return nil
	}
	settings, err := s.catalog.Assistant(r.Spec.Actor)
	if err != nil {
		return err
	}
	policy := s.catalog.AssistantPolicy()
	c := &personalCollector{s: s, r: r, state: state, persist: persist, settings: settings, batch: policy.BatchItems, lookback: time.Duration(policy.LookbackHours) * time.Hour, now: time.Now().UTC().Truncate(time.Second)}
	if c.batch == 0 {
		c.batch = 12
	}
	if c.lookback == 0 {
		c.lookback = 48 * time.Hour
	}
	// Provider pages are checkpointed before consuming individual items. Failed
	// reads retain their position; a restart cannot skip an unexamined message.
	if len(state.Reads) == 0 {
		if err = c.plan(ctx); err != nil {
			return err
		}
		if err = persist(); err != nil {
			return err
		}
	}
	for remaining := c.batch; remaining > 0 && len(state.Reads) > 0; remaining-- {
		observations, err := c.consume(ctx, state.Reads[0])
		if err != nil {
			return err
		}
		for _, o := range observations {
			observePersonal(state, o)
		}
		state.Reads = state.Reads[1:]
		if err = persist(); err != nil {
			return err
		}
	}
	return nil
}

func (c *personalCollector) fetch(ctx context.Context, op connectors.PersonalOperation) (map[string]any, error) {
	return c.s.personalEvidence(ctx, op)
}

func personalOperation(connection connectors.PersonalConnection, name string, args map[string]any) connectors.PersonalOperation {
	return connectors.PersonalOperation{Provider: connection.Provider, Generation: connection.Generation, Operation: name, Arguments: args}
}

// plan observes the data watches and queues the provider pages to read in
// this pass, one listing page per connection and feature.
func (c *personalCollector) plan(ctx context.Context) error {
	if slices.Contains(c.settings.Features, "operations") {
		if err := c.observeWatches(ctx); err != nil {
			return err
		}
	}
	var connections []connectors.PersonalConnection
	if c.s.personalConnections != nil {
		var err error
		if connections, err = c.s.personalConnections.List(c.r.Spec.Personal.Account); err != nil {
			return err
		}
	}
	for _, connection := range connections {
		if connection.State != "connected" {
			continue
		}
		if err := c.planConnection(ctx, connection); err != nil {
			return err
		}
	}
	return nil
}

// planConnection queues the pages of each enabled feature of a connection.
func (c *personalCollector) planConnection(ctx context.Context, connection connectors.PersonalConnection) error {
	enabled := func(provider, feature string) bool {
		return connection.Provider == provider && slices.Contains(c.settings.Features, feature)
	}
	if enabled("google", "email") {
		// An expired change cursor ends this connection's planning for the pass.
		if expired, err := c.planEmail(ctx, connection); expired || err != nil {
			return err
		}
	}
	if enabled("slack", "slack") {
		if err := c.planSlack(ctx, connection); err != nil {
			return err
		}
	}
	if enabled("google", "calendar") {
		// A rolling horizon catches reschedules, cancellations and approaching
		// events. Event etags, not the moving query window, drive generation.
		c.state.Reads = append(c.state.Reads, personalOperation(connection, "calendar.events", map[string]any{"calendar": "primary", "timeMin": c.now.Format(time.RFC3339), "timeMax": c.now.Add(c.lookback).Format(time.RFC3339)}))
	}
	return nil
}

// observeWatches runs each configured read-only data watch and observes its
// result directly.
func (c *personalCollector) observeWatches(ctx context.Context) error {
	for _, watch := range c.settings.Watches {
		op, ok := c.s.dataConnections.ConfiguredOperation(watch.Connection, watch.Operation)
		if !ok || !op.ReadOnly {
			return errors.New("personal data watches require an allowed read-only operation")
		}
		var args map[string]any
		if json.Unmarshal(watch.Arguments, &args) != nil {
			return errors.New("invalid watch arguments")
		}
		call := automation.Call{Connection: watch.Connection, Operation: watch.Operation, Arguments: args}
		result, err := c.s.automationCall(ctx, c.r, call)
		if err != nil {
			return err
		}
		raw, err := json.Marshal(result.Data)
		if err != nil {
			return err
		}
		observePersonal(c.state, personalObservation{Key: "data:" + automation.Hash(call), Kind: "operations", Revision: automation.Hash(raw), URL: "receipt:" + result.Receipt.ID, Observed: c.now, Data: &call, Content: raw})
	}
	return nil
}

// planEmail queues the messages on the mailbox's next page. A mailbox is
// first scanned over the lookback window from an anchored change cursor,
// then followed through changes from that anchor. An expired change cursor
// starts a new scan in the next pass and reports expired.
func (c *personalCollector) planEmail(ctx context.Context, connection connectors.PersonalConnection) (bool, error) {
	cursors := c.state.Cursors
	prefix := fmt.Sprintf("email:%d:", connection.Generation)
	mode := cursors[prefix+"mode"]
	if mode == "" {
		status, err := c.fetch(ctx, personalOperation(connection, "email.status", map[string]any{}))
		if err != nil {
			return false, err
		}
		cursors[prefix+"anchor"] = emailStatusCursor(status)
		cursors[prefix+"mode"] = "scan"
		cursors[prefix+"after"] = c.now.Add(-c.lookback).Format(time.RFC3339)
		mode = "scan"
	}
	args := map[string]any{"pageSize": min(c.batch, 500), "pageToken": cursors[prefix+"page"]}
	name := "email.changes"
	if mode == "scan" {
		name = "email.messages"
		args["filter"] = map[string]any{"after": cursors[prefix+"after"], "excludeFlags": []string{"spam", "trash", "draft"}}
	} else {
		args["cursor"] = cursors[prefix+"cursor"]
	}
	data, err := c.fetch(ctx, personalOperation(connection, name, args))
	if err != nil {
		return false, err
	}
	page := readEmailPage(data, mode != "scan")
	if page.expired {
		delete(cursors, prefix+"mode")
		delete(cursors, prefix+"page")
		return true, c.persist()
	}
	for _, id := range page.ids {
		if id == "" {
			return false, errors.New("email update omitted message ID")
		}
		c.state.Reads = append(c.state.Reads, personalOperation(connection, "email.message", map[string]any{"id": id}))
	}
	cursors[prefix+"page"] = page.next
	if page.next == "" {
		if mode == "scan" {
			cursors[prefix+"cursor"] = cursors[prefix+"anchor"]
		} else {
			cursors[prefix+"cursor"] = page.cursor
		}
		cursors[prefix+"mode"] = "changes"
	}
	return false, nil
}

// planSlack queues the lookback history of each conversation on the next
// page of the account's conversation list.
func (c *personalCollector) planSlack(ctx context.Context, connection connectors.PersonalConnection) error {
	prefix := fmt.Sprintf("slack:%d:", connection.Generation)
	data, err := c.fetch(ctx, personalOperation(connection, "slack.conversations", map[string]any{"cursor": c.state.Cursors[prefix+"page"]}))
	if err != nil {
		return err
	}
	list := readSlackList(data)
	if !list.listed {
		return errors.New("Slack omitted conversations")
	}
	for _, id := range list.ids {
		if id == "" {
			return errors.New("Slack omitted conversation ID")
		}
		args := map[string]any{"channel": id, "oldest": strconv.FormatInt(c.now.Add(-c.lookback).Unix(), 10)}
		c.state.Reads = append(c.state.Reads, personalOperation(connection, "slack.history", args))
	}
	c.state.Cursors[prefix+"page"] = list.next
	return nil
}

// consume reads one queued page or item. It queues the reads it leads to,
// settles sources that no longer wait for the owner, and returns the
// observations to judge.
func (c *personalCollector) consume(ctx context.Context, call connectors.PersonalOperation) ([]personalObservation, error) {
	data, err := c.fetch(ctx, call)
	if err != nil {
		return nil, err
	}
	switch call.Operation {
	case "email.message", "email.thread":
		return c.consumeEmail(ctx, call, data)
	case "slack.history", "slack.thread":
		return c.consumeSlack(call, data)
	case "calendar.events", "calendar.event":
		return c.consumeCalendar(call, data)
	}
	return nil, nil
}

func (c *personalCollector) observation(key, kind, link string, source connectors.PersonalOperation, data map[string]any) personalObservation {
	raw := personalPayload(data)
	return personalObservation{Key: key, Kind: kind, Revision: personalContentRevision(kind, raw), URL: link, Observed: c.now, Source: source, Content: raw}
}

// consumeEmail observes a message's whole thread while it waits for the
// owner. A thread the owner answered last, or one from a list, an automatic
// sender, spam or trash, is resolved; a prepared draft changes nothing.
func (c *personalCollector) consumeEmail(ctx context.Context, call connectors.PersonalOperation, data map[string]any) ([]personalObservation, error) {
	message := readEmailMessage(data)
	if message.unavailable {
		return nil, nil
	}
	id := message.thread
	if call.Operation == "email.thread" {
		id = personalArgument(call, "id")
	}
	if id == "" {
		return nil, errors.New("email omitted thread")
	}
	source := call
	source.Operation = "email.thread"
	source.Arguments = map[string]any{"id": id}
	thread := data
	if call.Operation != "email.thread" {
		var err error
		if thread, err = c.fetch(ctx, source); err != nil {
			return nil, err
		}
	}
	view := readEmailThread(thread)
	if !view.complete {
		return nil, errors.New("incomplete email thread; assistant did not analyze partial evidence")
	}
	if view.messages == 0 {
		return nil, errors.New("email thread empty")
	}
	last := view.last
	// A prepared mailbox draft is not a new request or a sent reply.
	if slices.Contains(last.flags, "draft") {
		return nil, nil
	}
	if slices.ContainsFunc(last.flags, func(flag string) bool { return flag == "spam" || flag == "trash" }) || strings.EqualFold(last.sender, last.account) || last.listID || last.autoSubmitted != "" && last.autoSubmitted != "no" {
		resolvePersonal(c.state, "email:"+id, "resolved")
		return nil, nil
	}
	return []personalObservation{c.observation("email:"+id, "email", "https://mail.google.com/mail/u/?authuser="+url.QueryEscape(last.account)+"#all/"+url.PathEscape(id), source, thread)}, nil
}

// consumeSlack follows a history page's continuation and queues each thread
// on it, or observes a thread still waiting for the owner. A thread the owner
// answered last, or an empty one, is resolved.
func (c *personalCollector) consumeSlack(call connectors.PersonalOperation, data map[string]any) ([]personalObservation, error) {
	page := readSlackHistory(data)
	if !page.listed {
		return nil, errors.New("Slack omitted messages")
	}
	if page.next != "" {
		more := call
		more.Arguments = clonePersonalArgs(call.Arguments)
		more.Arguments["cursor"] = page.next
		c.state.Reads = append(c.state.Reads, more)
	} else if page.hasMore {
		return nil, errors.New("Slack history truncated without continuation")
	}
	channel := personalArgument(call, "channel")
	if call.Operation != "slack.thread" {
		return nil, queueSlackThreads(c.state, call, channel, page.messages)
	}
	key := "slack:" + channel + ":" + personalArgument(call, "ts")
	connections, err := c.s.personalConnections.List(c.r.Spec.Personal.Account)
	if err != nil {
		return nil, err
	}
	own := ""
	for _, connection := range connections {
		if connection.Provider == "slack" {
			own = connection.Subject
		}
	}
	if len(page.messages) == 0 || page.messages[len(page.messages)-1].user == own {
		resolvePersonal(c.state, key, "resolved")
		return nil, nil
	}
	return []personalObservation{c.observation(key, "slack", "slack://channel?id="+url.QueryEscape(channel)+"&message="+url.QueryEscape(personalArgument(call, "ts")), call, data)}, nil
}

// queueSlackThreads queues a read of the thread of each human message on a
// history page, once: a thread already queued is not queued again.
//
// The queued thread reads are hashed once per page, not once per message: a
// page of 100 messages behind 5,000 pending reads otherwise hashed a million
// reads. Equal hashes mean equal operations, so only thread reads are hashed.
func queueSlackThreads(state *personalState, call connectors.PersonalOperation, channel string, messages []slackMessage) error {
	var queued map[string]bool
	for _, m := range messages {
		if m.automated {
			continue
		}
		ts := m.ts
		if ts == "" {
			return errors.New("Slack message omitted timestamp")
		}
		if m.thread != "" {
			ts = m.thread
		}
		source := call
		source.Operation = "slack.thread"
		source.Arguments = map[string]any{"channel": channel, "ts": ts}
		if queued == nil {
			queued = map[string]bool{}
			for _, read := range state.Reads {
				if read.Operation == source.Operation {
					queued[automation.Hash(read)] = true
				}
			}
		}
		if key := automation.Hash(source); !queued[key] {
			queued[key] = true
			state.Reads = append(state.Reads, source)
		}
	}
	return nil
}

// consumeCalendar follows an events page's continuation and observes each
// event; a cancelled event is resolved.
func (c *personalCollector) consumeCalendar(call connectors.PersonalOperation, data map[string]any) ([]personalObservation, error) {
	page := readCalendarPage(data, call.Operation == "calendar.event")
	if page.next != "" {
		more := call
		more.Arguments = clonePersonalArgs(call.Arguments)
		more.Arguments["pageToken"] = page.next
		c.state.Reads = append(c.state.Reads, more)
	}
	if page.malformed {
		return nil, errors.New("calendar returned malformed events")
	}
	observations := []personalObservation{}
	for _, event := range page.events {
		if event.id == "" {
			return nil, errors.New("calendar event missing ID")
		}
		if event.cancelled {
			resolvePersonal(c.state, "calendar:"+event.id, "resolved")
			continue
		}
		source := call
		source.Operation = "calendar.event"
		source.Arguments = map[string]any{"calendar": "primary", "id": event.id}
		observations = append(observations, c.observation("calendar:"+event.id, "calendar", event.link, source, event.data))
	}
	return observations, nil
}

func clonePersonalArgs(in map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range in {
		out[k] = v
	}
	return out
}
