package workspace

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"forgetmenote/internal/gitexport"
	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
)

// PurgedText stands where a purge redacted text, in the notebook and in the
// stores outside it that the purge cleans.
const PurgedText = purgedText

// A PurgeRedactor finds and redacts what a purge erases in what the notebook
// does not hold, such as chat transcripts and proposals that quoted it, the
// same way the purge redacts the notebook: an exact string, every match of an
// RE2 pattern, or each of a set of exact texts (PurgeQuotes), replaced with
// PurgedText.
type PurgeRedactor struct {
	m      purgeMatcher
	quotes *strings.Replacer
}

// NewPurgeRedactor returns the redactor of a text purge's scope, or
// ErrPurgeInvalid for a scope that is not a usable text purge.
func NewPurgeRedactor(scope PurgeScope) (PurgeRedactor, error) {
	if scope.Kind != PurgeText {
		return PurgeRedactor{}, ErrPurgeInvalid
	}
	m, err := newPurgeMatcher(scope)
	return PurgeRedactor{m: m}, err
}

// quoteRedactor redacts each of quotes, a text that holds another whole.
func quoteRedactor(quotes []string) *PurgeRedactor {
	slices.SortFunc(quotes, func(a, b string) int {
		if c := len(b) - len(a); c != 0 {
			return c
		}
		return strings.Compare(a, b)
	})
	pairs := make([]string, 0, 2*len(quotes))
	for _, q := range quotes {
		pairs = append(pairs, q, purgedText)
	}
	// At each position the replacer takes the first of pairs that matches:
	// the longest.
	return &PurgeRedactor{quotes: strings.NewReplacer(pairs...)}
}

// Holds reports whether s holds the text.
func (r PurgeRedactor) Holds(s string) bool {
	if r.quotes != nil {
		return r.quotes.Replace(s) != s
	}
	return r.m.count([]byte(s)) > 0
}

// Redact replaces the text in s with PurgedText.
func (r PurgeRedactor) Redact(s string) string {
	if r.quotes != nil {
		return r.quotes.Replace(s)
	}
	return string(r.m.replace([]byte(s)))
}

// MinQuotedLine is the shortest line of an erased version that a note or
// versions purge looks for in records outside the notebook: shorter lines are
// too common to tell that a record quoted the erased version rather than
// another note.
const MinQuotedLine = 24

// The most distinct texts a pattern purge's quotes hold, and the most bytes
// and distinct lines of erased versions a note or versions purge reads.
const (
	maxPurgeQuotes = 100000
	maxQuotedBytes = 64 << 20
	maxQuotedLines = 200000
)

// PurgeQuotes is what records outside a notebook, such as private chat
// transcripts, may quote of the content a purge plan erases, as the notebook
// holds it. A purge matches those records against it, never against text or
// a pattern the notebook does not hold, so a plan cannot tell whoever asks
// whether another person's records hold what they guess, and running it
// rewrites nothing else in them.
type PurgeQuotes struct {
	// Text, for a text purge, redacts its exact text, or each distinct text
	// its pattern matches in what the purge redacts: the versions in its
	// scope, the commit messages and the live documents it resets. It is nil
	// when the purge redacts nothing the notebook holds.
	Text *PurgeRedactor
	// Lines are, for a note or versions purge, the whole lines of the
	// versions it erases, MinQuotedLine bytes or longer, that no other file at
	// the head holds; Words are the words among them that could identify
	// something on their own (PurgeWords) and no other file at the head
	// holds. A purge's reason must quote neither.
	Lines map[string]bool
	Words map[string]bool
	// Partial reports that some of what the purge erases was too large to
	// read in full, so not all of it is looked for.
	Partial bool
}

// Quotes reports whether text quotes what the purge erases: a text of Text,
// one of Lines as a whole line, or one of Words.
func (q PurgeQuotes) Quotes(text string) bool {
	if q.Text != nil && q.Text.Holds(text) {
		return true
	}
	for _, line := range strings.Split(text, "\n") {
		if q.Lines[strings.TrimSpace(line)] {
			return true
		}
	}
	quoted := false
	PurgeWords(text, func(word string) { quoted = quoted || q.Words[word] })
	return quoted
}

// PurgeWords calls fn with each word of text that could identify something
// on its own, such as a card number, an account or an API key: four bytes or
// longer with a digit in it, or sixteen bytes or longer.
func PurgeWords(text string, fn func(string)) {
	separator := func(r rune) bool { return unicode.IsSpace(r) || strings.ContainsRune(",;!?()[]{}<>\"'`*|", r) }
	for _, field := range strings.FieldsFunc(text, separator) {
		word := strings.TrimRight(field, ".:")
		if len(word) >= 16 || len(word) >= 4 && strings.ContainsAny(word, "0123456789") {
			fn(word)
		}
	}
}

// PurgeQuotes returns what records outside the notebook may quote of what
// plan, which PreparePurge returned, erases. It reads the versions the plan
// erases and, for a note or versions purge, the files at the head, before
// the purge removes them. It changes nothing.
func (s *Store) PurgeQuotes(ctx context.Context, plan PurgePlan) (PurgeQuotes, error) {
	held := plan.From != "" || len(plan.Live) > 0
	if plan.Scope.Kind == PurgeText && (!held || plan.Scope.Pattern == "") {
		if !held {
			return PurgeQuotes{}, nil
		}
		r, err := NewPurgeRedactor(plan.Scope)
		if err != nil {
			return PurgeQuotes{}, err
		}
		return PurgeQuotes{Text: &r}, nil
	}
	h, err := s.purgeHistory(ctx)
	if err != nil {
		return PurgeQuotes{}, err
	}
	switch plan.Scope.Kind {
	case PurgeText:
		return s.patternQuotes(ctx, h, plan.Scope)
	case PurgeNote, PurgeVersions:
		return s.erasedQuotes(ctx, h, plan.Scope)
	}
	return PurgeQuotes{}, fmt.Errorf("%w: unknown purge kind %q", ErrPurgeInvalid, plan.Scope.Kind)
}

// patternQuotes returns the distinct texts a pattern purge's pattern matches
// in what it redacts, as planText finds them.
func (s *Store) patternQuotes(ctx context.Context, h *purgeHistory, scope PurgeScope) (PurgeQuotes, error) {
	m, err := newPurgeMatcher(scope)
	if err != nil {
		return PurgeQuotes{}, err
	}
	var selected map[string]bool
	if len(scope.Paths) > 0 {
		selected = map[string]bool{}
		for _, p := range scope.Paths {
			entry, err := h.entryAt(p)
			if err != nil {
				return PurgeQuotes{}, err
			}
			selected[entry] = true
		}
	}
	inScope := func(entry string) bool { return selected == nil || selected[entry] }
	var out PurgeQuotes
	found := map[string]bool{}
	add := func(content []byte) {
		for _, match := range m.re.FindAll(content, -1) {
			switch {
			case len(match) == 0 || !utf8.Valid(match) || found[string(match)]:
			case len(found) >= maxPurgeQuotes:
				out.Partial = true
			default:
				found[string(match)] = true
			}
		}
	}
	var blobs []gitobj.ID
	seen := map[gitobj.ID]bool{}
	for _, u := range h.uses {
		if u.mode != gitobj.ModeGitlink && inScope(u.entry) && !seen[u.blob] {
			seen[u.blob] = true
			blobs = append(blobs, u.blob)
		}
	}
	for start := 0; start < len(blobs); start += 500 {
		err := s.notes.ReadBlobs(ctx, blobs[start:min(start+500, len(blobs))], func(_ int, content []byte) error {
			if !isText(content) {
				return nil
			}
			add(content)
			if jsonDocument(content) {
				jsonStrings(content, func(start, end int) {
					if value, ok := escapedJSONString(content[start:end]); ok {
						add([]byte(value))
					}
				})
			}
			return nil
		})
		if err != nil {
			return PurgeQuotes{}, err
		}
	}
	for _, message := range h.messages {
		add([]byte(message))
	}
	live, err := s.notes.LiveEntries(ctx)
	if err != nil {
		return PurgeQuotes{}, err
	}
	for _, entry := range live {
		if !inScope(entry) {
			continue
		}
		st, err := s.notes.LiveState(ctx, entry)
		if err != nil {
			return PurgeQuotes{}, err
		}
		updates := make([][]byte, len(st.Updates))
		for i, u := range st.Updates {
			updates[i] = u.Update
		}
		if st.Snapshot == nil && len(updates) == 0 {
			continue
		}
		if doc, err := yjsLive.Load(st.Snapshot, updates); err == nil {
			add([]byte(doc.Text()))
		}
	}
	if len(found) > 0 {
		out.Text = quoteRedactor(slices.Collect(maps.Keys(found)))
	}
	return out, nil
}

// erasedQuotes returns the lines and words of the versions a note or
// versions purge erases that no other file at the head holds.
func (s *Store) erasedQuotes(ctx context.Context, h *purgeHistory, scope PurgeScope) (PurgeQuotes, error) {
	entry, err := h.entryAt(scope.Path)
	if err != nil {
		return PurgeQuotes{}, err
	}
	erased := map[gitobj.ID]bool{}
	if scope.Kind == PurgeNote {
		for _, u := range h.uses {
			if u.entry == entry && u.mode != gitobj.ModeGitlink {
				erased[u.blob] = true
			}
		}
	} else {
		for _, v := range scope.Versions {
			id, err := parseRevision(v)
			if err != nil || id.IsZero() {
				return PurgeQuotes{}, fmt.Errorf("%w: version %q", ErrPurgeInvalid, v)
			}
			for _, u := range h.uses {
				if u.entry == entry && u.commit == id && u.mode != gitobj.ModeGitlink {
					erased[u.blob] = true
				}
			}
		}
	}
	out := PurgeQuotes{Lines: map[string]bool{}, Words: map[string]bool{}}
	blobs := slices.SortedFunc(maps.Keys(erased), func(a, b gitobj.ID) int { return strings.Compare(a.String(), b.String()) })
	read := 0
	for start := 0; start < len(blobs); start += 100 {
		if read > maxQuotedBytes || len(out.Lines) > maxQuotedLines {
			out.Partial = true
			break
		}
		err := s.notes.ReadBlobs(ctx, blobs[start:min(start+100, len(blobs))], func(_ int, content []byte) error {
			if !isText(content) {
				return nil
			}
			read += len(content)
			for _, line := range strings.Split(string(content), "\n") {
				trimmed := strings.TrimSpace(line)
				if len(trimmed) >= MinQuotedLine && trimmed != purgedText {
					out.Lines[trimmed] = true
				}
				PurgeWords(trimmed, func(word string) { out.Words[word] = true })
			}
			return nil
		})
		if err != nil {
			return PurgeQuotes{}, err
		}
	}
	// What the notebook keeps at the head, in the files the purge leaves, is
	// not the purge's to redact elsewhere: a template line or a word other
	// notes share.
	notePath, _ := h.pathOf(entry)
	var kept []gitobj.ID
	for _, f := range h.files {
		switch {
		case f.Mode == gitobj.ModeGitlink:
		case f.EntryID == entry && (scope.Kind == PurgeNote || erased[f.Blob]):
		case scope.Kind == PurgeNote && f.Path == commentPath(notePath):
		default:
			kept = append(kept, f.Blob)
		}
	}
	for start := 0; start < len(kept) && (len(out.Lines) > 0 || len(out.Words) > 0); start += 500 {
		err := s.notes.ReadBlobs(ctx, kept[start:min(start+500, len(kept))], func(_ int, content []byte) error {
			if !isText(content) {
				return nil
			}
			for _, line := range strings.Split(string(content), "\n") {
				trimmed := strings.TrimSpace(line)
				delete(out.Lines, trimmed)
				PurgeWords(trimmed, func(word string) { delete(out.Words, word) })
			}
			return nil
		})
		if err != nil {
			return PurgeQuotes{}, err
		}
	}
	return out, nil
}

// ForgetPurge drops what this store holds in memory of its notebook from
// before a purge another gateway ran: committed reads, which it stops
// serving as soon as it reads the rewritten history's incarnation
// (committedReads), cached metadata, and every live document it loaded,
// which it reads again from the notes store before using. A gateway calls
// it when it hears of the purge.
func (s *Store) ForgetPurge() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.forgetPurged(nil)
	if !s.liveClosed {
		// As after missed hints: every loaded document is read again.
		n := s.backend.(*dbNotebook)
		n.liveHeard(n.liveSync(), notestore.LiveChange{})
	}
}

// PurgeExporter returns the Git export of this store's notebook, which a
// purge rebuilds, when this process runs it (UseExporter); otherwise nil.
func (s *Store) PurgeExporter() *gitexport.Exporter {
	exporters.Lock()
	defer exporters.Unlock()
	return exporters.stores[s.noteStore]
}
