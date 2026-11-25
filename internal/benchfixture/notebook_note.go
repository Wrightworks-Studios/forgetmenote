package benchfixture

import (
	"net/url"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

func (s *notebookSim) uniqueTitle(base string) string {
	title := base
	for i := 2; s.titles[strings.ToLower(title)]; i++ {
		title = base + " " + strconv.Itoa(i)
	}
	s.titles[strings.ToLower(title)] = true
	return title
}

func (s *notebookSim) createNote(r *Revision, a Actor) bool {
	for range 16 {
		if s.tryCreateNote(r, a) {
			return true
		}
	}
	return false
}

func (s *notebookSim) tryCreateNote(r *Revision, a Actor) bool {
	folder := pick(s.r, nbFolders)
	if a.Agent {
		folder = "Meetings"
	}
	n := &nbNote{id: s.r.hexID(16), kind: "note", titleField: s.r.permille(700)}
	date := s.now.UTC().Format("2006-01-02")
	switch {
	case folder == "Meetings":
		n.kind = "meeting"
		n.title = s.uniqueTitle(date + " " + titleText(s.r, s.r.between(1, 3)) + " sync")
	case folder == "Journal":
		n.kind = "journal"
		n.title = s.uniqueTitle(date + " Journal")
	case strings.HasPrefix(folder, "Projects/"):
		n.kind = "project"
		n.title = s.uniqueTitle(titleText(s.r, s.r.between(2, 4)))
	case strings.HasPrefix(folder, "Reference/"):
		n.kind = "reference"
		n.title = s.uniqueTitle(titleText(s.r, s.r.between(1, 3)))
	default:
		n.title = s.uniqueTitle(titleText(s.r, s.r.between(2, 4)))
	}
	n.path = strings.TrimPrefix(folder+"/"+n.title+".md", "/")
	if !s.editable(n.path) || s.occupied(n.path) {
		n.path = "Inbox/" + n.title + ".md"
		if !s.editable(n.path) || s.occupied(n.path) {
			return false
		}
	}
	n.status = pick(s.r, nbStatuses)
	for range s.r.between(0, 3) {
		if t := pick(s.r, nbTags); !slices.Contains(n.tags, t) {
			n.tags = append(n.tags, t)
		}
	}
	// Sizes spread over the range, most notes small.
	lo, hi := s.o.MinNoteBytes, s.o.MaxNoteBytes
	small, medium := lo*5/2, min(lo*5, hi-1500)
	target := []int{s.r.between(lo+300, small), s.r.between(small, medium), s.r.between(medium, hi-1500)}[s.r.weighted([]int{40, 35, 25})]
	n.blocks = []nbBlock{{kind: 'p', text: prose(s.r, s.r.between(2, 4))}, {kind: 'p', text: prose(s.r, s.r.between(2, 5))}}
	dir := path.Dir(n.path)
	for size := len(n.render()); size < target; {
		b := s.newBlock(n)
		if size += len(n.renderBlock(b, dir, false)) + 2; size > hi-1000 {
			break
		}
		n.blocks = append(n.blocks, b)
	}
	s.live = append(s.live, n)
	s.all = append(s.all, n)
	s.writeNote(r, n, "")
	s.sign(r, a, "Update "+quotePath(n.path), false)
	return true
}

// newBlock returns a block of the shape notes are written in.
func (s *notebookSim) newBlock(n *nbNote) nbBlock {
	switch s.r.weighted([]int{55, 10, 10, 6, 6, 4, 4, 5}) {
	case 0:
		return nbBlock{kind: 'p', text: s.linkedProse(n)}
	case 1:
		return nbBlock{kind: 'h', text: titleText(s.r, s.r.between(1, 4))}
	case 2:
		b := nbBlock{kind: 'l'}
		for range s.r.between(2, 6) {
			b.items = append(b.items, strings.TrimSuffix(sentence(s.r), "."))
		}
		return b
	case 3:
		b := nbBlock{kind: 't'}
		for range s.r.between(2, 5) {
			b.items = append(b.items, capitalize(word(s.r))+" "+strings.TrimSuffix(strings.ToLower(sentence(s.r)), "."))
			b.done = append(b.done, s.r.permille(400))
		}
		return b
	case 4:
		return nbBlock{kind: 'q', text: prose(s.r, s.r.between(1, 2))}
	case 5:
		lines := make([]string, s.r.between(1, 4))
		for i := range lines {
			lines[i] = word(s.r) + "(" + word(s.r) + ", " + strconv.Itoa(s.r.intn(100)) + ")"
		}
		return nbBlock{kind: 'c', text: strings.Join(lines, "\n"), items: []string{pick(s.r, []string{"text", "sh", "python", "json"})}}
	case 6:
		b := nbBlock{kind: 'T'}
		for range s.r.between(2, 5) {
			b.items = append(b.items, capitalize(word(s.r))+"\x00"+pick(s.r, Actors[:3]).Subject+"\x00"+pick(s.r, nbStatuses[1:]))
		}
		return b
	default:
		if len(s.assets) == 0 {
			return nbBlock{kind: 'p', text: prose(s.r, 2)}
		}
		return nbBlock{kind: 'i', asset: pick(s.r, s.assets).path, text: titleText(s.r, 2)}
	}
}

func (n *nbNote) addLink(l nbLink) string {
	n.links = append(n.links, l)
	return "\x00" + strconv.Itoa(len(n.links)-1) + "\x00"
}

// withLink inserts a link to another live note by title or by relative path.
func (s *notebookSim) withLink(n *nbNote, text string) string {
	if len(s.live) == 0 {
		return text
	}
	other := pick(s.r, s.live)
	if other == n {
		return text
	}
	var token string
	switch s.r.intn(4) {
	case 0, 1:
		token = n.addLink(nbLink{wiki: true, target: other.title})
	case 2:
		token = n.addLink(nbLink{wiki: true, target: other.title, label: strings.ToLower(word(s.r))})
	default:
		token = n.addLink(nbLink{target: other.path, label: strings.ToLower(titleText(s.r, 1))})
	}
	words := strings.Split(text, " ")
	at := s.r.between(1, len(words)-1)
	return strings.Join(slices.Insert(words, at, token), " ")
}

// linkedProse is a paragraph that sometimes links another note.
func (s *notebookSim) linkedProse(n *nbNote) string {
	text := prose(s.r, s.r.between(2, 6))
	if s.r.permille(350) {
		text = s.withLink(n, text)
	}
	return text
}

func relLink(dir, target string) string {
	rel, err := filepath.Rel(dir, target)
	if err != nil {
		panic(err)
	}
	return (&url.URL{Path: filepath.ToSlash(rel)}).String()
}

func (n *nbNote) renderText(text, dir string, visible bool) string {
	if !strings.Contains(text, "\x00") {
		return text
	}
	var b strings.Builder
	for {
		i := strings.IndexByte(text, 0)
		if i < 0 {
			b.WriteString(text)
			return b.String()
		}
		b.WriteString(text[:i])
		j := i + 1 + strings.IndexByte(text[i+1:], 0)
		k, _ := strconv.Atoi(text[i+1 : j])
		l := n.links[k]
		switch {
		case l.wiki && l.label == "":
			b.WriteString("[[" + l.target + "]]")
		case l.wiki:
			b.WriteString("[[" + l.target + "|" + l.label + "]]")
		case visible:
			b.WriteString(l.label)
		default:
			b.WriteString("[" + l.label + "](" + relLink(dir, l.target) + ")")
		}
		text = text[j+1:]
	}
}

func (n *nbNote) renderBlock(b nbBlock, dir string, visible bool) string {
	var out []string
	switch b.kind {
	case 'p':
		return n.renderText(b.text, dir, visible)
	case 'h':
		if visible {
			return b.text
		}
		return "## " + b.text
	case 'q':
		if visible {
			return b.text
		}
		return "> " + b.text
	case 'l':
		for _, item := range b.items {
			out = append(out, "- "+item)
		}
	case 't':
		for i, item := range b.items {
			mark := "- [ ] "
			if b.done[i] {
				mark = "- [x] "
			}
			out = append(out, mark+item)
		}
	case 'c':
		if visible {
			return b.text
		}
		return "```" + b.items[0] + "\n" + b.text + "\n```"
	case 'T':
		out = append(out, "| Item | Owner | Status |", "| --- | --- | --- |")
		for _, row := range b.items {
			out = append(out, "| "+strings.ReplaceAll(row, "\x00", " | ")+" |")
		}
	case 'i':
		if visible {
			return ""
		}
		return "![" + b.text + "](" + relLink(dir, b.asset) + ")"
	}
	if visible {
		return strings.Join(out, " ")
	}
	return strings.Join(out, "\n")
}

func (n *nbNote) render() []byte {
	var b strings.Builder
	b.WriteString("---\nfmn-id: " + n.id + "\n")
	if n.titleField {
		b.WriteString("title: " + n.title + "\n")
	}
	b.WriteString("kind: " + n.kind + "\n")
	if n.status != "" {
		b.WriteString("status: " + n.status + "\n")
	}
	if len(n.tags) > 0 {
		b.WriteString("tags: [" + strings.Join(n.tags, ", ") + "]\n")
	}
	b.WriteString("---\n\n# " + n.title + "\n")
	dir := path.Dir(n.path)
	for _, block := range n.blocks {
		b.WriteString("\n" + n.renderBlock(block, dir, false) + "\n")
	}
	return []byte(b.String())
}

// visibleText approximates the rendered text comment anchors match against
// (workspace commentText): block texts separated by spaces, collapsed.
func (n *nbNote) visibleText() string {
	parts := []string{n.title}
	for _, block := range n.blocks {
		parts = append(parts, n.renderBlock(block, ".", true))
	}
	return strings.Join(strings.Fields(strings.Join(parts, " ")), " ")
}

func (n *nbNote) paragraphs() []int {
	var out []int
	for i, b := range n.blocks {
		if b.kind == 'p' {
			out = append(out, i)
		}
	}
	return out
}

func (s *notebookSim) editNote(r *Revision, a Actor) bool {
	n := s.pickNote()
	if n == nil {
		return false
	}
	lo, hi := s.o.MinNoteBytes+400, s.o.MaxNoteBytes-600
	for range 20 {
		blocks, links := cloneBlocks(n.blocks), slices.Clone(n.links)
		size := len(n.content)
		op := s.r.weighted([]int{40, 18, 12, 10, 6, 4, 3, 3, 2, 2})
		grow := []int{2, 3, 6, 7, 8}
		shrink := []int{0, 1, 2}
		if size > hi && !slices.Contains(shrink, op) {
			op = pick(s.r, shrink)
		} else if size < lo && !slices.Contains(grow, op) {
			op = pick(s.r, grow)
		}
		s.applyEdit(n, op, size > hi, size < lo)
		if content := n.render(); string(content) != string(n.content) && len(content) >= s.o.MinNoteBytes+100 && len(content) <= s.o.MaxNoteBytes-200 {
			s.writeNote(r, n, "")
			s.sign(r, a, "Update "+quotePath(n.path), true)
			return true
		}
		n.blocks, n.links = blocks, links // discard the rejected edit
	}
	return false
}

func cloneBlocks(blocks []nbBlock) []nbBlock {
	out := slices.Clone(blocks)
	for i := range out {
		out[i].items, out[i].done = slices.Clone(out[i].items), slices.Clone(out[i].done)
	}
	return out
}

func (s *notebookSim) applyEdit(n *nbNote, op int, shrinkOnly, growOnly bool) {
	paras := n.paragraphs()
	p := &n.blocks[pick(s.r, paras)]
	switch op {
	case 0: // word
		words := strings.Split(p.text, " ")
		at := s.r.intn(len(words))
		if strings.Contains(words[at], "\x00") {
			return
		}
		_, punct := splitTrailing(words[at])
		switch {
		case shrinkOnly || !growOnly && s.r.permille(250) && len(words) > 6:
			words = slices.Delete(words, at, at+1)
			if at > 0 && punct != "" && !strings.Contains(words[at-1], "\x00") {
				base, _ := splitTrailing(words[at-1])
				words[at-1] = base + punct
			} else if at == 0 && len(words) > 0 && !strings.Contains(words[0], "\x00") {
				words[0] = capitalize(words[0])
			}
		case growOnly || s.r.permille(400):
			words = slices.Insert(words, at+1, word(s.r))
		default:
			w := word(s.r)
			if at == 0 {
				w = capitalize(w)
			}
			words[at] = w + punct
		}
		p.text = strings.Join(words, " ")
	case 1: // sentence
		sentences := splitSentences(p.text)
		at := s.r.intn(len(sentences))
		switch {
		case shrinkOnly && len(sentences) > 1 || !growOnly && len(sentences) > 2 && s.r.permille(300):
			if !strings.Contains(sentences[at], "\x00") {
				sentences = slices.Delete(sentences, at, at+1)
			}
		case growOnly || s.r.permille(500):
			sentences = slices.Insert(sentences, s.r.intn(len(sentences)+1), sentence(s.r))
		default:
			if !strings.Contains(sentences[at], "\x00") {
				sentences[at] = sentence(s.r)
			}
		}
		p.text = strings.Join(sentences, " ")
	case 2: // paragraph
		at := pick(s.r, paras)
		if !growOnly && (shrinkOnly || s.r.permille(450)) && len(paras) > 2 {
			n.blocks = slices.Delete(n.blocks, at, at+1)
		} else {
			n.blocks = slices.Insert(n.blocks, s.r.between(0, len(n.blocks)), nbBlock{kind: 'p', text: s.linkedProse(n)})
		}
	case 3: // list items and tasks
		for _, i := range s.r.permutation(len(n.blocks)) {
			b := &n.blocks[i]
			if b.kind == 't' && s.r.permille(600) {
				j := s.r.intn(len(b.done))
				b.done[j] = !b.done[j]
				return
			}
			if b.kind == 'l' || b.kind == 't' {
				b.items = append(b.items, strings.TrimSuffix(sentence(s.r), "."))
				if b.kind == 't' {
					b.done = append(b.done, false)
				}
				return
			}
		}
		n.blocks = append(n.blocks, s.newBlock(n))
	case 4, 5: // links
		p.text = s.withLink(n, p.text)
	case 6: // image
		if len(s.assets) > 0 {
			n.blocks = slices.Insert(n.blocks, s.r.between(0, len(n.blocks)), nbBlock{kind: 'i', asset: pick(s.r, s.assets).path, text: titleText(s.r, 2)})
		}
	case 7: // heading
		n.blocks = slices.Insert(n.blocks, s.r.between(0, len(n.blocks)), nbBlock{kind: 'h', text: titleText(s.r, s.r.between(1, 4))})
	case 8: // code or table
		b := s.newBlock(n)
		for b.kind != 'c' && b.kind != 'T' {
			b = s.newBlock(n)
		}
		n.blocks = slices.Insert(n.blocks, s.r.between(0, len(n.blocks)), b)
	case 9: // drop a link
		words := strings.Split(p.text, " ")
		if i := slices.IndexFunc(words, func(w string) bool { return strings.Contains(w, "\x00") }); i >= 0 && len(words) > 4 {
			p.text = strings.Join(slices.Delete(words, i, i+1), " ")
		}
	}
}

func (s *notebookSim) editFrontMatter(r *Revision, a Actor) bool {
	n := s.pickNote()
	if n == nil {
		return false
	}
	before := string(n.content)
	for string(n.render()) == before {
		switch op := s.r.intn(10); {
		case op < 6 || a.Key == "organizer":
			t := pick(s.r, nbTags)
			if i := slices.Index(n.tags, t); i >= 0 {
				n.tags = slices.Delete(n.tags, i, i+1)
			} else {
				n.tags = append(n.tags, t)
			}
		case op < 9:
			n.status = pick(s.r, nbStatuses)
		default:
			n.titleField = !n.titleField
		}
	}
	s.writeNote(r, n, "")
	subject := "Update " + quotePath(n.path)
	if a.Key == "organizer" {
		subject = "Organize note: tags"
	}
	s.sign(r, a, subject, false)
	return true
}
