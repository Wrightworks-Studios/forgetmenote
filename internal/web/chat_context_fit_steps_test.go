package web

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"forgetmenote/internal/chat"
)

// contentTokens estimates a request as its messages' content bytes, so step
// tests can reason about exact sizes.
func contentTokens(r chat.Request) (int, error) {
	n := 0
	for _, m := range r.Messages {
		n += len(m.Content)
	}
	return n, nil
}

func fitStepSource(id, path, text string) chat.Citation {
	return chat.Citation{ID: id, Kind: "committed", Path: path, URL: "/" + path + "?fmn-rev=rev", Revision: "rev", Hash: "hash-" + path, PassageHash: "passage-" + id, Text: text}
}

func fitStepEnvelope(t *testing.T, kind string, sources ...chat.Citation) string {
	t.Helper()
	raw, err := json.Marshal(chatSourceContext{Kind: kind, Sources: sources})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func fitStepTask(t *testing.T, sources ...chat.Citation) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"status": "Selected", "sources": sources})
	if err != nil {
		t.Fatal(err)
	}
	return "TASK\n\n<notebook_guidance>\n\"Guidance.\"\n</notebook_guidance>\n<untrusted_notebook_context>\n" + string(raw) + "\n</untrusted_notebook_context>"
}

func fitStepCall(id, name string) chat.Message {
	return chat.Message{Role: "assistant", ToolCalls: []chat.ToolCall{{ID: id, Name: name, Arguments: json.RawMessage(`{}`)}}}
}

func TestChatFitProjectionFindsOptionalSources(t *testing.T) {
	messages := []chat.Message{
		{Role: "user", Content: "Earlier question"},
		{Role: "assistant", Content: "Earlier answer"},
		{Role: "user", Content: fitStepTask(t, fitStepSource("task", "notes/task.md", "task text"))},
		fitStepCall("read", "read_note"),
		{Role: "tool", ToolCallID: "read", Content: fitStepEnvelope(t, "note_read", fitStepSource("read", "notes/read.md", "read text"))},
		fitStepCall("search", "search_passages"),
		{Role: "tool", ToolCallID: "search", Content: fitStepEnvelope(t, "passage_search")},
		fitStepCall("doc", "read_document"),
		{Role: "tool", ToolCallID: "doc", Content: fitStepEnvelope(t, "note_read", fitStepSource("doc", "notes/doc.md", "doc text"))},
	}
	request := chat.Request{Messages: messages, MaxOutputTokens: 100}
	f := projectChatRequest(chat.Run{}, request, 1000, contentTokens)
	if f.hard != 900 || f.soft != 388 || f.task.index != 2 || f.history != 2 {
		t.Fatalf("limits %d/%d, task %d, history %d", f.hard, f.soft, f.task.index, f.history)
	}
	// Only a read or search result with sources is optional evidence.
	if len(f.envelopes) != 1 || f.envelopes[4] == nil || f.envelopes[4].Sources[0].ID != "read" || f.calls["doc"] != "read_document" {
		t.Fatalf("envelopes %v calls %v", f.envelopes, f.calls)
	}
	// The model never sees passage checksums.
	if len(f.task.sources) != 1 || f.task.sources[0].PassageHash != "" || f.envelopes[4].Sources[0].PassageHash != "" || strings.Contains(f.request.Messages[2].Content, "passage-task") {
		t.Fatalf("checksums reached the projection: %+v %s", f.task.sources, f.request.Messages[2].Content)
	}
	if messages[2].Content == f.request.Messages[2].Content || !strings.Contains(messages[2].Content, "passage-task") {
		t.Fatal("projection changed the caller's messages")
	}
	// A review's task envelope is mandatory; its dialogue still ends at the task.
	review := projectChatRequest(chat.Run{Options: chat.Options{Review: true}}, request, 1000, contentTokens)
	if review.task.index != -1 || review.history != 2 || review.request.Messages[2].Content != messages[2].Content {
		t.Fatalf("review task %d history %d", review.task.index, review.history)
	}
}

func TestChatFitProjectionRestoresLegacyResultFromItsReceipt(t *testing.T) {
	result, _ := json.Marshal(noteOutput{Path: "notes/read.md", Content: "Original note text", Revision: "rev"})
	op := chat.Operation{Name: "read_note", ToolCallID: "read", State: "completed", Result: result, Citations: []chat.Citation{fitStepSource("read", "notes/read.md", "")}}
	legacy := oldChatSourceRepresentation(op)
	request := chat.Request{Messages: []chat.Message{{Role: "user", Content: "TASK"}, fitStepCall("read", "read_note"), {Role: "tool", ToolCallID: "read", Content: legacy}}}
	f := projectChatRequest(chat.Run{Operations: []chat.Operation{op}}, request, 1000, contentTokens)
	if f.envelopes[2] == nil || f.envelopes[2].Sources[0].Text != "Original note text" || f.request.Messages[2].Content == legacy {
		t.Fatalf("legacy read was not restored: %s", f.request.Messages[2].Content)
	}
	// Without its receipt a legacy result stays as it is.
	f = projectChatRequest(chat.Run{}, request, 1000, contentTokens)
	if len(f.envelopes) != 0 || f.request.Messages[2].Content != legacy {
		t.Fatal("legacy result guessed without a receipt")
	}
}

func TestChatFitWithdrawsSourcesInMessageOrderThenTheTasks(t *testing.T) {
	request := chat.Request{Messages: []chat.Message{
		{Role: "user", Content: fitStepTask(t, fitStepSource("t1", "notes/t.md", "t1"), fitStepSource("t2", "notes/t.md", "t2"))},
		fitStepCall("search", "search_passages"),
		{Role: "tool", ToolCallID: "search", Content: fitStepEnvelope(t, "passage_search", fitStepSource("s1", "notes/s.md", "s1"))},
		fitStepCall("read", "read_note"),
		{Role: "tool", ToolCallID: "read", Content: fitStepEnvelope(t, "note_read", fitStepSource("r1", "notes/r.md", "r1"))},
	}}
	f := projectChatRequest(chat.Run{}, request, 1000, contentTokens)
	candidates := f.withdrawSources()
	got := []string{}
	for _, c := range candidates {
		got = append(got, fmt.Sprintf("%s@%d#%d:%v", c.source.ID, c.message, c.rank, c.read))
	}
	if want := []string{"s1@2#0:false", "r1@4#0:true", "t1@0#0:false", "t2@0#1:false"}; !slices.Equal(got, want) {
		t.Fatalf("candidates %v, want %v", got, want)
	}
	if f.stats.SourcesBefore != 4 || len(f.task.sources) != 0 || len(f.envelopes[2].Sources) != 0 || len(f.envelopes[4].Sources) != 0 {
		t.Fatalf("sources were not withdrawn: %+v", f.stats)
	}
	f.render()
	var partial chatSourceContext
	if json.Unmarshal([]byte(f.request.Messages[4].Content), &partial) != nil || !partial.Partial || !strings.Contains(partial.Message, "omission does not mean absence") {
		t.Fatalf("withdrawn result does not say it is partial: %s", f.request.Messages[4].Content)
	}
}

func TestChatFitDropsOlderDialogueUntilAToolCycle(t *testing.T) {
	old := strings.Repeat("o", 100)
	request := chat.Request{Messages: []chat.Message{
		{Role: "user", Content: old}, {Role: "assistant", Content: old},
		{Role: "user", Content: old}, fitStepCall("old", "list_documents"), {Role: "tool", ToolCallID: "old", Content: "{}"}, {Role: "assistant", Content: old},
		{Role: "user", Content: fitStepTask(t)},
		fitStepCall("read", "read_note"),
		{Role: "tool", ToolCallID: "read", Content: fitStepEnvelope(t, "note_read", fitStepSource("r1", "notes/r.md", "r1"))},
	}}
	f := projectChatRequest(chat.Run{}, request, 100000, contentTokens)
	candidates := f.withdrawSources()
	if err := f.update(); err != nil {
		t.Fatal(err)
	}
	if err := f.dropHistory(0, candidates); err != nil {
		t.Fatal(err)
	}
	// The first exchange goes; the one holding a tool cycle stays whole.
	if f.stats.DroppedHistory != 2 || len(f.request.Messages) != 7 || f.task.index != 4 || f.history != 4 || f.envelopes[6] == nil || candidates[0].message != 6 {
		t.Fatalf("dropped %d, %d messages, task %d, history %d, envelopes %v, candidate at %d", f.stats.DroppedHistory, len(f.request.Messages), f.task.index, f.history, f.envelopes, candidates[0].message)
	}
	if size, _ := contentTokens(f.request); f.stats.AfterTokens != size {
		t.Fatal("estimate not refreshed after dropping")
	}
	// A request within its target keeps its dialogue.
	f = projectChatRequest(chat.Run{}, request, 100000, contentTokens)
	_ = f.update()
	if err := f.dropHistory(f.stats.AfterTokens, nil); err != nil || f.stats.DroppedHistory != 0 {
		t.Fatalf("fitting request dropped %d", f.stats.DroppedHistory)
	}
}

func TestChatFitOrdersMatchesReadsRanksAndNotes(t *testing.T) {
	candidate := func(id, path string, message, rank int, read bool) chatFitCandidate {
		return chatFitCandidate{source: fitStepSource(id, path, id), message: message, rank: rank, read: read}
	}
	ordered := orderChatFitCandidates([]chatFitCandidate{
		candidate("passage-a0", "notes/a.md", 2, 0, false),
		candidate("passage-a1", "notes/a.md", 2, 1, false),
		candidate("passage-b0", "notes/b.md", 4, 0, false),
		candidate("read-c", "notes/c.md", 6, 0, true),
		candidate("passage-c1", "notes/c.md", 2, 2, false),
		candidate("read-d", "notes/d.md", 8, 0, true),
	})
	got := []string{}
	for _, c := range ordered {
		got = append(got, c.source.ID)
	}
	// A passage of a read note first, then the reads, then ranked passages,
	// later messages first within a rank, one per note before any second.
	want := []string{"passage-c1", "read-d", "passage-b0", "passage-a0", "read-c", "passage-a1"}
	if !slices.Equal(got, want) {
		t.Fatalf("order %v, want %v", got, want)
	}
}

func TestChatFitTriesAnExcerptAndKeepsItOnlyIfItFits(t *testing.T) {
	request := chat.Request{Messages: []chat.Message{{Role: "user", Content: fitStepTask(t)}}}
	f := projectChatRequest(chat.Run{}, request, 100000, contentTokens)
	_ = f.update()
	base, baseText := f.stats.AfterTokens, f.request.Messages[0].Content
	entry := chatFitCandidate{source: fitStepSource("p", "notes/p.md", strings.Repeat("p", 500)), message: 0}
	if kept, err := f.trySource(entry, 500, base+100); err != nil || kept || f.request.Messages[0].Content != baseText || f.stats.AfterTokens != base {
		t.Fatalf("excerpt over target kept=%v err=%v", kept, err)
	}
	if kept, err := f.trySource(entry, 200, base+1000); err != nil || !kept || len(f.task.sources) != 1 || !strings.HasSuffix(f.task.sources[0].Text, "full text remains in the conversation.]") {
		t.Fatalf("bounded excerpt kept=%v err=%v sources=%v", kept, err, f.task.sources)
	}
	// A shorter excerpt of retained text is already covered.
	if kept, err := f.trySource(entry, 100, base+100000); err != nil || kept || len(f.task.sources) != 1 {
		t.Fatalf("covered excerpt kept=%v err=%v", kept, err)
	}
}

func TestChatFitRetainsAtMostOneSourcePerKilotoken(t *testing.T) {
	sources := []chat.Citation{}
	for i := range 6 {
		sources = append(sources, fitStepSource(fmt.Sprint(i), fmt.Sprintf("notes/%d.md", i), "short"))
	}
	f := projectChatRequest(chat.Run{}, chat.Request{Messages: []chat.Message{{Role: "user", Content: fitStepTask(t, sources...)}}}, 4096, contentTokens)
	ordered := orderChatFitCandidates(f.withdrawSources())
	_ = f.update()
	if err := f.retainSources(ordered); err != nil {
		t.Fatal(err)
	}
	if f.stats.SourcesAfter != 4 || len(f.task.sources) != 4 {
		t.Fatalf("retained %d of 6 in a 4096-token context", f.stats.SourcesAfter)
	}
}

func TestChatFitRetainsOneExcerptWithinTheHardLimit(t *testing.T) {
	read := fitStepSource("read", "notes/read.md", strings.Repeat("r", 5000))
	request := func(task int) chat.Request {
		return chat.Request{MaxOutputTokens: 1000, Messages: []chat.Message{
			{Role: "user", Content: strings.Repeat("t", task)},
			fitStepCall("read", "read_note"),
			{Role: "tool", ToolCallID: "read", Content: fitStepEnvelope(t, "note_read", read)},
		}}
	}
	withdrawn := func(task int) (*chatFit, []chatFitCandidate) {
		f := projectChatRequest(chat.Run{}, request(task), 10000, contentTokens)
		ordered := orderChatFitCandidates(f.withdrawSources())
		f.render()
		_ = f.update()
		return f, ordered
	}
	// Hard 9000 and soft 6500 tokens. A mandatory part 50 tokens under the
	// soft target leaves no room for a read to continue, but one excerpt still
	// fits beside the output allowance.
	empty, _ := withdrawn(0)
	f, ordered := withdrawn(empty.soft - 50 - empty.stats.AfterTokens)
	if f.hard != 9000 || f.soft != 6500 || f.stats.AfterTokens != f.soft-50 {
		t.Fatalf("limits %d/%d at %d", f.hard, f.soft, f.stats.AfterTokens)
	}
	if err := f.retainSources(ordered); err != nil || f.stats.SourcesAfter != 0 {
		t.Fatalf("read retained %d within the soft target: %v", f.stats.SourcesAfter, err)
	}
	if err := f.retainOneSource(ordered); err != nil {
		t.Fatal(err)
	}
	if f.stats.SourcesAfter != 1 || f.stats.AfterTokens > f.hard || len(chatExcerptOriginal(f.envelopes[2].Sources[0].Text)) != 1000 {
		t.Fatalf("kept %d at %d of %d", f.stats.SourcesAfter, f.stats.AfterTokens, f.hard)
	}
}

func TestChatFitOmitsTheLargestResultKeepingItsReceipt(t *testing.T) {
	data, _ := json.Marshal(map[string]any{"receipt": map[string]any{"id": "receipt-1", "connection": "synthetic", "operation": "list"}, "result": strings.Repeat("row ", 1000)})
	request := chat.Request{Messages: []chat.Message{
		{Role: "user", Content: "TASK"},
		fitStepCall("data", "call_data_operation"),
		{Role: "tool", ToolCallID: "data", Content: string(data)},
		fitStepCall("doc", "read_document"),
		{Role: "tool", ToolCallID: "doc", Content: `{"content":"` + strings.Repeat("d", 2000) + `"}`},
		fitStepCall("list", "list_documents"),
		{Role: "tool", ToolCallID: "list", Content: strings.Repeat("l", 1000)},
	}}
	f := projectChatRequest(chat.Run{}, request, 4000, contentTokens)
	_ = f.update()
	if err := f.omitLargeResults(); err != nil {
		t.Fatal(err)
	}
	// Only the data result needed to go; other kinds of result are never omitted.
	var omitted struct {
		ContextOmitted bool `json:"contextOmitted"`
		Receipt        struct {
			ID string `json:"id"`
		} `json:"receipt"`
	}
	if json.Unmarshal([]byte(f.request.Messages[2].Content), &omitted) != nil || !omitted.ContextOmitted || omitted.Receipt.ID != "receipt-1" || !f.stats.Changed {
		t.Fatalf("data result %s", f.request.Messages[2].Content)
	}
	if f.request.Messages[4].Content != request.Messages[4].Content || f.request.Messages[6].Content != request.Messages[6].Content || f.stats.AfterTokens > f.hard {
		t.Fatalf("omitted more than needed: %d of %d", f.stats.AfterTokens, f.hard)
	}
	// When nothing omittable remains, the request is left to fail the limit.
	f = projectChatRequest(chat.Run{}, chat.Request{Messages: request.Messages[5:]}, 500, contentTokens)
	_ = f.update()
	if err := f.omitLargeResults(); err != nil || f.stats.Changed || f.stats.AfterTokens <= f.hard {
		t.Fatalf("unomittable request changed: %+v", f.stats)
	}
}
