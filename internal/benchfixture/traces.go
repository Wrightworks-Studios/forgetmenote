package benchfixture

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
)

// Trace is a T1 browser edit trace against F1. A harness opens the note,
// replays Events at their times and measures the editor. ResultSHA256 is the
// Markdown after applying the events as plain-text edits in order, which is
// what a Source mode editor produces; rich-text editors may serialize
// differently and are checked against the trace's own intent instead.
type Trace struct {
	Version       int          `json:"version"`
	Name          string       `json:"name"`
	Description   string       `json:"description"`
	Fixture       string       `json:"fixture"`
	FixtureSHA256 string       `json:"fixtureSha256"`
	Seed          uint64       `json:"seed"`
	DurationMS    int64        `json:"durationMs"`
	Clients       []string     `json:"clients,omitempty"`
	Events        []TraceEvent `json:"events"`
	ResultSHA256  string       `json:"resultSha256"`
	ResultBytes   int          `json:"resultBytes"`
}

// TraceEvent is one input. Ops:
//   - "place": put the caret at the start or end (At) of Find, which occurs
//     exactly once in the note and contains no Markdown syntax;
//   - "select": select from the start of Find to the start of To;
//   - "char": type one character (keydown, input, keyup);
//   - "key": press Key: Backspace, Delete, ArrowLeft or ArrowRight;
//   - "paste": paste Text as text/plain;
//   - "compose": IME composition update showing Text, not yet inserted;
//   - "commit": end the composition, inserting Text (empty Text cancels it);
//   - "patch": an agent replaces the unique Find with Text on the server.
type TraceEvent struct {
	T      int64  `json:"t"` // milliseconds after the trace starts
	Client string `json:"client,omitempty"`
	Op     string `json:"op"`
	Text   string `json:"text,omitempty"`
	Key    string `json:"key,omitempty"`
	Find   string `json:"find,omitempty"`
	To     string `json:"to,omitempty"`
	At     string `json:"at,omitempty"`
}

// Durations of the T1 traces.
const (
	TraceTypingMS     = 10 * 60 * 1000
	TraceConcurrentMS = 2 * 60 * 1000
	TracePasteBytes   = 50 << 10
)

// Traces generates T1 for novel (F1, or any F1-shaped note): ten minutes of
// typing at the start, middle and end; a 50 KiB paste; a chapter delete; IME
// compositions; and three concurrent clients with an agent patch every 5 s.
func Traces(novel []byte, seed uint64) ([]Trace, error) {
	doc, err := parseNovel(novel)
	if err != nil {
		return nil, err
	}
	g := &traceGen{novel: string(novel), doc: doc, seed: seed}
	starts := doc.sections()
	paragraphs := func(from, to int) []int {
		var out []int
		for i := from; i < to && i < len(doc.blocks); i++ {
			if isParagraph(doc.blocks[i]) {
				out = append(out, i)
			}
		}
		return out
	}
	first, last := paragraphs(starts[0], len(doc.blocks)), paragraphs(0, len(doc.blocks))
	middle := paragraphs(len(doc.blocks)/2, len(doc.blocks))
	if len(first) < 8 || len(middle) < 32 {
		return nil, errors.New("benchfixture: traces need a note with at least 64 paragraphs")
	}
	end := slices.Clone(last[len(last)-3:])
	slices.Reverse(end)
	traces := []Trace{
		g.typing("typing-start", "Ten minutes of typing at the end of the first paragraph of the first chapter.", first[:3]),
		g.typing("typing-middle", "Ten minutes of typing at the end of a paragraph in the middle of the note.", middle[:3]),
		g.typing("typing-end", "Ten minutes of typing at the end of the last paragraph.", end),
		g.paste(middle[3:]),
		g.chapterDelete(starts),
		g.ime(middle[6:]),
		g.concurrent(first, middle, last),
	}
	for _, t := range traces {
		if t.Name == "" {
			return nil, fmt.Errorf("benchfixture: no unique anchor for a trace in this note")
		}
	}
	return traces, nil
}

type traceGen struct {
	novel string
	doc   novelDoc
	seed  uint64
}

// safeSnippet reports whether s is plain text that renders as itself.
func safeSnippet(s string) bool { return !strings.ContainsAny(s, "_*`[]<>#\\|~&!") }

func unique(doc, s string) bool {
	plain := strings.NewReplacer("_", "", "*", "").Replace(doc)
	return strings.Count(doc, s) == 1 && strings.Count(plain, s) == 1
}

// paragraphEnd returns the shortest unique plain suffix of a paragraph of
// at least 24 bytes, trying candidates in order.
func (g *traceGen) paragraphEnd(doc string, candidates []int) (int, string) {
	for _, i := range candidates {
		p := g.doc.blocks[i]
		for n := 24; n <= len(p) && n <= 160; n++ {
			s := p[len(p)-n:]
			if !utf8.RuneStart(s[0]) || s[0] == ' ' {
				continue
			}
			if !safeSnippet(s) {
				break
			}
			if unique(doc, s) {
				return i, s
			}
		}
	}
	return -1, ""
}

type caret struct{ anchor, head int }

// textModel applies trace events to Markdown as plain-text edits, moving
// every other client's caret as a collaborative editor would.
type textModel struct {
	doc    []byte
	carets map[string]*caret
}

func newTextModel(doc string) *textModel {
	return &textModel{doc: []byte(doc), carets: map[string]*caret{}}
}

func (m *textModel) index(s string) int {
	if !unique(string(m.doc), s) {
		panic(fmt.Sprintf("benchfixture: trace anchor %q is not unique", s))
	}
	return strings.Index(string(m.doc), s)
}

func (m *textModel) replace(from, to int, text string) {
	// In place: a keystroke moves the tail instead of copying the whole note.
	tail, delta := len(m.doc)-to, len(text)-(to-from)
	if delta > 0 {
		m.doc = append(m.doc, make([]byte, delta)...)
	}
	copy(m.doc[from+len(text):], m.doc[to:to+tail])
	copy(m.doc[from:], text)
	m.doc = m.doc[:from+len(text)+tail]
	shift := func(p int) int {
		switch {
		case p >= to:
			return p + len(text) - (to - from)
		case p > from:
			return from
		}
		return p
	}
	for _, c := range m.carets {
		c.anchor, c.head = shift(c.anchor), shift(c.head)
	}
}

func (m *textModel) apply(e TraceEvent) {
	c := m.carets[e.Client]
	if c == nil {
		c = &caret{}
		m.carets[e.Client] = c
	}
	lo, hi := min(c.anchor, c.head), max(c.anchor, c.head)
	insert := func(text string) {
		m.replace(lo, hi, text)
		c.anchor, c.head = lo+len(text), lo+len(text)
	}
	switch e.Op {
	case "place":
		p := m.index(e.Find)
		if e.At == "end" {
			p += len(e.Find)
		}
		c.anchor, c.head = p, p
	case "select":
		c.anchor, c.head = m.index(e.Find), m.index(e.To)
	case "char", "paste", "commit":
		if e.Text != "" {
			insert(e.Text)
		}
	case "patch":
		p := m.index(e.Find)
		m.replace(p, p+len(e.Find), e.Text)
	case "key":
		switch {
		case e.Key == "ArrowLeft" && lo == hi:
			_, n := utf8.DecodeLastRune(m.doc[:lo])
			c.anchor, c.head = lo-n, lo-n
		case e.Key == "ArrowRight" && lo == hi:
			_, n := utf8.DecodeRune(m.doc[hi:])
			c.anchor, c.head = hi+n, hi+n
		case e.Key == "ArrowLeft":
			c.anchor, c.head = lo, lo
		case e.Key == "ArrowRight":
			c.anchor, c.head = hi, hi
		case lo != hi:
			insert("")
		case e.Key == "Backspace":
			_, n := utf8.DecodeLastRune(m.doc[:lo])
			lo -= n
			insert("")
		case e.Key == "Delete":
			_, n := utf8.DecodeRune(m.doc[hi:])
			hi += n
			insert("")
		}
	}
}

// finish records the result; DurationMS is at least duration, the time the
// trace covers even when its last input comes earlier.
func (g *traceGen) finish(t Trace, m *textModel, duration int64) Trace {
	t.Version, t.Fixture, t.FixtureSHA256, t.Seed, t.DurationMS = 1, "f1", sha256Hex([]byte(g.novel)), g.seed, duration
	for _, e := range t.Events {
		t.DurationMS = max(t.DurationMS, e.T)
	}
	t.ResultSHA256, t.ResultBytes = sha256Hex(m.doc), len(m.doc)
	return t
}

// typist emits realistic typing: 55–70 words a minute with bursts, pauses at
// sentence ends, occasional long thinking pauses, typos corrected with
// Backspace and a few caret round trips.
type typist struct {
	r      *rng
	m      *textModel
	client string
	t      int64
	events *[]TraceEvent
}

var neighbours = map[byte]string{'a': "qsz", 'e': "wrd", 'i': "uok", 'o': "ipl", 'n': "bm", 's': "adw", 't': "rgy", 'r': "etf", 'h': "gjn", 'l': "kop"}

func (ty *typist) emit(e TraceEvent) {
	e.T, e.Client = ty.t, ty.client
	ty.m.apply(e)
	*ty.events = append(*ty.events, e)
}

func (ty *typist) key(key string, delay int) {
	ty.t += int64(delay)
	ty.emit(TraceEvent{Op: "key", Key: key})
}

func (ty *typist) char(c string, delay int) {
	ty.t += int64(delay)
	ty.emit(TraceEvent{Op: "char", Text: c})
}

// word types w followed by a space or the sentence's end.
func (ty *typist) word(w string) {
	r := ty.r
	typo := len(w) > 3 && r.permille(35)
	for i := 0; i < len(w); i++ {
		delay := r.between(70, 220)
		if r.permille(40) {
			delay += r.between(200, 700)
		}
		if typo && i == len(w)/2 {
			if wrong, ok := neighbours[w[i]]; ok {
				extra := min(r.between(0, 2), len(w)-i-1)
				ty.char(string(wrong[r.intn(len(wrong))]), delay)
				for j := 1; j <= extra; j++ {
					ty.char(string(w[i+j]), r.between(70, 200))
				}
				ty.t += int64(r.between(250, 600)) // noticing
				for range extra + 1 {
					ty.key("Backspace", r.between(80, 160))
				}
				delay = r.between(90, 200)
				typo = false
			}
		}
		ty.char(string(w[i]), delay)
	}
	if r.permille(10) {
		steps := r.between(1, 6)
		for range steps {
			ty.key("ArrowLeft", r.between(120, 300))
		}
		for range steps {
			ty.key("ArrowRight", r.between(100, 250))
		}
	}
}

// typeFor types sentences until the trace clock passes until.
func (ty *typist) typeFor(until int64) {
	words := 0
	for ty.t < until {
		sent := strings.Fields(sentence(ty.r))
		for i, w := range sent {
			if ty.t >= until {
				return
			}
			ty.char(" ", ty.r.between(90, 260))
			ty.word(w)
			words++
			if words%ty.r.between(40, 80) == 0 {
				ty.t += int64(ty.r.between(4000, 15000)) // thinking
			}
			if i == len(sent)-1 {
				ty.t += int64(ty.r.between(500, 2500))
			}
		}
	}
}

func (g *traceGen) typing(name, description string, candidates []int) Trace {
	_, anchor := g.paragraphEnd(g.novel, candidates)
	if anchor == "" {
		return Trace{}
	}
	t := Trace{Name: name, Description: description}
	m := newTextModel(g.novel)
	ty := &typist{r: newRNG(g.seed, "t1/"+name), m: m, events: &t.Events}
	ty.emit(TraceEvent{Op: "place", Find: anchor, At: "end"})
	ty.t = 1000
	ty.typeFor(TraceTypingMS)
	return g.finish(t, m, TraceTypingMS)
}

func (g *traceGen) paste(candidates []int) Trace {
	_, anchor := g.paragraphEnd(g.novel, candidates)
	if anchor == "" {
		return Trace{}
	}
	r := newRNG(g.seed, "t1/paste")
	var text strings.Builder
	for text.Len() < TracePasteBytes {
		if text.Len() > 0 {
			text.WriteString("\n\n")
		}
		text.WriteString(prose(r, r.between(3, 8)))
	}
	pasted := strings.TrimRight(text.String()[:TracePasteBytes], " \n")
	t := Trace{Name: "paste-50k", Description: "Paste 50 KiB of prose paragraphs at the end of a paragraph in the middle of the note, then type for 30 seconds."}
	m := newTextModel(g.novel)
	ty := &typist{r: r, m: m, events: &t.Events}
	ty.emit(TraceEvent{Op: "place", Find: anchor, At: "end"})
	ty.t = 500
	ty.emit(TraceEvent{Op: "paste", Text: " " + pasted})
	ty.t += 2000
	ty.typeFor(ty.t + 30000)
	return g.finish(t, m, ty.t)
}

// chapterDelete selects one whole chapter in the middle of the note, from its
// heading to the next heading, and deletes it.
func (g *traceGen) chapterDelete(starts []int) Trace {
	for k := len(starts) / 2; k+1 < len(starts); k++ {
		from, to := strings.TrimPrefix(g.doc.blocks[starts[k]], "## "), strings.TrimPrefix(g.doc.blocks[starts[k+1]], "## ")
		if !safeSnippet(from) || !safeSnippet(to) || !unique(g.novel, from) || !unique(g.novel, to) {
			continue
		}
		var next []int
		for i := starts[k+1] + 1; i < len(g.doc.blocks) && isParagraph(g.doc.blocks[i]); i++ {
			next = append(next, i)
		}
		_, anchor := g.paragraphEnd(g.novel, next)
		if anchor == "" {
			continue
		}
		t := Trace{Name: "chapter-delete", Description: "Select one chapter in the middle of the note from its heading to the next heading and delete it, then type for 10 seconds in the following chapter."}
		m := newTextModel(g.novel)
		ty := &typist{r: newRNG(g.seed, "t1/chapter"), m: m, events: &t.Events}
		ty.emit(TraceEvent{Op: "select", Find: from, To: to})
		ty.key("Backspace", 800)
		ty.t += 2000
		ty.emit(TraceEvent{Op: "place", Find: anchor, At: "end"})
		ty.typeFor(ty.t + 10000)
		return g.finish(t, m, ty.t)
	}
	return Trace{}
}

var compositions = []struct {
	steps  []string
	commit string
}{
	{[]string{"n", "に", "にh", "にほ", "にほn", "にほん", "にほんg", "にほんご"}, "日本語"},
	{[]string{"k", "か", "かn", "かん", "かんj", "かんじ"}, "漢字"},
	{[]string{"z", "zh", "zho", "zhon", "zhong", "zhong'w", "zhong'we", "zhong'wen"}, "中文"},
	{[]string{"n", "ni", "ni'h", "ni'ha", "ni'hao"}, "你好"},
	{[]string{"ㅎ", "하", "한", "한ㄱ", "한구", "한국", "한국ㅇ", "한국어"}, "한국어"},
	{[]string{"s", "さ", "さk", "さく"}, ""}, // cancelled with Escape
}

func (g *traceGen) ime(candidates []int) Trace {
	_, anchor := g.paragraphEnd(g.novel, candidates)
	if anchor == "" {
		return Trace{}
	}
	t := Trace{Name: "ime", Description: "Japanese, Chinese and Korean IME compositions, one cancelled, typed at the end of a paragraph for 90 seconds."}
	m := newTextModel(g.novel)
	ty := &typist{r: newRNG(g.seed, "t1/ime"), m: m, events: &t.Events}
	ty.emit(TraceEvent{Op: "place", Find: anchor, At: "end"})
	ty.t = 1000
	for i := 0; ty.t < 90000; i++ {
		c := compositions[i%len(compositions)]
		ty.char(" ", ty.r.between(150, 300))
		for _, step := range c.steps {
			ty.t += int64(ty.r.between(120, 300))
			ty.emit(TraceEvent{Op: "compose", Text: step})
		}
		ty.t += int64(ty.r.between(400, 900))
		ty.emit(TraceEvent{Op: "commit", Text: c.commit})
	}
	return g.finish(t, m, ty.t)
}

// concurrent has three clients typing in three paragraphs while an agent
// rewrites a sentence elsewhere every five seconds.
func (g *traceGen) concurrent(first, middle, last []int) Trace {
	t := Trace{Name: "concurrent", Description: "Three clients type at the start, middle and end of the note for two minutes while an agent patches one sentence every five seconds.", Clients: []string{"c1", "c2", "c3", "agent"}}
	m := newTextModel(g.novel)
	var streams [][]TraceEvent
	used := map[int]bool{}
	for i, candidates := range [][]int{first[3:6], middle[9:12], last[len(last)-6 : len(last)-3]} {
		at, anchor := g.paragraphEnd(g.novel, candidates)
		if anchor == "" {
			return Trace{}
		}
		used[at] = true
		var events []TraceEvent
		ty := &typist{r: newRNG(g.seed, fmt.Sprintf("t1/concurrent/%d", i)), m: newTextModel(g.novel), client: t.Clients[i], events: &events}
		ty.emit(TraceEvent{Op: "place", Find: anchor, At: "end"})
		ty.t = int64(500 + 300*i)
		ty.typeFor(TraceConcurrentMS)
		streams = append(streams, events)
	}
	// The agent rewrites sentences in paragraphs no client types in.
	r := newRNG(g.seed, "t1/concurrent/agent")
	var patches []TraceEvent
	for at := int64(5000); at <= TraceConcurrentMS; at += 5000 {
		for range 1000 {
			i := pick(r, middle[20:])
			if used[i] || used[i-1] || used[i+1] {
				continue
			}
			sentences := splitSentences(g.doc.blocks[i])
			s := pick(r, sentences)
			if len(s) < 40 || !safeSnippet(s) || !unique(g.novel, s) {
				continue
			}
			used[i] = true
			patches = append(patches, TraceEvent{T: at, Client: "agent", Op: "patch", Find: s, Text: sentence(r)})
			break
		}
	}
	streams = append(streams, patches)
	for _, s := range streams {
		t.Events = append(t.Events, s...)
	}
	slices.SortStableFunc(t.Events, func(a, b TraceEvent) int { return int(a.T - b.T) })
	for _, e := range t.Events {
		m.apply(e)
	}
	return g.finish(t, m, TraceConcurrentMS)
}
