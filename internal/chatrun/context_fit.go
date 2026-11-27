package chatrun

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"forgetmenote/internal/chat"
	"forgetmenote/internal/dataops"
	"forgetmenote/internal/knowledge"
	"forgetmenote/internal/notebooks"
	"forgetmenote/internal/webcore"
)

type ContextFit struct {
	// BeforeTokens is the estimate of the request as received. A request that
	// fits once its evidence is projected for the model is estimated only once,
	// so then it equals AfterTokens.
	BeforeTokens, AfterTokens, DroppedHistory int
	SourcesBefore, SourcesAfter               int
	Changed                                   bool
	FinalAnswer                               bool
}
type chatSourceContext struct {
	Weak    bool            `json:"weakEvidence,omitempty"`
	Kind    string          `json:"kind"`
	Sources []chat.Citation `json:"sources"`
	Message string          `json:"message,omitempty"`
	Partial bool            `json:"partial,omitempty"`
}

func modelChatSources(sources []chat.Citation) []chat.Citation {
	out := make([]chat.Citation, len(sources))
	for i, source := range sources {
		// Whole-note hash and revision are required guarded-edit preconditions.
		// Passage checksums and reference closure remain in the durable record.
		source.PassageHash, source.References = "", ""
		out[i] = source
	}
	return out
}
func CompactSourceResult(name string, result json.RawMessage, citations []chat.Citation) (string, bool) {
	envelope := chatSourceContext{Sources: modelChatSources(citations)}
	switch name {
	case "read_document":
		var document notebooks.DocumentOutput
		if json.Unmarshal(result, &document) != nil || document.Path == "" || document.Revision == "" || document.Hash == "" {
			return "", false
		}
		// HTML is the UI's rendering of the same Markdown. Keep the complete
		// original, health warnings and range metadata until request fitting.
		document.HTML = ""
		raw, _ := json.Marshal(document)
		return string(raw), true
	case "search_notebooks":
		if len(citations) == 0 {
			return "", false
		}
		envelope.Kind = "document_evidence"
	case "read_note":
		var note webcore.NoteOutput
		if json.Unmarshal(result, &note) != nil || note.Path == "" || note.Revision == "" {
			return "", false
		}
		if len(envelope.Sources) == 0 {
			return "", false
		}
		envelope.Kind = "note_read"
		envelope.Sources = envelope.Sources[:1]
		envelope.Sources[0].Text = note.Content
	case "search_passages":
		var status knowledge.RetrievalResult
		if json.Unmarshal(result, &status) != nil || len(status.Passages) == 0 && len(citations) == 0 {
			return "", false
		}
		if len(envelope.Sources) == 0 {
			for _, p := range status.Passages {
				envelope.Sources = append(envelope.Sources, passageCitation(p.ID, "", p))
			}
			envelope.Sources = modelChatSources(envelope.Sources)
		}
		envelope.Kind = "passage_search"
		envelope.Weak = status.Weak
		envelope.Message = status.Message
	default:
		return "", false
	}
	// The complete native request owns context fitting, including whole-note reads.
	raw, _ := json.Marshal(envelope)
	return string(raw), true
}

// Only the last server-built user envelope has optional automatic sources. Its
// entire task/guidance prefix and every other JSON field, including attachments,
// remain mandatory. Rightmost delimiters cannot be supplied by escaped note JSON.
func optionalChatSources(messages []chat.Message, review bool) (int, string, map[string]json.RawMessage, []chat.Citation) {
	if review {
		return -1, "", nil, nil
	}
	index := -1
	for i, m := range messages {
		if m.Role == "user" {
			index = i
		}
	}
	if index < 0 {
		return -1, "", nil, nil
	}
	content := messages[index].Content
	const opening = "\n<untrusted_notebook_context>\n"
	const closing = "\n</untrusted_notebook_context>"
	split := strings.LastIndex(content, opening)
	if split < 0 || !strings.HasSuffix(content, closing) || !strings.Contains(content[:split], "\n\n<notebook_guidance>\n") || !strings.HasSuffix(content[:split], "\n</notebook_guidance>") {
		return -1, "", nil, nil
	}
	var payload map[string]json.RawMessage
	if json.Unmarshal([]byte(content[split+len(opening):len(content)-len(closing)]), &payload) != nil {
		return -1, "", nil, nil
	}
	var sources []chat.Citation
	if json.Unmarshal(payload["sources"], &sources) != nil {
		return -1, "", nil, nil
	}
	return index, content[:split], payload, modelChatSources(sources)
}

// Legacy read messages appended pinned metadata after their clipped JSON. Its
// operation-specific citation and revision are part of the match, not just the
// shared content prefix. Old custom search messages had no appended metadata;
// if different receipts could produce that same clipped result, do not guess.
func legacyChatSourceResult(run chat.Run, name string, message chat.Message) (string, bool) {
	match := ""
	for _, op := range run.Operations {
		if op.State != "completed" || op.Name != name || op.ToolCallID != message.ToolCallID || len(op.Result) == 0 {
			continue
		}
		old := TextLimit(string(op.Result), 12000)
		if name == "read_note" && len(op.Citations) > 0 {
			metadata := append([]chat.Citation(nil), op.Citations...)
			for i := range metadata {
				metadata[i].Text = ""
			}
			raw, _ := json.Marshal(metadata)
			old += "\nPinned notebook evidence: " + string(raw)
		}
		if message.Content != old {
			continue
		}
		compact, ok := CompactSourceResult(name, op.Result, op.Citations)
		if !ok {
			continue
		}
		if match != "" && match != compact {
			return "", false
		}
		match = compact
	}
	return match, match != ""
}

// Fit a private model request, never the durable transcript or native state.
// Native assistant blocks and call/result identities are indivisible. Only
// bounded, known evidence excerpts and older conversational history are optional.
func fitChatRequest(run chat.Run, request chat.Request, contextTokens int, estimators ...func(chat.Request) (int, error)) (chat.Request, ContextFit, error) {
	estimate := func(req chat.Request) (int, error) { return estimateTokens(req), nil }
	if len(estimators) > 0 && estimators[0] != nil {
		estimate = estimators[0]
	}
	f := projectChatRequest(run, request, contextTokens, estimate)
	// Each estimate encodes the whole request, which may hold entire notes.
	// Estimate the model projection once; only a request that does not fit is
	// estimated again as received and then reduced.
	if err := f.update(); err != nil {
		return f.request, f.stats, err
	}
	if f.stats.AfterTokens <= f.hard {
		f.keepAllSources()
		return f.request, f.stats, nil
	}
	err := f.reduce(request)
	return f.request, f.stats, err
}

// FitRound fits one round's model request. An ordinary Ask answers from
// retained evidence once its exploration allowance is spent, and also when
// its working request cannot fit.
func FitRound(ctx context.Context, run chat.Run, request chat.Request, connection chat.Connection, estimators ...func(chat.Request) (int, error)) (chat.Request, ContextFit, error) {
	if chatAnswerDue(ctx, run, connection) {
		request = FinalRequest(request)
	}
	fitted, fit, err := fitChatRequest(run, request, connection.Limits.ContextTokens, estimators...)
	if err != nil && OrdinaryAsk(run) && !request.FinalAnswer {
		request = FinalRequest(request)
		fitted, fit, err = fitChatRequest(run, request, connection.Limits.ContextTokens, estimators...)
	}
	fit.FinalAnswer = request.FinalAnswer
	return fitted, fit, err
}

// chatFit is one model request being fitted to a context window. Its messages
// are a private copy. The optional sources of each read or search result and
// of the task envelope are held apart, so they can be rendered again as
// sources are retained.
type chatFit struct {
	request       chat.Request
	estimate      func(chat.Request) (int, error)
	stats         ContextFit
	contextTokens int
	// hard bounds the request beside its output allowance; soft also leaves
	// room to continue.
	hard, soft int
	// calls names each tool call by its ID.
	calls map[string]string
	// envelopes are the read and search results that carry sources, by
	// message index.
	envelopes map[int]*chatSourceContext
	task      chatTaskSources
	// history is the index of the current task's message; the dialogue before
	// it is optional.
	history int
}

// chatTaskSources are the optional sources of the last server-built user
// envelope. Its index is -1 when the request has none.
type chatTaskSources struct {
	index   int
	prefix  string
	payload map[string]json.RawMessage
	sources []chat.Citation
}

// chatFitCandidate is an optional source waiting to be retained in the
// message it came from.
type chatFitCandidate struct {
	source        chat.Citation
	message, rank int
	read, matched bool
}

// projectChatRequest copies request for fitting and finds its optional
// evidence: the sources of each read or search result, restoring a legacy
// result from its exact receipt, and the task envelope's sources, rendered as
// the model sees them.
func projectChatRequest(run chat.Run, request chat.Request, contextTokens int, estimate func(chat.Request) (int, error)) *chatFit {
	f := &chatFit{request: request, estimate: estimate, contextTokens: contextTokens, calls: map[string]string{}, envelopes: map[int]*chatSourceContext{}}
	f.request.Messages = append([]chat.Message(nil), request.Messages...)
	f.hard = contextTokens - request.MaxOutputTokens
	f.soft = f.hard - min(4096, max(512, contextTokens/4))
	for i := range f.request.Messages {
		m := &f.request.Messages[i]
		if m.Role == "assistant" {
			for _, call := range m.ToolCalls {
				f.calls[call.ID] = call.Name
			}
		}
		if m.Role == "tool" {
			if envelope := chatSourceEnvelope(run, f.calls[m.ToolCallID], m); envelope != nil {
				f.envelopes[i] = envelope
			}
		}
	}
	f.task.index, f.task.prefix, f.task.payload, f.task.sources = optionalChatSources(f.request.Messages, run.Options.Review)
	f.history = f.task.index
	if f.history < 0 {
		for i, m := range f.request.Messages {
			if m.Role == "user" {
				f.history = i
			}
		}
	}
	if f.task.index >= 0 {
		f.renderTask()
	}
	return f
}

// chatSourceEnvelope reads the sources of a read_note or search_passages
// result. Older interrupted runs hold raw or clipped JSON instead; that is
// reconstructed only from its exact, already acknowledged durable receipt,
// which then replaces the message's content.
func chatSourceEnvelope(run chat.Run, name string, m *chat.Message) *chatSourceContext {
	kind := ""
	if name == "read_note" {
		kind = "note_read"
	} else if name == "search_passages" {
		kind = "passage_search"
	}
	if kind == "" {
		return nil
	}
	var envelope chatSourceContext
	if json.Unmarshal([]byte(m.Content), &envelope) != nil || envelope.Kind != kind {
		if compact, ok := legacyChatSourceResult(run, name, *m); ok {
			json.Unmarshal([]byte(compact), &envelope)
			m.Content = compact
		}
	}
	if envelope.Kind != kind || len(envelope.Sources) == 0 {
		return nil
	}
	envelope.Sources = modelChatSources(envelope.Sources)
	return &envelope
}

func (f *chatFit) update() error {
	var err error
	f.stats.AfterTokens, err = f.estimate(f.request)
	return err
}

// keepAllSources records a request that fits as projected.
func (f *chatFit) keepAllSources() {
	f.stats.BeforeTokens = f.stats.AfterTokens
	for _, envelope := range f.envelopes {
		f.stats.SourcesBefore += len(envelope.Sources)
	}
	f.stats.SourcesBefore += len(f.task.sources)
	f.stats.SourcesAfter = f.stats.SourcesBefore
}

// reduce fits a request that does not fit as projected. All optional evidence
// shares one encoded request budget; a per-tool text ceiling cannot bound
// repeated URLs, paths, hashes and revision metadata. Older dialogue goes
// first, then excerpts are retained while they fit, and finally oversized
// results are omitted.
func (f *chatFit) reduce(original chat.Request) error {
	before, err := f.estimate(original)
	if err != nil {
		return err
	}
	f.stats.BeforeTokens = before
	candidates := f.withdrawSources()
	f.render()
	if err := f.update(); err != nil {
		return err
	}
	if err := f.dropHistory(f.historyTarget(len(candidates) > 0), candidates); err != nil {
		return err
	}
	ordered := orderChatFitCandidates(candidates)
	if err := f.retainSources(ordered); err != nil {
		return err
	}
	if err := f.retainOneSource(ordered); err != nil {
		return err
	}
	f.stats.Changed = f.stats.BeforeTokens != f.stats.AfterTokens || f.stats.DroppedHistory > 0 || f.stats.SourcesBefore != f.stats.SourcesAfter
	if err := f.omitLargeResults(); err != nil {
		return err
	}
	if f.stats.AfterTokens > f.hard {
		return FailureError(chatContextLimit, "Model context limit reached after reducing optional evidence. The task, notebook guidance, explicit attachments or native tool history still cannot fit; shorten explicit inputs or start a fresh focused conversation")
	}
	return nil
}

// withdrawSources removes every optional source from its message, in message
// order and then the task's, as candidates to retain.
func (f *chatFit) withdrawSources() []chatFitCandidate {
	candidates := []chatFitCandidate{}
	for i := range f.request.Messages {
		if envelope := f.envelopes[i]; envelope != nil {
			for rank, source := range envelope.Sources {
				candidates = append(candidates, chatFitCandidate{source: source, message: i, rank: rank, read: envelope.Kind == "note_read"})
			}
			envelope.Sources = nil
		}
	}
	if f.task.index >= 0 {
		for rank, source := range f.task.sources {
			candidates = append(candidates, chatFitCandidate{source: source, message: f.task.index, rank: rank})
		}
		f.task.sources = nil
	}
	f.stats.SourcesBefore += len(candidates)
	return candidates
}

func (f *chatFit) renderTask() {
	raw, _ := json.Marshal(f.task.sources)
	f.task.payload["sources"] = raw
	raw, _ = json.Marshal(f.task.payload)
	f.request.Messages[f.task.index].Content = f.task.prefix + "\n<untrusted_notebook_context>\n" + string(raw) + "\n</untrusted_notebook_context>"
}

// render writes the retained sources back into their messages. A reduced
// result says that it is partial.
func (f *chatFit) render() {
	if f.task.index >= 0 {
		f.renderTask()
	}
	for i, envelope := range f.envelopes {
		envelope.Partial = true
		if len(envelope.Sources) == 0 {
			envelope.Message = "Completed. Original evidence omitted from this model context; omission does not mean absence. Full result remains in the conversation."
		} else {
			envelope.Message = "Selected original evidence; this partial context does not certify full note or notebook coverage."
		}
		if envelope.Weak {
			envelope.Message += " Weak topic match: selected evidence does not establish requested details or notebook absence."
		}
		raw, _ := json.Marshal(envelope)
		f.request.Messages[i].Content = string(raw)
	}
}

// historyTarget leaves room for original evidence before retaining optional
// old dialogue.
func (f *chatFit) historyTarget(evidence bool) int {
	if evidence {
		return f.soft - min(2048, max(256, f.contextTokens/8))
	}
	return f.soft
}

// dropHistory drops the oldest dialogue, one user exchange at a time, until
// the request fits target. It stops at an exchange holding a native tool cycle
// or continuation, which is never split.
func (f *chatFit) dropHistory(target int, candidates []chatFitCandidate) error {
	for f.stats.AfterTokens > target && f.history > 0 {
		end := 1
		for end < f.history && f.request.Messages[end].Role != "user" {
			end++
		}
		for _, m := range f.request.Messages[:end] {
			if len(m.ToolCalls) > 0 || m.ToolCallID != "" || len(m.Continuation) > 0 {
				return nil
			}
		}
		f.request.Messages = f.request.Messages[end:]
		f.stats.DroppedHistory += end
		if f.task.index >= 0 {
			f.task.index -= end
		}
		f.history -= end
		shifted := map[int]*chatSourceContext{}
		for i, envelope := range f.envelopes {
			shifted[i-end] = envelope
		}
		f.envelopes = shifted
		for i := range candidates {
			candidates[i].message -= end
		}
		if err := f.update(); err != nil {
			return err
		}
	}
	return nil
}

// orderChatFitCandidates orders candidates for retention. A ranked passage from
// a read note carries the requested match, which may occur beyond every
// retained read prefix, so it is kept independently and first; only text
// actually retained can establish duplicate coverage. Ranked matches precede
// broad reads. Both retain local ranking and breadth across notes before
// another passage from the same note.
func orderChatFitCandidates(candidates []chatFitCandidate) []chatFitCandidate {
	for i := range candidates {
		if candidates[i].read {
			continue
		}
		for _, read := range candidates {
			a, b := candidates[i].source, read.source
			if read.read && a.Kind == b.Kind && a.Path == b.Path && a.Revision == b.Revision && a.Hash == b.Hash {
				candidates[i].matched = true
				break
			}
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if a.matched != b.matched {
			return a.matched
		}
		if a.read != b.read {
			return a.read
		}
		if a.rank != b.rank {
			return a.rank < b.rank
		}
		return a.message > b.message
	})
	ordered := make([]chatFitCandidate, 0, len(candidates))
	remaining := []chatFitCandidate{}
	notes := map[string]bool{}
	for _, entry := range candidates {
		key := entry.source.Kind + "\x00" + entry.source.Path + "\x00" + entry.source.Revision
		if notes[key] {
			remaining = append(remaining, entry)
		} else {
			notes[key] = true
			ordered = append(ordered, entry)
		}
	}
	return append(ordered, remaining...)
}

// retainSources keeps each candidate, in order, at the largest excerpt that
// fits, up to one source per 1024 context tokens. A broad read must leave room
// to continue; a ranked passage, whole if it fits, needs only the mandatory
// output allowance.
func (f *chatFit) retainSources(ordered []chatFitCandidate) error {
	target := f.soft
	if target <= f.stats.AfterTokens {
		target = f.hard
	}
	maximum := max(4, min(24, f.contextTokens/1024))
	for _, entry := range ordered {
		if f.stats.SourcesAfter >= maximum {
			break
		}
		if f.covered(entry.source) {
			continue
		}
		budgets := []int{1600, 1000, 600, 256}
		entryTarget := target
		if !entry.read {
			// Optional continuation headroom cannot displace a bounded ranked
			// original that fits alongside the mandatory output allowance.
			budgets = append([]int{len(chatExcerptOriginal(entry.source.Text))}, budgets...)
			entryTarget = f.hard
		}
		if _, err := f.retainExcerpt(entry, budgets, entryTarget); err != nil {
			return err
		}
	}
	return nil
}

// retainOneSource keeps one useful excerpt when none was retained: soft
// continuation room must not erase every substantive original if the
// mandatory native and task baseline permits one.
func (f *chatFit) retainOneSource(ordered []chatFitCandidate) error {
	if f.stats.SourcesAfter > 0 {
		return nil
	}
	for _, entry := range ordered {
		kept, err := f.retainExcerpt(entry, []int{1000, 600, 256}, f.hard)
		if err != nil || kept {
			return err
		}
	}
	return nil
}

// retainExcerpt keeps entry at the first budget whose excerpt fits target.
func (f *chatFit) retainExcerpt(entry chatFitCandidate, budgets []int, target int) (bool, error) {
	for _, budget := range budgets {
		kept, err := f.trySource(entry, budget, target)
		if err != nil {
			return false, err
		}
		if kept {
			f.stats.SourcesAfter++
			return true, nil
		}
	}
	return false, nil
}

// trySource adds entry's excerpt of at most budget bytes to its message and
// keeps it if the request then fits target.
func (f *chatFit) trySource(entry chatFitCandidate, budget, target int) (bool, error) {
	source := entry.source
	source.Text = TextLimit(chatExcerptOriginal(source.Text), budget)
	if f.covered(source) {
		return false, nil
	}
	if entry.message == f.task.index {
		f.task.sources = append(f.task.sources, source)
	} else {
		f.envelopes[entry.message].Sources = append(f.envelopes[entry.message].Sources, source)
	}
	f.render()
	if err := f.update(); err != nil {
		return false, err
	}
	if f.stats.AfterTokens <= target {
		return true, nil
	}
	if entry.message == f.task.index {
		f.task.sources = f.task.sources[:len(f.task.sources)-1]
	} else {
		envelope := f.envelopes[entry.message]
		envelope.Sources = envelope.Sources[:len(envelope.Sources)-1]
	}
	f.render()
	return false, f.update()
}

// covered reports whether a retained source already holds source's text.
func (f *chatFit) covered(source chat.Citation) bool {
	actual := func(prior chat.Citation) bool {
		return prior.Kind == source.Kind && prior.Path == source.Path && prior.Revision == source.Revision && prior.Hash == source.Hash && prior.URL == source.URL && strings.Contains(chatExcerptOriginal(prior.Text), chatExcerptOriginal(source.Text))
	}
	for _, prior := range f.task.sources {
		if actual(prior) {
			return true
		}
	}
	for _, envelope := range f.envelopes {
		for _, prior := range envelope.Sources {
			if actual(prior) {
				return true
			}
		}
	}
	return false
}

// omitLargeResults keeps valid original results whenever they fit. When they
// do not, it replaces the largest data, document, search or map result with
// honest receipt metadata until the request fits, keeping the tool call
// paired, so the model can narrow the operation or export the retained result
// instead of terminating the answer.
func (f *chatFit) omitLargeResults() error {
	for f.stats.AfterTokens > f.hard {
		largest := -1
		for i, m := range f.request.Messages {
			name := f.calls[m.ToolCallID]
			if m.Role != "tool" || name != "call_data_operation" && name != "read_document" && name != "search_notebooks" && name != "notebook_map" {
				continue
			}
			var omitted struct {
				ContextOmitted bool `json:"contextOmitted"`
			}
			_ = json.Unmarshal([]byte(m.Content), &omitted)
			if !omitted.ContextOmitted && (largest < 0 || len(m.Content) > len(f.request.Messages[largest].Content)) {
				largest = i
			}
		}
		if largest < 0 {
			return nil
		}
		message := &f.request.Messages[largest]
		out := map[string]any{"contextOmitted": true, "error": "The original result does not fit the remaining model context. Do not infer values from this omission. Request a smaller page or aggregate; a retained data receipt can be exported."}
		if f.calls[message.ToolCallID] == "call_data_operation" {
			var data dataops.Result
			if json.Unmarshal([]byte(message.Content), &data) == nil && data.Receipt.ID != "" {
				out["receipt"] = data.Receipt
			}
		}
		raw, _ := json.Marshal(out)
		message.Content = string(raw)
		f.stats.Changed = true
		if err := f.update(); err != nil {
			return err
		}
	}
	return nil
}

// Remove only our trailing excerpt annotation before further budgeting, avoiding
// chains of omission notices that consume the shared evidence allowance.
func chatExcerptOriginal(text string) string {
	return strings.TrimSuffix(text, "\n[Omitted from model context; full text remains in the conversation.]")
}
