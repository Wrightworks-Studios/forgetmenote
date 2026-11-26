package chatrun

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"forgetmenote/internal/chat"
	"forgetmenote/internal/webcore"
)

var updateChatFit = flag.Bool("update-chat-fit", false, "rewrite testdata/chat-fit-characterization.json")

// chatFitRand is a fixed generator, so recorded cases never depend on the
// standard library's random streams.
type chatFitRand uint64

func (r *chatFitRand) next() uint64 {
	*r += 0x9e3779b97f4a7c15
	z := uint64(*r)
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}
func (r *chatFitRand) intn(n int) int { return int(r.next() % uint64(n)) }
func (r *chatFitRand) chance(percent int) bool {
	return r.intn(100) < percent
}

// chatFitCase generates a model request with every kind of content fitting
// distinguishes: optional sources in the task envelope, note reads and passage
// searches (current and legacy), large data and document results, protected
// native tool cycles and droppable older dialogue.
func chatFitCase(seed int) (chat.Run, chat.Request, int, func(chat.Request) (int, error)) {
	rng := chatFitRand(seed)
	paths := []string{"notes/alpha.md", "notes/beta.md", "notes/gamma.md", "notes/delta.md"}
	revisions := []string{"rev-one", "rev-two"}
	source := func(label string, i int) chat.Citation {
		path, revision := paths[rng.intn(len(paths))], revisions[rng.intn(len(revisions))]
		text := strings.Repeat(fmt.Sprintf("%s passage %d about %s. ", label, i, path), 1+rng.intn(120))
		if rng.chance(40) {
			// The same excerpt reached through another call is already covered.
			text = strings.Repeat("Shared passage about "+path+". ", []int{4, 60}[rng.intn(2)])
		}
		if rng.chance(10) {
			text += "\n[Omitted from model context; full text remains in the conversation.]"
		}
		return chat.Citation{ID: fmt.Sprintf("%s-%d", label, i), Kind: "committed", Path: path, URL: "/" + path + "?fmn-rev=" + revision, Title: "Heading " + path, Revision: revision, Hash: "hash-" + path + "-" + revision, PassageHash: fmt.Sprintf("passage-%s-%d", label, i), Text: text}
	}
	sources := func(label string, n int) []chat.Citation {
		out := []chat.Citation{}
		for i := 0; i < n; i++ {
			out = append(out, source(label, i))
		}
		return out
	}
	run := chat.Run{Options: chat.Options{Mode: "ask", Review: rng.chance(8)}}
	messages := []chat.Message{}
	// Older dialogue, sometimes holding a native tool cycle that must stay paired.
	for turn, turns := 0, rng.intn(6); turn < turns; turn++ {
		messages = append(messages, chat.Message{Role: "user", Content: strings.Repeat(fmt.Sprintf("Earlier question %d. ", turn), 1+rng.intn(80))})
		if rng.chance(15) {
			id := fmt.Sprintf("old-call-%d", turn)
			messages = append(messages, chat.Message{Role: "assistant", ToolCalls: []chat.ToolCall{{ID: id, Name: "list_documents", Arguments: json.RawMessage(`{}`)}}}, chat.Message{Role: "tool", ToolCallID: id, Content: `{"documents":[]}`})
		}
		messages = append(messages, chat.Message{Role: "assistant", Content: strings.Repeat(fmt.Sprintf("Earlier answer %d. ", turn), 1+rng.intn(80))})
	}
	switch roll := rng.intn(100); {
	case roll < 70:
		var found any = sources("initial", rng.intn(8))
		if rng.chance(4) {
			found = "not a source list"
		}
		payload, _ := json.Marshal(map[string]any{"status": "Selected evidence", "scope": "selected committed evidence; not complete notebook coverage", "sources": found, "explicitAttachments": []chat.Attachment{{Kind: "private", Path: "notes/attached.md", Content: strings.Repeat("attached ", rng.intn(200)), Hash: "attached-hash"}}})
		context := string(payload)
		if rng.chance(4) {
			context = "{not json"
		}
		messages = append(messages, chat.Message{Role: "user", Content: "CURRENT_TASK\n\n<notebook_guidance>\n\"Synthetic guidance.\"\n</notebook_guidance>\n<untrusted_notebook_context>\n" + context + "\n</untrusted_notebook_context>"})
	case roll < 97:
		messages = append(messages, chat.Message{Role: "user", Content: "CURRENT_TASK without an envelope"})
	}
	for call, calls := 0, rng.intn(5); call < calls; call++ {
		id := fmt.Sprintf("call-%d", call)
		names := []string{"read_note", "search_passages", "call_data_operation", "read_document", "search_notebooks", "notebook_map", "list_documents"}
		name := names[rng.intn(len(names))]
		messages = append(messages, chat.Message{Role: "assistant", ToolCalls: []chat.ToolCall{{ID: id, Name: name, Arguments: json.RawMessage(`{}`)}}})
		content := ""
		switch name {
		case "read_note", "search_passages":
			kind, found := "note_read", sources(id, 1)
			if name == "search_passages" {
				kind, found = "passage_search", sources(id, 1+rng.intn(5))
			}
			if rng.chance(20) {
				// A legacy result: clipped raw JSON that only its receipt can restore.
				var result json.RawMessage
				if name == "read_note" {
					result, _ = json.Marshal(webcore.NoteOutput{Path: found[0].Path, Content: found[0].Text, Revision: found[0].Revision})
				} else {
					passages := []map[string]any{}
					for _, f := range found {
						passages = append(passages, map[string]any{"text": f.Text, "path": f.Path, "revision": f.Revision, "citation": f.URL, "noteHash": f.Hash})
					}
					result, _ = json.Marshal(map[string]any{"passages": passages, "weak": rng.chance(50)})
				}
				op := chat.Operation{ID: "op-" + id, Name: name, ToolCallID: id, State: "completed", Result: result, Citations: found}
				// Receipts of other calls or states never restore this result.
				running, other := op, op
				running.State = "running"
				other.Result = json.RawMessage(`{"other":true}`)
				run.Operations = append(run.Operations, running, other, op)
				content = oldChatSourceRepresentation(op)
			} else {
				raw, _ := json.Marshal(chatSourceContext{Kind: kind, Sources: found, Weak: rng.chance(30)})
				content = string(raw)
			}
		case "call_data_operation":
			raw, _ := json.Marshal(map[string]any{"receipt": map[string]any{"id": "receipt-" + id, "connection": "synthetic", "operation": "list"}, "result": strings.Repeat("row ", []int{200, 4000}[rng.intn(2)])})
			content = string(raw)
		default:
			content = `{"result":"` + strings.Repeat("document text ", []int{1, 50, 3000}[rng.intn(3)]) + `"}`
		}
		messages = append(messages, chat.Message{Role: "tool", ToolCallID: id, Content: content})
	}
	limits := []int{1024, 2048, 4096, 8192, 16384, 32768}
	outputs := []int{128, 512, 1024}
	request := chat.Request{Model: "synthetic", System: "Synthetic system", Messages: messages, MaxOutputTokens: outputs[rng.intn(len(outputs))]}
	var estimate func(chat.Request) (int, error)
	switch roll := rng.intn(100); {
	case roll < 30:
		// A provider whose estimate differs from the default encoding size.
		estimate = func(r chat.Request) (int, error) { raw, _ := json.Marshal(r); return len(raw) / 4, nil }
	case roll < 36:
		// A provider whose estimate fails partway through fitting.
		calls, failAt := 0, 1+rng.intn(30)
		estimate = func(r chat.Request) (int, error) {
			if calls++; calls >= failAt {
				return 0, errors.New("synthetic estimate failure")
			}
			return estimateTokens(r), nil
		}
	}
	return run, request, limits[rng.intn(len(limits))], estimate
}

// chatFitBoundaryCase sweeps a request whose mandatory part approaches the
// soft target, so a whole-note read fits only within the hard limit.
func chatFitBoundaryCase(step int) (chat.Run, chat.Request, int, func(chat.Request) (int, error)) {
	read, _ := json.Marshal(chatSourceContext{Kind: "note_read", Sources: []chat.Citation{{ID: "read", Kind: "committed", Path: "notes/read.md", URL: "/notes/read.md?fmn-rev=rev", Revision: "rev", Hash: "hash", Text: strings.Repeat("Whole note sentence. ", 300)}}})
	messages := []chat.Message{
		{Role: "user", Content: "CURRENT_TASK " + strings.Repeat("x", 14000+40*step)},
		{Role: "assistant", ToolCalls: []chat.ToolCall{{ID: "read", Name: "read_note", Arguments: json.RawMessage(`{}`)}}},
		{Role: "tool", ToolCallID: "read", Content: string(read)},
	}
	return chat.Run{}, chat.Request{Model: "synthetic", System: "Synthetic system", Messages: messages, MaxOutputTokens: 1024}, 8192, nil
}

func chatFitCharacterizationCase(i int) (chat.Run, chat.Request, int, func(chat.Request) (int, error)) {
	if i < 400 {
		return chatFitCase(i + 1)
	}
	return chatFitBoundaryCase(i - 400)
}

// TestChatFitCharacterization records the observable result of fitting
// generated requests: the fitted request, its statistics and its error. The
// digests were recorded before fitChatRequest was split into its steps; update
// them with -update-chat-fit only for an intended change of behaviour.
func TestChatFitCharacterization(t *testing.T) {
	const cases = 440
	digests := make([]string, cases)
	outcomes := map[string]int{}
	for i := range digests {
		run, request, limit, estimate := chatFitCharacterizationCase(i)
		original, _ := json.Marshal(request)
		fitted, fit, err := fitChatRequest(run, request, limit, estimate)
		if after, _ := json.Marshal(request); string(after) != string(original) {
			t.Fatalf("case %d: fitting mutated the durable request", i+1)
		}
		message := ""
		switch {
		case err != nil:
			message = err.Error()
			outcomes["error"]++
		case fit.DroppedHistory > 0:
			outcomes["dropped history"]++
		case fit.SourcesAfter < fit.SourcesBefore:
			outcomes["reduced sources"]++
		case fit.Changed:
			outcomes["omitted results"]++
		default:
			outcomes["fits"]++
		}
		raw, _ := json.Marshal(struct {
			Request chat.Request
			Fit     ContextFit
			Error   string
		}{fitted, fit, message})
		sum := sha256.Sum256(raw)
		digests[i] = hex.EncodeToString(sum[:8])
	}
	t.Logf("outcomes: %v", outcomes)
	for _, outcome := range []string{"error", "dropped history", "reduced sources", "omitted results", "fits"} {
		if outcomes[outcome] == 0 {
			t.Fatalf("generated cases never reach %q: %v", outcome, outcomes)
		}
	}
	path := filepath.Join("testdata", "chat-fit-characterization.json")
	if *updateChatFit {
		raw, _ := json.MarshalIndent(digests, "", " ")
		if err := os.WriteFile(path, append(raw, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	if err := json.Unmarshal(raw, &want); err != nil || len(want) != cases {
		t.Fatalf("invalid recorded digests: %v", err)
	}
	for i := range want {
		if digests[i] != want[i] {
			run, request, limit, _ := chatFitCharacterizationCase(i)
			t.Errorf("case %d (limit %d, output %d, %d messages, review %v) changed: %s, recorded %s", i+1, limit, request.MaxOutputTokens, len(request.Messages), run.Options.Review, digests[i], want[i])
		}
	}
}
