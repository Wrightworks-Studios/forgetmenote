package benchfixture

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func traceByName(t *testing.T, traces []Trace, name string) Trace {
	t.Helper()
	i := slices.IndexFunc(traces, func(tr Trace) bool { return tr.Name == name })
	if i < 0 {
		t.Fatalf("no %s trace", name)
	}
	return traces[i]
}

// replayTrace decodes a trace as a harness would and replays it as text.
func replayTrace(t *testing.T, novel []byte, tr Trace) string {
	t.Helper()
	raw, err := json.Marshal(tr)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Trace
	if err = json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	m := newTextModel(string(novel))
	last := int64(0)
	for _, e := range decoded.Events {
		if e.T < last {
			t.Fatalf("%s: event at %d after %d", tr.Name, e.T, last)
		}
		last = e.T
		m.apply(e)
	}
	if sha256Hex(m.doc) != tr.ResultSHA256 || len(m.doc) != tr.ResultBytes || tr.FixtureSHA256 != sha256Hex(novel) {
		t.Fatalf("%s: replay does not reproduce the recorded result", tr.Name)
	}
	return string(m.doc)
}

func checkTraces(t *testing.T, novel []byte, traces []Trace) {
	names := []string{"typing-start", "typing-middle", "typing-end", "paste-50k", "chapter-delete", "ime", "concurrent"}
	for _, name := range names {
		traceByName(t, traces, name)
	}
	doc := string(novel)
	for _, name := range names[:3] {
		tr := traceByName(t, traces, name)
		result := replayTrace(t, novel, tr)
		keys := 0
		for _, e := range tr.Events {
			if e.Op == "char" || e.Op == "key" {
				keys++
			}
		}
		if last := tr.Events[len(tr.Events)-1].T; tr.DurationMS != max(last, TraceTypingMS) || last < TraceTypingMS-20000 || last > TraceTypingMS+5000 || keys < 1500 || keys > 5000 {
			t.Fatalf("%s: %d ms, last input at %d, %d keystrokes", name, tr.DurationMS, last, keys)
		}
		// Typing only inserts text where the caret was placed.
		place := tr.Events[0]
		at := strings.Index(doc, place.Find) + len(place.Find)
		typed := len(result) - len(doc)
		if place.Op != "place" || typed < 1000 || result[:at] != doc[:at] || result[at+typed:] != doc[at:] {
			t.Fatalf("%s: typed %d bytes elsewhere than after %q", name, typed, place.Find)
		}
	}
	paste := traceByName(t, traces, "paste-50k")
	pasted := paste.Events[1]
	if pasted.Op != "paste" || len(pasted.Text) < TracePasteBytes-8 || len(pasted.Text) > TracePasteBytes+1 || !strings.Contains(replayTrace(t, novel, paste), pasted.Text) {
		t.Fatalf("paste of %d bytes", len(pasted.Text))
	}
	del := traceByName(t, traces, "chapter-delete")
	sel := del.Events[0]
	result := replayTrace(t, novel, del)
	if sel.Op != "select" || strings.Contains(result, "## "+sel.Find) || !strings.Contains(result, "## "+sel.To) || len(result) > len(doc)-500 {
		t.Fatalf("chapter delete left %q or lost %q", sel.Find, sel.To)
	}
	ime := traceByName(t, traces, "ime")
	result = replayTrace(t, novel, ime)
	composes := 0
	for _, e := range ime.Events {
		if e.Op == "compose" {
			composes++
		}
	}
	if composes < 50 || !strings.Contains(result, "日本語") || !strings.Contains(result, "한국어") || strings.Contains(result, "さく") {
		t.Fatalf("IME trace: %d compositions", composes)
	}
	concurrent := traceByName(t, traces, "concurrent")
	result = replayTrace(t, novel, concurrent)
	clients := map[string]int{}
	for _, e := range concurrent.Events {
		clients[e.Client]++
		if e.Op == "patch" && (!unique(doc, e.Find) || !strings.Contains(result, e.Text)) {
			t.Fatalf("agent patch %q", e.Find)
		}
	}
	if len(clients) != 4 || clients["agent"] != TraceConcurrentMS/5000 || clients["c1"] < 300 {
		t.Fatalf("concurrent events by client %v", clients)
	}
}

func TestTracesReplayToRecordedResults(t *testing.T) {
	novel := testNovel(40)
	traces, err := Traces(novel, 5)
	if err != nil {
		t.Fatal(err)
	}
	checkTraces(t, novel, traces)
	again, _ := Traces(novel, 5)
	other, _ := Traces(novel, 6)
	a, _ := json.Marshal(traces)
	b, _ := json.Marshal(again)
	c, _ := json.Marshal(other)
	if string(a) != string(b) || string(a) == string(c) {
		t.Fatal("traces are not a deterministic function of the seed")
	}
}

func TestTracesOnF1(t *testing.T) {
	novel := NovelOrSkip(t)
	traces, err := Traces(novel, 1)
	if err != nil {
		t.Fatal(err)
	}
	checkTraces(t, novel, traces)
	if start := traceByName(t, traces, "typing-start").Events[0].Find; !strings.Contains(string(novel[:20000]), start) {
		t.Fatalf("typing-start anchor %q is not in chapter 1", start)
	}
}
