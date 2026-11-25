// Package tasks owns the format of task queue notes, recurring schedules and
// the rules for changing a task. A queue note holds one person's open tasks;
// task actions in the web package are its only writers and Git history is its
// record.
package tasks

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Credential matches known credential formats, refused before they reach
// notebook Git, whose history is never rewritten. Agent memory uses it too.
var Credential = regexp.MustCompile(`AKIA[0-9A-Z]{16}|ASIA[0-9A-Z]{16}|gh[pousr]_[A-Za-z0-9]{36}|github_pat_[A-Za-z0-9_]{22,}|xox[abeoprs]-[A-Za-z0-9-]{10,}|sk-[A-Za-z0-9_-]{20,}|AIza[0-9A-Za-z_-]{35}|-----BEGIN [A-Z ]*PRIVATE KEY|eyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.|(?i:\b(?:password|passwd|secret|api[_-]?key|access[_-]?token)\s*[:=]\s*\S{6,})`)

var (
	taskID   = regexp.MustCompile(`^t-[a-z2-7]{8}$`)
	personID = regexp.MustCompile(`^p-[a-z2-7]{10}$`)
	metaLine = regexp.MustCompile("^  `(t-[a-z0-9]{8}) · (.+)`\\s*$")
)

const header = "# Tasks\n\nOpen tasks. Task actions keep this note: add, change and close tasks with them, so each change records its outcome and who made it.\n"

// Recurrence repeats a task every N units counted from Start, so a late
// completion never shifts later dates.
type Recurrence struct {
	N     int    `json:"n"`
	Unit  string `json:"unit"` // days, weeks, months or years
	Start string `json:"start"`
}

// Task is one open task. From is the requester's person ID; the owner is the
// person whose note holds it. Revision identifies this version of the task.
type Task struct {
	ID       string      `json:"id"`
	Text     string      `json:"text"`
	From     string      `json:"from"`
	Added    string      `json:"added"`
	Due      string      `json:"due,omitempty"`
	Priority string      `json:"priority,omitempty"` // high or low; normal is omitted
	Snoozed  string      `json:"snoozedUntil,omitempty"`
	Every    *Recurrence `json:"every,omitempty"`
	// Lead is how many working days before each due date a recurring task
	// joins the weekday reminder; 0 uses its schedule's default.
	Lead     int    `json:"lead,omitempty"`
	Source   string `json:"source,omitempty"`
	Revision string `json:"revision"`
}

// Queue is one person's note.
type Queue struct{ Tasks []Task }

// Closing is the outcome of closing one occurrence of a recurring task.
type Closing struct {
	Closed string `json:"closed"`
	Missed int    `json:"missed,omitempty"`
	Next   string `json:"next"`
}

func ValidID(id string) bool { return taskID.MatchString(id) }

func validDate(value string) bool {
	_, err := time.Parse(time.DateOnly, value)
	return err == nil
}

// CheckText accepts one line of task text without credentials.
func CheckText(text string) error {
	switch {
	case strings.TrimSpace(text) == "" || !utf8.ValidString(text) || utf8.RuneCountInString(text) > 500 || strings.ContainsFunc(text, unicode.IsControl):
		return errors.New("task text must be one line of 1 to 500 characters")
	case Credential.MatchString(text):
		return errors.New("this looks like a credential; tasks never store secrets. Say where the secret lives instead")
	}
	return nil
}

// CheckSource accepts an empty source or an HTTPS link without spaces or
// backticks, such as a chat message link.
func CheckSource(source string) error {
	if source == "" {
		return nil
	}
	u, err := url.Parse(source)
	if err != nil || u.Scheme != "https" || u.Host == "" || len(source) > 500 || strings.ContainsAny(source, " `\t\n\r") || Credential.MatchString(source) {
		return errors.New("a source must be an HTTPS link of at most 500 characters")
	}
	return nil
}

// Check validates a task before it is written.
func (t Task) Check() error {
	switch {
	case !ValidID(t.ID) || !personID.MatchString(t.From) || !validDate(t.Added):
		return errors.New("invalid task identity")
	case t.Due != "" && !validDate(t.Due) || t.Snoozed != "" && !validDate(t.Snoozed):
		return errors.New("dates must look like 2026-11-21")
	case t.Priority != "" && t.Priority != "high" && t.Priority != "low":
		return errors.New("priority must be high, normal or low")
	case t.Every != nil && (t.Every.check() != nil || t.Due == ""):
		return errors.New("a recurring task needs a valid schedule and a due date")
	case t.Lead < 0 || t.Lead > MaxLead || t.Lead > 0 && t.Every == nil:
		return fmt.Errorf("only a recurring task has a lead time, of 1 to %d working days", MaxLead)
	}
	if err := CheckText(t.Text); err != nil {
		return err
	}
	return CheckSource(t.Source)
}

func (t Task) lines() []string {
	meta := []string{t.ID, "from: " + t.From, "added: " + t.Added}
	if t.Due != "" {
		meta = append(meta, "due: "+t.Due)
	}
	if t.Priority != "" {
		meta = append(meta, "priority: "+t.Priority)
	}
	if t.Snoozed != "" {
		meta = append(meta, "snoozed until: "+t.Snoozed)
	}
	if t.Every != nil {
		meta = append(meta, "every: "+t.Every.String())
	}
	if t.Lead > 0 {
		meta = append(meta, "lead: "+leadString(t.Lead))
	}
	if t.Source != "" {
		meta = append(meta, "source: "+t.Source)
	}
	return []string{"- " + t.Text, "  `" + strings.Join(meta, " · ") + "`"}
}

func revision(lines []string) string {
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:6])
}

// Format writes the note. Tasks keep their order.
func (q Queue) Format() string {
	var b strings.Builder
	b.WriteString(header)
	if len(q.Tasks) > 0 {
		b.WriteString("\n")
	}
	for _, t := range q.Tasks {
		b.WriteString(strings.Join(t.lines(), "\n") + "\n")
	}
	return b.String()
}

// Parse reads a note written by Format. Anything else is refused, so a write
// never drops lines it did not understand.
func Parse(content string) (Queue, error) {
	q := Queue{Tasks: []Task{}}
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	for i := 0; i < len(lines); i++ {
		text, ok := strings.CutPrefix(lines[i], "- ")
		if !ok {
			if strings.HasPrefix(lines[i], "  `") {
				return q, fmt.Errorf("line %d: task metadata without a task", i+1)
			}
			continue
		}
		if i+1 >= len(lines) {
			return q, fmt.Errorf("line %d: a task without its metadata", i+1)
		}
		m := metaLine.FindStringSubmatch(lines[i+1])
		if m == nil {
			return q, fmt.Errorf("line %d: a task without its metadata", i+1)
		}
		t := Task{ID: m[1], Text: text}
		for _, field := range strings.Split(m[2], " · ") {
			key, value, ok := strings.Cut(field, ": ")
			if !ok {
				return q, fmt.Errorf("line %d: unknown task field %q", i+2, field)
			}
			switch key {
			case "from":
				t.From = value
			case "added":
				t.Added = value
			case "due":
				t.Due = value
			case "priority":
				t.Priority = value
			case "snoozed until":
				t.Snoozed = value
			case "every":
				r, err := parseEvery(value)
				if err != nil {
					return q, fmt.Errorf("line %d: %w", i+2, err)
				}
				t.Every = &r
			case "lead":
				n, err := parseLead(value)
				if err != nil {
					return q, fmt.Errorf("line %d: %w", i+2, err)
				}
				t.Lead = n
			case "source":
				t.Source = value
			default:
				return q, fmt.Errorf("line %d: unknown task field %q", i+2, key)
			}
		}
		t.Revision = revision(lines[i : i+2])
		q.Tasks = append(q.Tasks, t)
		i++
	}
	return q, nil
}

// Find returns a task's index, or -1.
func (q Queue) Find(id string) int {
	for i, t := range q.Tasks {
		if t.ID == id {
			return i
		}
	}
	return -1
}

var units = map[string]string{"day": "days", "days": "days", "week": "weeks", "weeks": "weeks", "month": "months", "months": "months", "year": "years", "years": "years"}
var named = map[string]Recurrence{"daily": {N: 1, Unit: "days"}, "weekly": {N: 1, Unit: "weeks"}, "monthly": {N: 1, Unit: "months"}, "quarterly": {N: 3, Unit: "months"}, "yearly": {N: 1, Unit: "years"}, "annually": {N: 1, Unit: "years"}}

// ParseRecurrence reads a schedule such as "3 months", "every 2 weeks" or
// "quarterly", counted from start.
func ParseRecurrence(every, start string) (Recurrence, error) {
	words := strings.Fields(strings.ToLower(every))
	if len(words) > 0 && words[0] == "every" {
		words = words[1:]
	}
	r, ok := Recurrence{}, false
	switch len(words) {
	case 1:
		if r, ok = named[words[0]]; !ok {
			r, ok = Recurrence{N: 1, Unit: units[words[0]]}, units[words[0]] != ""
		}
	case 2:
		n, err := strconv.Atoi(words[0])
		r, ok = Recurrence{N: n, Unit: units[words[1]]}, err == nil && units[words[1]] != ""
	}
	r.Start = start
	if !ok || r.check() != nil {
		return Recurrence{}, errors.New(`a schedule repeats every 1 to 24 days, weeks, months or years, such as "3 months" or "quarterly"`)
	}
	return r, nil
}

func parseEvery(value string) (Recurrence, error) {
	schedule, start, ok := strings.Cut(value, " from ")
	if !ok {
		return Recurrence{}, errors.New("a schedule needs a start date")
	}
	return ParseRecurrence(schedule, start)
}

func (r Recurrence) check() error {
	if r.N < 1 || r.N > 24 || units[r.Unit] != r.Unit || !validDate(r.Start) {
		return errors.New("invalid schedule")
	}
	return nil
}

func (r Recurrence) String() string {
	return strconv.Itoa(r.N) + " " + r.Unit + " from " + r.Start
}

// Occurrence returns the k-th date, counted from the start. A month without
// the start's day uses its last day, and later months return to the start's day.
func (r Recurrence) Occurrence(k int) time.Time {
	start, _ := time.Parse(time.DateOnly, r.Start)
	switch r.Unit {
	case "days":
		return start.AddDate(0, 0, k*r.N)
	case "weeks":
		return start.AddDate(0, 0, 7*k*r.N)
	}
	months := k * r.N
	if r.Unit == "years" {
		months *= 12
	}
	first := time.Date(start.Year(), start.Month()+time.Month(months), 1, 0, 0, 0, 0, time.UTC)
	last := first.AddDate(0, 1, -1).Day()
	return first.AddDate(0, 0, min(start.Day(), last)-1)
}

// index returns the number of occurrences on or before day.
func (r Recurrence) index(day time.Time) int {
	start, _ := time.Parse(time.DateOnly, r.Start)
	if day.Before(start) {
		return 0
	}
	k := 0
	switch r.Unit {
	case "days":
		k = int(day.Sub(start).Hours()/24) / r.N
	case "weeks":
		k = int(day.Sub(start).Hours()/24) / (7 * r.N)
	default:
		months := (day.Year()-start.Year())*12 + int(day.Month()-start.Month())
		if r.Unit == "years" {
			months /= 12
		}
		k = max(months/r.N-1, 0)
	}
	for !r.Occurrence(k + 1).After(day) {
		k++
	}
	for k > 0 && r.Occurrence(k).After(day) {
		k--
	}
	return k + 1
}

// NextAfter lists the next n dates after a day.
func (r Recurrence) NextAfter(after time.Time, n int) []string {
	out := []string{}
	for k := r.index(after); len(out) < n; k++ {
		out = append(out, r.Occurrence(k).Format(time.DateOnly))
	}
	return out
}

// Close closes the occurrence due on due, as of today. Closed before its date,
// that occurrence is closed. Closed late, the latest date that went by is the
// one closed, the dates between are missed, and the next date follows today.
func (r Recurrence) Close(due string, today time.Time) Closing {
	day, err := time.Parse(time.DateOnly, due)
	today = time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.UTC)
	if err != nil {
		day = today
	}
	if day.After(today) {
		return Closing{Closed: due, Next: r.NextAfter(day, 1)[0]}
	}
	latest := r.Occurrence(r.index(today) - 1)
	closing := Closing{Closed: latest.Format(time.DateOnly), Next: r.NextAfter(today, 1)[0]}
	for k := r.index(day) - 1; k >= 0 && r.Occurrence(k).Before(latest); k++ {
		if !r.Occurrence(k).Before(day) {
			closing.Missed++
		}
	}
	return closing
}

// MaxLead is the longest lead time, in working days.
const MaxLead = 60

func leadString(n int) string {
	if n == 1 {
		return "1 working day"
	}
	return strconv.Itoa(n) + " working days"
}

func parseLead(value string) (int, error) {
	count, unit, _ := strings.Cut(value, " ")
	n, err := strconv.Atoi(count)
	if err != nil || n < 1 || n > MaxLead || unit != "working days" && !(n == 1 && unit == "working day") {
		return 0, fmt.Errorf("lead must look like 5 working days, from 1 to %d", MaxLead)
	}
	return n, nil
}
