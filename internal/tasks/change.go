package tasks

import (
	"errors"
	"strconv"
	"strings"
	"time"
)

// The kinds of Refusal: a caller who neither owns nor filed a task is told it
// does not exist; a change made from an older revision is stale; one the
// caller's role does not allow is forbidden; and one the caller must correct
// is invalid.
var (
	ErrNotFound  = errors.New("task not found")
	ErrStale     = errors.New("task changed since it was read")
	ErrForbidden = errors.New("task change not allowed")
	ErrInvalid   = errors.New("invalid task change")
)

// Refusal is a change Decide refuses. Kind is one of the errors above and
// Message what the caller reads.
type Refusal struct {
	Kind    error
	Message string
}

func (r Refusal) Error() string { return r.Message }
func (r Refusal) Unwrap() error { return r.Kind }

// NotFound refuses a task the caller may not see as if it did not exist.
func NotFound(id string) Refusal {
	return Refusal{ErrNotFound, "No open task " + id + " in your queue or among the tasks you filed"}
}

func forbidden(message string) Refusal { return Refusal{ErrForbidden, message} }
func invalid(message string) Refusal   { return Refusal{ErrInvalid, message} }

// Change is one person's change to one task: an outcome that closes it, a
// reassignment to another queue, or edits.
type Change struct {
	// Revision is the task's revision the change was made from.
	Revision string
	// The edits: a nil field is left alone, and an empty one clears it.
	Text, Due, Priority, SnoozeUntil, Every *string
	Lead                                    *int
	// Outcome is done, not_applicable or withdrawn.
	Outcome string
	// Reason says why, in the task's history and to whoever hears of it.
	Reason string
	// ReassignTo is the person ID of the queue the task moves to.
	ReassignTo string
}

func (c Change) edits() bool {
	return c.Text != nil || c.Due != nil || c.Priority != nil || c.SnoozeUntil != nil || c.Every != nil || c.Lead != nil
}

// Decision is what an allowed change does.
type Decision struct {
	// Task is the task afterwards, held in Owner's queue, unless Closed says
	// it left every queue; Task is then the task as it was.
	Task  Task
	Owner string
	// Closed is the outcome (done, not_applicable or withdrawn) that took the
	// task out of its queue, or reassigned when it moved to Owner's. It is
	// empty when the task stays.
	Closed string
	// Occurrence is the occurrence of a recurring task the change closed;
	// the task stays, due on its next date.
	Occurrence *Closing
	// Reason is the change's reason, trimmed.
	Reason string
	// Summary is the subject of the commit that records the change: the task,
	// what happened and who did it. TaskHistory reads it back.
	Summary string
}

// CheckReason accepts a reason that, trimmed, is one line of at most 200
// bytes without backticks or credentials.
func CheckReason(reason string) error {
	reason = strings.TrimSpace(reason)
	if len(reason) > 200 || strings.ContainsAny(reason, "\r\n`") || Credential.MatchString(reason) {
		return invalid("A reason is one line of at most 200 bytes, without backticks or credentials")
	}
	return nil
}

// Decide checks a change that caller, a person ID, makes to a task as it is
// now in owner's queue, and returns what the change does or its Refusal. The
// owner may do anything but withdraw a task someone else filed; the requester
// may withdraw it and change its text, due date, schedule and lead time; to
// anyone else it does not exist. today dates the closing of an occurrence.
func Decide(t Task, owner, caller string, c Change, today time.Time) (Decision, error) {
	if err := CheckReason(c.Reason); err != nil {
		return Decision{}, err
	}
	isOwner, isRequester := owner == caller, t.From == caller
	if !isOwner && !isRequester {
		return Decision{}, NotFound(t.ID)
	}
	if c.Revision != t.Revision {
		return Decision{}, Refusal{ErrStale, "Task " + t.ID + " changed since you read it; list it again and retry"}
	}
	d := Decision{Task: t, Owner: owner, Reason: strings.TrimSpace(c.Reason)}
	var what string
	var err error
	switch {
	case c.Outcome != "":
		what, err = d.close(c.Outcome, isOwner, isRequester, today)
	case c.ReassignTo != "":
		what, err = d.reassign(c, isOwner)
	default:
		what, err = d.edit(c, isOwner)
	}
	if err != nil {
		return Decision{}, err
	}
	d.Summary = "Task " + t.ID + ": " + what + " by " + caller
	if o := d.Occurrence; o != nil {
		if o.Missed > 0 {
			d.Summary += "; missed " + strconv.Itoa(o.Missed)
		}
		d.Summary += "; next " + o.Next
	}
	if d.Reason != "" {
		d.Summary += ": " + d.Reason
	}
	return d, nil
}

// close closes the task with an outcome, or for a recurring task closes its
// current occurrence and moves it, awake, to its next date.
func (d *Decision) close(outcome string, isOwner, isRequester bool, today time.Time) (string, error) {
	switch outcome {
	case "done", "not_applicable":
	case "withdrawn":
		if !isRequester {
			return "", forbidden("Only the person who filed a task withdraws it")
		}
		d.Closed = outcome
		return outcome, nil
	default:
		return "", invalid("Outcome must be done, not_applicable or withdrawn")
	}
	switch {
	case !isOwner:
		return "", forbidden("Only the queue's owner marks a task done or not applicable; you can withdraw a task you filed")
	case outcome == "not_applicable" && d.Reason == "":
		return "", invalid("Say why the task is not applicable")
	}
	what := strings.ReplaceAll(outcome, "_", " ")
	if d.Task.Every == nil {
		d.Closed = outcome
		return what, nil
	}
	closing := d.Task.Every.Close(d.Task.Due, today)
	d.Occurrence = &closing
	d.Task.Due, d.Task.Snoozed = closing.Next, ""
	return what + " for " + closing.Closed, nil
}

// reassign moves the task to another queue, whose owner sets its priority
// and snooze afresh.
func (d *Decision) reassign(c Change, isOwner bool) (string, error) {
	switch {
	case !isOwner:
		return "", forbidden("Only the queue's owner reassigns a task")
	case c.ReassignTo == d.Owner:
		return "", invalid("The task is already in that queue")
	case c.edits():
		return "", invalid("Reassign a task on its own, then change it")
	}
	what := "reassigned from " + d.Owner + " to " + c.ReassignTo
	d.Task.Priority, d.Task.Snoozed = "", ""
	d.Owner, d.Closed = c.ReassignTo, "reassigned"
	return what, nil
}

// edit changes the fields the change sets. Only the owner sets priority and
// snoozes. A new due date restarts a schedule from that date.
func (d *Decision) edit(c Change, isOwner bool) (string, error) {
	t, changed := d.Task, []string{}
	if c.Text != nil {
		t.Text = strings.TrimSpace(*c.Text)
		changed = append(changed, "text")
	}
	if c.Due != nil {
		t.Due = strings.TrimSpace(*c.Due)
		changed = append(changed, "due date")
	}
	if c.Priority != nil {
		if !isOwner {
			return "", forbidden("Only the queue's owner sets priority")
		}
		switch p := strings.ToLower(strings.TrimSpace(*c.Priority)); p {
		case "", "normal":
			t.Priority = ""
		case "high", "low":
			t.Priority = p
		default:
			return "", invalid("Priority must be high, normal or low")
		}
		changed = append(changed, "priority")
	}
	if c.SnoozeUntil != nil {
		if !isOwner {
			return "", forbidden("Only the queue's owner snoozes a task")
		}
		t.Snoozed = strings.TrimSpace(*c.SnoozeUntil)
		changed = append(changed, "snooze")
	}
	if c.Every != nil {
		what, err := reschedule(&t, *c.Every, d.Reason)
		if err != nil {
			return "", err
		}
		changed = append(changed, what)
	} else if t.Every != nil && t.Due != d.Task.Due {
		if t.Due == "" {
			return "", invalid("A recurring task keeps a due date; stop repeating it first")
		}
		every := *t.Every
		every.Start = t.Due
		t.Every = &every
	}
	if c.Lead != nil {
		t.Lead = *c.Lead
		changed = append(changed, "lead time")
	}
	if len(changed) == 0 {
		return "", invalid("Nothing to change")
	}
	if err := t.Check(); err != nil {
		return "", invalid(err.Error())
	}
	d.Task = t
	return "changed " + strings.Join(changed, ", "), nil
}

// reschedule repeats a task on a new schedule from its due date, or with an
// empty schedule and a reason stops it repeating.
func reschedule(t *Task, every, reason string) (string, error) {
	if strings.TrimSpace(every) == "" {
		switch {
		case t.Every == nil:
			return "", invalid("The task does not repeat")
		case reason == "":
			return "", invalid("Say why the task stops repeating")
		}
		t.Every, t.Lead = nil, 0
		return "stopped repeating", nil
	}
	if t.Due == "" {
		return "", invalid("A recurring task needs a due date: its first date")
	}
	r, err := ParseRecurrence(every, t.Due)
	if err != nil {
		return "", invalid(err.Error())
	}
	t.Every = &r
	return "schedule", nil
}

// Notice names what people other than the caller hear once a decided change
// is made: the outcome that closed the task or one of its occurrences,
// reassigned, or moved earlier when the change brought the due date forward
// and its caller is not the owner. after is the task as it now stands in
// owner's queue, or as it was before the change when it left every queue.
func (d Decision) Notice(c Change, caller string, before, after Task, owner string) string {
	switch {
	case d.Closed == "reassigned":
		return "reassigned"
	case d.Closed != "" || d.Occurrence != nil:
		return c.Outcome
	case c.Due != nil && before.Due != "" && after.Due != "" && after.Due < before.Due && caller != owner:
		return "moved earlier"
	}
	return ""
}
