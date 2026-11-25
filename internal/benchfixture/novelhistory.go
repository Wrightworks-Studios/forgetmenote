package benchfixture

import (
	"bytes"
	"errors"
	"fmt"
	"iter"
	"slices"
	"strings"
	"time"
)

// NovelHistoryOptions configures F1h. Zero fields take the benchmark defaults.
type NovelHistoryOptions struct {
	Seed  uint64
	Edits int       // edit revisions after the note is created; default 5000
	Start time.Time // default 2025-01-06T09:00:00Z
	Span  time.Duration
	Path  string // initial note path; default "Manuscripts/Moby-Dick.md"
}

// DefaultNovelEdits is F1h's benchmark length.
const DefaultNovelEdits = 5000

var novelPaths = []string{
	"Manuscripts/Moby-Dick.md",
	"Manuscripts/Moby-Dick; or, The Whale.md",
	"Manuscripts/The Whale.md",
	"Drafts/Moby-Dick.md",
	"Drafts/Moby-Dick (revised).md",
	"Novels/Melville/Moby-Dick.md",
	"Moby-Dick.md",
}

// NovelHistory simulates F1h: the init commit, the creation of novel (an F1
// note) and o.Edits edits by three humans and two agents. The edit mix is
// exact: 70% word or sentence edits (three word edits to one sentence edit),
// 15% paragraph inserts or deletes, 5% chapter moves, 5% front matter changes
// and 5% renames, shuffled by the seed. Every revision changes one file, so
// Changes[0] is its (path, oldPath, content). The returned sequence can be
// iterated repeatedly and yields the same revisions each time.
func NovelHistory(novel []byte, o NovelHistoryOptions) (iter.Seq[Revision], error) {
	if o.Edits == 0 {
		o.Edits = DefaultNovelEdits
	}
	if o.Start.IsZero() {
		o.Start = time.Date(2025, 1, 6, 9, 0, 0, 0, time.UTC)
	}
	if o.Span == 0 {
		o.Span = 270 * 24 * time.Hour
	}
	if o.Path == "" {
		o.Path = novelPaths[0]
	}
	if o.Edits < 0 || !strings.HasSuffix(o.Path, ".md") {
		return nil, errors.New("benchfixture: invalid novel history options")
	}
	doc, err := parseNovel(novel)
	if err != nil {
		return nil, err
	}
	return func(yield func(Revision) bool) {
		sim := newNovelSim(doc.clone(), o)
		for r := range sim.revisions() {
			if !yield(r) {
				return
			}
		}
	}, nil
}

type field struct{ key, value string }

type novelDoc struct {
	front  []field
	blocks []string // title heading, then chapter headings and paragraphs
}

func (d novelDoc) clone() novelDoc {
	return novelDoc{slices.Clone(d.front), slices.Clone(d.blocks)}
}

// parseNovel accepts exactly the shape ConvertGutenberg writes.
func parseNovel(content []byte) (novelDoc, error) {
	var d novelDoc
	text := string(content)
	rest, ok := strings.CutPrefix(text, "---\n")
	header, body, found := strings.Cut(rest, "\n---\n\n")
	if !ok || !found || !strings.HasSuffix(body, "\n") {
		return d, errors.New("benchfixture: novel needs a front matter block and a trailing newline")
	}
	for _, line := range strings.Split(header, "\n") {
		key, value, ok := strings.Cut(line, ": ")
		if !ok {
			return d, fmt.Errorf("benchfixture: unsupported front matter line %q", line)
		}
		d.front = append(d.front, field{key, value})
	}
	d.blocks = strings.Split(strings.TrimSuffix(body, "\n"), "\n\n")
	paragraphs := 0
	for _, b := range d.blocks {
		if isParagraph(b) {
			paragraphs++
		}
	}
	if len(d.front) == 0 || d.front[0].key != "fmn-id" || !strings.HasPrefix(d.blocks[0], "# ") || len(d.sections()) < 2 || paragraphs < 2 || !bytes.Equal(d.render(), content) {
		return d, errors.New("benchfixture: novel must be an F1-shaped note: fmn-id first, a title heading and at least two ## chapters")
	}
	return d, nil
}

func (d novelDoc) render() []byte {
	n := 16
	for _, f := range d.front {
		n += len(f.key) + len(f.value) + 3
	}
	for _, b := range d.blocks {
		n += len(b) + 2
	}
	out := make([]byte, 0, n)
	out = append(out, "---\n"...)
	for _, f := range d.front {
		out = append(out, f.key+": "+f.value+"\n"...)
	}
	out = append(out, "---\n\n"...)
	for i, b := range d.blocks {
		if i > 0 {
			out = append(out, "\n\n"...)
		}
		out = append(out, b...)
	}
	return append(out, '\n')
}

// sections returns the start index of every "## " chapter block.
func (d novelDoc) sections() []int {
	var starts []int
	for i, b := range d.blocks {
		if strings.HasPrefix(b, "## ") {
			starts = append(starts, i)
		}
	}
	return starts
}

func isParagraph(block string) bool { return !strings.HasPrefix(block, "#") }

type novelSim struct {
	o          NovelHistoryOptions
	doc        novelDoc
	path       string
	r          *rng
	words      []string // inserted words, drawn from the novel itself
	sentences  []string
	cursor     int // block of the previous edit; edits cluster around it
	paragraphs int // paragraph count at creation; inserts and deletes stay near it
	git        *gitState
}

func newNovelSim(doc novelDoc, o NovelHistoryOptions) *novelSim {
	s := &novelSim{o: o, doc: doc, path: o.Path, r: newRNG(o.Seed, "f1h"), git: newGitState()}
	for _, b := range doc.blocks {
		if !isParagraph(b) {
			continue
		}
		s.paragraphs++
		for _, w := range strings.Fields(b) {
			if len(w) >= 3 && strings.Trim(w, "abcdefghijklmnopqrstuvwxyz") == "" {
				s.words = append(s.words, w)
			}
		}
		for _, sentence := range splitSentences(b) {
			if len(sentence) >= 30 && len(sentence) <= 300 && strings.HasSuffix(sentence, ".") {
				s.sentences = append(s.sentences, sentence)
			}
		}
	}
	if len(s.words) == 0 {
		s.words = []string{"whale", "sea", "ship"}
	}
	if len(s.sentences) == 0 {
		s.sentences = []string{"The sea was calm and the ship moved on."}
	}
	return s
}

// splitSentences cuts a paragraph after ". ", "? " and "! ".
func splitSentences(p string) []string {
	var out []string
	start := 0
	for i := 1; i < len(p)-1; i++ {
		if p[i] == ' ' && (p[i-1] == '.' || p[i-1] == '?' || p[i-1] == '!') {
			out = append(out, p[start:i])
			start = i + 1
		}
	}
	return append(out, p[start:])
}

// novelSchedule returns the exact, shuffled edit mix for n edits.
func novelSchedule(r *rng, n int) []string {
	paragraph, chapter, front, rename := n*15/100, n*5/100, n*5/100, n*5/100
	text := n - paragraph - chapter - front - rename
	sentence := text / 4
	kinds := make([]string, 0, n)
	add := func(kind string, count int) {
		for range count {
			kinds = append(kinds, kind)
		}
	}
	add(KindWordEdit, text-sentence)
	add(KindSentenceEdit, sentence)
	add(KindParagraphInsert, paragraph) // replaced by deletes as the paragraph count drifts
	add(KindChapterMove, chapter)
	add(KindFrontMatter, front)
	add(KindRename, rename)
	r.shuffle(len(kinds), func(i, j int) { kinds[i], kinds[j] = kinds[j], kinds[i] })
	return kinds
}

func (s *novelSim) revisions() iter.Seq[Revision] {
	return func(yield func(Revision) bool) {
		n := s.o.Edits + 2
		times, actors := timeline(newRNG(s.o.Seed, "f1h-time"), n, s.o.Start, s.o.Start.Add(s.o.Span), []int{40, 20, 14, 18, 8}, 24)
		kinds := novelSchedule(newRNG(s.o.Seed, "f1h-kinds"), s.o.Edits)
		for i := range n {
			r := Revision{Seq: i + 1, Time: times[i]}
			switch i {
			case 0:
				r.Kind, r.Actor, r.AuthorName, r.AuthorEmail, r.Subject = KindInit, "system", CommitterName, CommitterEmail, "Initialize notes notebook"
				r.Time = r.Time.In(time.UTC)
			case 1:
				r.Kind, r.Subject = KindCreate, "Update "+quotePath(s.path)
				Actors[actorAda].sign(&r, "")
				r.Changes = []Change{{Path: s.path, Content: s.doc.render()}}
			default:
				s.edit(&r, kinds[i-2], actors[i])
			}
			s.git.commit(&r)
			if !yield(r) {
				return
			}
		}
	}
}

func (s *novelSim) edit(r *Revision, kind string, actor int) {
	if kind == KindParagraphInsert {
		count := 0
		for _, b := range s.doc.blocks {
			if isParagraph(b) {
				count++
			}
		}
		if count > s.paragraphs+40 || count >= s.paragraphs-40 && s.r.permille(500) {
			kind = KindParagraphDelete
		}
	}
	a := Actors[actor]
	organize := kind == KindFrontMatter || kind == KindRename
	switch {
	case organize && s.r.permille(400):
		a = Actors[actorOrganizer]
	case kind == KindChapterMove && a.Agent:
		a = Actors[actorAda]
	case !organize && a.Key == "organizer":
		a = Actors[actorAssistant] // the organizer only retags and moves notes
	}
	r.Kind = kind
	old := s.path
	switch kind {
	case KindWordEdit:
		s.wordEdit(s.paragraphNear())
	case KindSentenceEdit:
		s.sentenceEdit(s.paragraphNear())
	case KindParagraphInsert:
		s.insertParagraph()
	case KindParagraphDelete:
		s.deleteParagraph()
	case KindChapterMove:
		s.moveChapter()
	case KindFrontMatter:
		s.frontMatter(a.Key == "organizer")
	case KindRename:
		for s.path == old {
			s.path = pick(s.r, novelPaths)
		}
	}
	content := s.doc.render()
	operation := ""
	if a.Key == "organizer" {
		operation = fmt.Sprintf("organization:f1h:%d", r.Seq)
	}
	switch {
	case kind == KindRename:
		r.Subject = fmt.Sprintf("Move %q to %q", old, s.path)
		a.sign(r, operation)
		r.Changes = []Change{{Path: s.path, OldPath: old, Content: content}}
		return
	case a.Key == "organizer":
		r.Subject = "Organize note: tags"
		a.sign(r, operation)
	case !a.Agent && kind != KindFrontMatter && s.r.permille(400):
		a.signLive(r, s.path, s.git.parent)
	default:
		r.Subject = "Update " + quotePath(s.path)
		a.sign(r, "")
	}
	r.Changes = []Change{{Path: s.path, Content: content}}
}

// paragraphNear picks a paragraph near the previous edit, or anywhere.
func (s *novelSim) paragraphNear() int {
	for {
		i := s.r.between(1, len(s.doc.blocks)-1)
		if s.r.permille(650) {
			i = min(max(s.cursor+s.r.between(-10, 10), 1), len(s.doc.blocks)-1)
		}
		if isParagraph(s.doc.blocks[i]) {
			s.cursor = i
			return i
		}
	}
}

func splitTrailing(word string) (string, string) {
	end := len(word)
	for end > 0 && strings.ContainsRune(".,;:!?", rune(word[end-1])) {
		end--
	}
	return word[:end], word[end:]
}

func (s *novelSim) wordEdit(i int) {
	words := strings.Split(s.doc.blocks[i], " ")
	at := s.r.intn(len(words))
	switch op := s.r.intn(10); {
	case op < 5: // replace, keeping the punctuation that followed the word
		_, punct := splitTrailing(words[at])
		words[at] = pick(s.r, s.words) + punct
	case op < 8 || len(words) < 4: // insert
		words = slices.Insert(words, at+1, pick(s.r, s.words))
	default: // delete, moving its punctuation to the previous word
		_, punct := splitTrailing(words[at])
		words = slices.Delete(words, at, at+1)
		if at > 0 && punct != "" {
			base, _ := splitTrailing(words[at-1])
			words[at-1] = base + punct
		}
	}
	s.doc.blocks[i] = strings.Join(words, " ")
}

func (s *novelSim) sentenceEdit(i int) {
	sentences := splitSentences(s.doc.blocks[i])
	at := s.r.intn(len(sentences))
	switch op := s.r.intn(3); {
	case op == 0 || len(sentences) < 2: // insert
		sentences = slices.Insert(sentences, s.r.intn(len(sentences)+1), pick(s.r, s.sentences))
	case op == 1: // rewrite
		sentences[at] = pick(s.r, s.sentences)
	default:
		sentences = slices.Delete(sentences, at, at+1)
	}
	s.doc.blocks[i] = strings.Join(sentences, " ")
}

func (s *novelSim) newParagraph() string {
	sentences := make([]string, s.r.between(2, 5))
	for i := range sentences {
		sentences[i] = pick(s.r, s.sentences)
	}
	return strings.Join(sentences, " ")
}

func (s *novelSim) insertParagraph() {
	first := s.doc.sections()[0]
	at := s.r.between(first+1, len(s.doc.blocks))
	if s.r.permille(650) {
		at = min(max(s.cursor+s.r.between(-10, 10), first+1), len(s.doc.blocks))
	}
	s.doc.blocks = slices.Insert(s.doc.blocks, at, s.newParagraph())
	s.cursor = at
}

func (s *novelSim) deleteParagraph() {
	for range 1000 {
		i := s.paragraphNear()
		// Keep at least one paragraph under every heading.
		alone := !isParagraph(s.doc.blocks[i-1]) && (i+1 == len(s.doc.blocks) || !isParagraph(s.doc.blocks[i+1]))
		if !alone {
			s.doc.blocks = slices.Delete(s.doc.blocks, i, i+1)
			s.cursor = max(i-1, 1)
			return
		}
	}
	s.insertParagraph() // every chapter is down to one paragraph
}

func (s *novelSim) moveChapter() {
	starts := s.doc.sections()
	from := s.r.intn(len(starts))
	end := len(s.doc.blocks)
	if from+1 < len(starts) {
		end = starts[from+1]
	}
	chapter := slices.Clone(s.doc.blocks[starts[from]:end])
	s.doc.blocks = slices.Delete(s.doc.blocks, starts[from], end)
	starts = s.doc.sections()
	to := from
	for to == from {
		to = s.r.intn(len(starts) + 1)
	}
	at := len(s.doc.blocks)
	if to < len(starts) {
		at = starts[to]
	}
	s.doc.blocks = slices.Insert(s.doc.blocks, at, chapter...)
	s.cursor = at
}

var (
	novelStatuses = []string{"draft", "revising", "copyedit", "final"}
	novelTags     = []string{"novel", "whaling", "manuscript", "melville", "sea", "classic"}
	novelTitles   = []string{"Moby-Dick; or, The Whale", "Moby-Dick", "The Whale", "Moby-Dick; or, The Whale (revised)"}
)

func (s *novelSim) setField(key, value string) {
	for i := range s.doc.front {
		if s.doc.front[i].key == key {
			if value == "" {
				s.doc.front = slices.Delete(s.doc.front, i, i+1)
			} else {
				s.doc.front[i].value = value
			}
			return
		}
	}
	if value != "" {
		s.doc.front = append(s.doc.front, field{key, value})
	}
}

func (s *novelSim) fieldValue(key string) string {
	for _, f := range s.doc.front {
		if f.key == key {
			return f.value
		}
	}
	return ""
}

// frontMatter changes the status, tags or title; the organizer only retags.
func (s *novelSim) frontMatter(tagsOnly bool) {
	switch op := s.r.intn(10); {
	case op < 5 && !tagsOnly:
		current := s.fieldValue("status")
		next := current
		for next == current {
			next = pick(s.r, novelStatuses)
		}
		s.setField("status", next)
	case op < 9 || tagsOnly:
		tags := strings.Split(strings.Trim(s.fieldValue("tags"), "[]"), ", ")
		if tags[0] == "" {
			tags = nil
		}
		tag := pick(s.r, novelTags)
		if i := slices.Index(tags, tag); i >= 0 {
			tags = slices.Delete(tags, i, i+1)
		} else {
			tags = append(tags, tag)
		}
		value := ""
		if len(tags) > 0 {
			value = "[" + strings.Join(tags, ", ") + "]"
		}
		s.setField("tags", value)
	default:
		current := s.fieldValue("title")
		next := current
		for next == current {
			next = pick(s.r, novelTitles)
		}
		s.setField("title", next)
	}
}
