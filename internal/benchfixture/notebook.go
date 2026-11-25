package benchfixture

import (
	"errors"
	"fmt"
	"iter"
	"slices"
	"strings"
	"time"
)

// NotebookOptions configures F2. Zero fields take the benchmark defaults.
type NotebookOptions struct {
	Seed          uint64
	Notes         int // live notes at the end; notes in Trash come on top
	Assets        int // PNG images under assets/
	Revisions     int // total commits, the init commit included
	Start         time.Time
	Months        int
	MinNoteBytes  int
	MaxNoteBytes  int
	MinAssetBytes int
	MaxAssetBytes int
}

// F2's benchmark shape.
const (
	DefaultNotebookNotes     = 2000
	DefaultNotebookAssets    = 200
	DefaultNotebookRevisions = 20000
)

func (o *NotebookOptions) defaults() error {
	set := func(v *int, d int) {
		if *v == 0 {
			*v = d
		}
	}
	set(&o.Notes, DefaultNotebookNotes)
	set(&o.Assets, DefaultNotebookAssets)
	set(&o.Revisions, DefaultNotebookRevisions)
	set(&o.Months, 18)
	set(&o.MinNoteBytes, 2<<10)
	set(&o.MaxNoteBytes, 20<<10)
	set(&o.MinAssetBytes, 1<<20)
	set(&o.MaxAssetBytes, 5<<20)
	if o.Start.IsZero() {
		o.Start = time.Date(2024, 5, 1, 0, 0, 0, 0, time.UTC)
	}
	if o.Notes < 20 || o.Assets < 1 || o.Revisions < 2*(o.Notes+o.Assets) || o.MinNoteBytes < 1024 || o.MaxNoteBytes < 4*o.MinNoteBytes || o.MinAssetBytes < 256 || o.MaxAssetBytes < 2*o.MinAssetBytes {
		return errors.New("benchfixture: notebook options need at least 20 notes, 1 asset, twice as many revisions as notes and assets, and size ranges of 4x (notes) and 2x (assets)")
	}
	return nil
}

// Notebook generates F2: a notebook that grows to o.Notes live notes (plus
// notes in Trash) and o.Assets images over o.Revisions commits spread across
// o.Months months, written by three humans and two agents. Notes carry fmn-ids, front matter, headings,
// lists, tasks, tables, code, wiki links, relative note links and images.
// Comments, locks and Trash are committed in the formats internal/workspace
// reads, through the same transitions the app makes: moves rewrite links and
// carry discussions, Trash records each note's history by commit, restores
// and empties update the index. The sequence can be iterated repeatedly.
func Notebook(o NotebookOptions) (iter.Seq[Revision], error) {
	if err := o.defaults(); err != nil {
		return nil, err
	}
	return func(yield func(Revision) bool) { newNotebookSim(o).run(yield) }, nil
}

type nbLink struct {
	wiki   bool
	target string // note title (wiki) or notebook path (Markdown link)
	label  string
}

type nbBlock struct {
	kind  byte // 'p' paragraph, 'h' heading, 'l' list, 't' tasks, 'c' code, 'i' image, 'q' quote, 'T' table
	text  string
	items []string
	done  []bool
	asset string
}

type noteEvent struct{ path, commit string }

type nbNote struct {
	id, path, title, kind, status string
	titleField                    bool
	tags                          []string
	blocks                        []nbBlock
	links                         []nbLink
	comments                      []comment
	content                       []byte      // as committed
	trash                         *trashEntry // set while in Trash
	events                        []noteEvent // commits that changed this note since it was created or restored
	restored                      map[string][]string
}

type nbAsset struct {
	path           string
	index, version int
}

type notebookSim struct {
	o         NotebookOptions
	r         *rng
	git       *gitState
	all       []*nbNote
	live      []*nbNote
	trashed   map[string]*nbNote // by Trash path
	titles    map[string]bool    // lower-cased titles ever used
	assets    []*nbAsset
	locks     []string
	index     trashIndex
	head      string
	now       time.Time
	touched   []noteEvent
	touchedBy []*nbNote
	quota     map[string]int   // remaining notes and assets to create
	due       map[string][]int // steps at which each occasional kind is next attempted
}

var (
	nbFolders = []string{"Inbox", "Journal", "Meetings",
		"Projects/Atlas", "Projects/Atlas/Research", "Projects/Beacon", "Projects/Beacon/Design", "Projects/Cedar", "Projects/Delta",
		"Projects/Ember", "Projects/Fjord", "Projects/Granite", "Projects/Harbor", "Projects/Harbor/Interviews", "Projects/Iris", "Projects/Juniper",
		"Areas/Hiring", "Areas/Finance", "Areas/Operations", "Areas/Writing", "Areas/Health",
		"Reference/Recipes", "Reference/Books", "Reference/Tools", "Reference/Travel", "Archive/2023", "Archive/2024", ""}
	nbTags     = []string{"planning", "research", "draft", "idea", "todo", "reference", "meeting", "followup", "important", "archive", "personal", "team", "q1", "q2", "q3", "q4"}
	nbStatuses = []string{"", "draft", "active", "review", "done"}
)

func newNotebookSim(o NotebookOptions) *notebookSim {
	s := &notebookSim{o: o, r: newRNG(o.Seed, "f2"), git: newGitState(), trashed: map[string]*nbNote{}, titles: map[string]bool{}, index: trashIndex{Version: 1, Records: []trashRecord{}}}
	r := o.Revisions
	s.quota = map[string]int{KindNoteCreate: o.Notes, KindAssetAdd: o.Assets}
	// Occasional kinds are due at random steps; one that is not possible yet
	// (nothing to restore, a lock in the way) is retried at the next step.
	schedule := newRNG(o.Seed, "f2-schedule")
	s.due = map[string][]int{}
	for _, k := range occasional {
		count, from, to := k.count(r), r/20, r
		switch k.kind {
		case KindTrash:
			to = r * 97 / 100 // leave steps to create the trashed notes' replacements
		case KindRestore:
			from, to = r/5, r*97/100
		case KindEmptyTrash:
			from, to = r/10, r*85/100 // Trash still holds notes at the end
		}
		steps := make([]int, count)
		for i := range steps {
			steps[i] = schedule.between(from, to-1)
		}
		slices.Sort(steps)
		s.due[k.kind] = steps
	}
	return s
}

var occasional = []struct {
	kind  string
	count func(revisions int) int
}{
	{KindMove, func(r int) int { return r * 15 / 1000 }},
	{KindLock, func(r int) int { return max(2, r/500) }},
	{KindUnlock, func(r int) int { return max(2, r/700) }},
	{KindTrash, func(r int) int { return max(3, r/400) }},
	{KindRestore, func(r int) int { return max(2, r/1500) }},
	{KindEmptyTrash, func(r int) int { return max(1, r/4000) }},
	{KindAssetReplace, func(r int) int { return max(1, r/1000) }},
}

func (s *notebookSim) run(yield func(Revision) bool) {
	end := s.o.Start.AddDate(0, s.o.Months, 0)
	times, actors := timeline(newRNG(s.o.Seed, "f2-time"), s.o.Revisions, s.o.Start, end, []int{30, 22, 18, 20, 10}, 16)
	for i := range s.o.Revisions {
		r := Revision{Seq: i + 1, Time: times[i]}
		s.now = times[i]
		if i == 0 {
			r.Kind, r.Actor, r.AuthorName, r.AuthorEmail, r.Subject = KindInit, "system", CommitterName, CommitterEmail, "Initialize notes notebook"
			r.Time = r.Time.In(time.UTC)
		} else {
			s.step(&r, actors[i], i)
		}
		s.git.commit(&r)
		s.committed(r.Commit)
		if !yield(r) {
			return
		}
	}
}

// committed records the notes the last revision changed in their histories.
func (s *notebookSim) committed(commit string) {
	for j, n := range s.touchedBy {
		n.events = append(n.events, noteEvent{s.touched[j].path, commit})
	}
	s.touched, s.touchedBy = s.touched[:0], s.touchedBy[:0]
	s.head = commit
}

// step makes revision i. Notes and assets are created with probability
// remaining/steps left, so they spread evenly and their totals are exact.
func (s *notebookSim) step(r *Revision, actor, i int) {
	x, kind := s.r.intn(s.o.Revisions-i), ""
	switch {
	case x < s.quota[KindNoteCreate] || len(s.live) == 0:
		kind = KindNoteCreate
	case x < s.quota[KindNoteCreate]+s.quota[KindAssetAdd]:
		kind = KindAssetAdd
	}
	if kind != "" {
		if !s.do(r, kind, actor) {
			panic("benchfixture: mandatory " + kind + " failed")
		}
		s.quota[kind]--
		return
	}
	for _, k := range occasional {
		if due := s.due[k.kind]; len(due) > 0 && due[0] <= i && s.do(r, k.kind, actor) {
			s.due[k.kind] = due[1:]
			return
		}
	}
	choices := []string{KindNoteEdit, KindFrontMatter, KindComment, KindCommentReply, KindCommentResolve}
	weights := []int{780, 70, 70, 45, 35}
	for {
		k := choices[s.r.weighted(weights)]
		if s.do(r, k, actor) {
			return
		}
		if k == KindNoteEdit && !s.do(r, KindFrontMatter, actor) {
			panic("benchfixture: no editable note left")
		}
		if r.Kind != "" {
			return
		}
	}
}

// actorFor applies who may do what: the owner manages locks and empties
// Trash, humans upload, trash and resolve, the organizer only organizes.
func actorFor(kind string, actor int) Actor {
	a := Actors[actor]
	switch kind {
	case KindLock, KindUnlock, KindEmptyTrash:
		return Actors[actorAda]
	case KindAssetAdd, KindAssetReplace, KindTrash, KindRestore, KindCommentResolve:
		if a.Agent {
			return Actors[actorAda]
		}
	case KindMove, KindFrontMatter:
	default:
		if a.Key == "organizer" {
			return Actors[actorAssistant]
		}
	}
	return a
}

func (s *notebookSim) do(r *Revision, kind string, actor int) bool {
	a := actorFor(kind, actor)
	ok := false
	switch kind {
	case KindNoteCreate:
		ok = s.createNote(r, a)
	case KindNoteEdit:
		ok = s.editNote(r, a)
	case KindFrontMatter:
		ok = s.editFrontMatter(r, a)
	case KindAssetAdd, KindAssetReplace:
		ok = s.writeAsset(r, a, kind == KindAssetAdd)
	case KindComment, KindCommentReply, KindCommentResolve:
		ok = s.discuss(r, a, kind)
	case KindMove:
		ok = s.move(r, a)
	case KindLock, KindUnlock:
		ok = s.lock(r, a, kind == KindLock)
	case KindTrash:
		ok = s.trashNotes(r, a)
	case KindRestore:
		ok = s.restore(r, a)
	case KindEmptyTrash:
		ok = s.emptyTrash(r, a)
	}
	if ok {
		r.Kind = kind
		slices.SortFunc(r.Changes, func(x, y Change) int { return strings.Compare(x.Path, y.Path) })
	}
	return ok
}

func matchesPrefix(p string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if p == prefix || strings.HasPrefix(p, prefix+"/") {
			return true
		}
	}
	return false
}

func (s *notebookSim) editable(p string) bool {
	return !strings.HasPrefix(p, "Trash/") && !matchesPrefix(p, s.locks)
}

// pickNote returns a random live, editable note.
func (s *notebookSim) pickNote() *nbNote {
	if len(s.live) == 0 {
		return nil
	}
	for range 64 {
		if n := pick(s.r, s.live); s.editable(n.path) {
			return n
		}
	}
	return nil
}

func (s *notebookSim) occupied(p string) bool {
	return slices.ContainsFunc(s.live, func(n *nbNote) bool { return n.path == p })
}

// sign attributes r to a; human content edits are often live checkpoints.
func (s *notebookSim) sign(r *Revision, a Actor, subject string, live bool) {
	switch {
	case live && !a.Agent && s.r.permille(450):
		a.signLive(r, strings.TrimPrefix(subject, "Update "), s.head)
	case a.Key == "organizer":
		r.Subject = subject
		a.sign(r, fmt.Sprintf("organization:f2:%d", r.Seq))
	default:
		r.Subject = subject
		a.sign(r, "")
	}
}

// writeNote renders n at its path into r and records the change in its history.
func (s *notebookSim) writeNote(r *Revision, n *nbNote, oldPath string) {
	n.content = n.render()
	r.Changes = append(r.Changes, Change{Path: n.path, OldPath: oldPath, Content: n.content})
	s.touched, s.touchedBy = append(s.touched, noteEvent{path: n.path}), append(s.touchedBy, n)
}
