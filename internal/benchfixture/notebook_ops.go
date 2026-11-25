package benchfixture

import (
	"fmt"
	"path"
	"slices"
	"strings"
	"time"
)

func (s *notebookSim) assetBytes(a *nbAsset) []byte {
	return pngImage(newRNG(s.o.Seed, fmt.Sprintf("asset/%d/%d", a.index, a.version)), s.o.MinAssetBytes, s.o.MaxAssetBytes)
}

func (s *notebookSim) writeAsset(r *Revision, a Actor, add bool) bool {
	var asset *nbAsset
	if add {
		asset = &nbAsset{index: len(s.assets)}
		asset.path = fmt.Sprintf("assets/images/%s/%s-%04d.png", s.now.UTC().Format("2006/01"), strings.ToLower(pick(s.r, vocabulary[60:])), asset.index+1)
		s.assets = append(s.assets, asset)
	} else {
		if len(s.assets) == 0 {
			return false
		}
		asset = pick(s.r, s.assets)
		asset.version++
	}
	r.Changes = []Change{{Path: asset.path, Content: s.assetBytes(asset)}}
	s.sign(r, a, "Update "+asset.path, false)
	return true
}

func (s *notebookSim) discussionContent(n *nbNote) []byte { return indented(n.comments, false) }

// anchor quotes a passage that occurs once in the rendered note, with context
// from the same text run, as the browser sends it.
func (s *notebookSim) anchor(n *nbNote) *commentAnchor {
	visible, raw := n.visibleText(), string(n.content)
	paras := n.paragraphs()
	for range 20 {
		text := n.blocks[pick(s.r, paras)].text
		runs := strings.Split(text, "\x00")
		run := runs[2*s.r.intn((len(runs)+1)/2)] // text between link tokens
		words := strings.Fields(run)
		if len(words) < 6 {
			continue
		}
		k := s.r.between(3, min(8, len(words)))
		quote := strings.Join(words[s.r.intn(len(words)-k+1):][:k], " ")
		at := strings.Index(run, quote)
		if at < 0 || strings.Count(visible, quote) != 1 || strings.Count(raw, quote) != 1 {
			continue
		}
		return &commentAnchor{Quote: quote, Prefix: run[max(0, at-24):at], Suffix: run[at+len(quote) : min(len(run), at+len(quote)+24)], Revision: s.head}
	}
	return nil
}

func (s *notebookSim) discuss(r *Revision, a Actor, kind string) bool {
	created := s.now.UTC().Format(time.RFC3339)
	var n *nbNote
	if kind == KindComment {
		if n = s.pickNote(); n == nil {
			return false
		}
		c := comment{ID: s.r.hexID(16), Author: a.Subject, Text: prose(s.r, s.r.between(1, 3)), Created: created}
		if s.r.permille(600) {
			c.Anchor = s.anchor(n)
		}
		n.comments = append(n.comments, c)
	} else {
		for range 64 {
			candidate := s.pickNote()
			if candidate != nil && slices.ContainsFunc(candidate.comments, func(c comment) bool { return !c.Resolved }) {
				n = candidate
				break
			}
		}
		if n == nil {
			return false
		}
		var open []int
		for i, c := range n.comments {
			if !c.Resolved {
				open = append(open, i)
			}
		}
		c := &n.comments[pick(s.r, open)]
		if kind == KindCommentReply {
			c.Replies = append(c.Replies, commentReply{ID: s.r.hexID(16), Author: a.Subject, Text: prose(s.r, s.r.between(1, 2)), Created: created})
		} else {
			c.Resolved = true
		}
	}
	p := commentPath(n.path)
	r.Changes = []Change{{Path: p, Content: s.discussionContent(n)}}
	s.sign(r, a, "Update "+p, false)
	return true
}

// relink points n's Markdown links from one path to another; it reports
// whether n's rendering changes, as rewriteLinks would change it.
func (n *nbNote) relink(from, to string) bool {
	changed := false
	for i := range n.links {
		if !n.links[i].wiki && n.links[i].target == from {
			n.links[i].target = to
			changed = true
		}
	}
	return changed && string(n.render()) != string(n.content)
}

func (s *notebookSim) move(r *Revision, a Actor) bool {
	n := s.pickNote()
	return n != nil && s.moveNote(r, a, n)
}

func (s *notebookSim) moveNote(r *Revision, a Actor, n *nbNote) bool {
	from := n.path
	dir, base := path.Dir(from), path.Base(from)
	to := from
	if s.r.permille(300) {
		to = path.Join(dir, strings.TrimSuffix(base, ".md")+" "+strings.ToLower(titleText(s.r, 1))+".md")
	} else if folder := pick(s.r, nbFolders); folder != dir && !(folder == "" && dir == ".") {
		to = strings.TrimPrefix(folder+"/"+base, "/")
	}
	if to == from || !s.editable(to) || s.occupied(to) {
		return false
	}
	// Moves rewrite Markdown links in every note; Trash and locked notes
	// cannot be rewritten, so the app refuses such a move.
	var linkers []*nbNote
	for _, other := range s.all {
		if other == n || other.content == nil || !slices.ContainsFunc(other.links, func(l nbLink) bool { return !l.wiki && l.target == from }) {
			continue
		}
		if other.trash == nil && !slices.Contains(s.live, other) {
			continue // emptied from Trash
		}
		saved := slices.Clone(other.links)
		changed := other.relink(from, to)
		other.links = saved
		if changed {
			if other.trash != nil || !s.editable(other.path) {
				return false
			}
			linkers = append(linkers, other)
		}
	}
	n.path = to
	s.writeNote(r, n, from)
	for _, other := range linkers {
		other.relink(from, to)
		s.writeNote(r, other, "")
	}
	if len(n.comments) > 0 {
		r.Changes = append(r.Changes, Change{Path: commentPath(to), OldPath: commentPath(from), Content: s.discussionContent(n)})
	}
	s.sign(r, a, fmt.Sprintf("Move %q to %q", from, to), false)
	return true
}

func (s *notebookSim) lockFile() Change {
	return Change{Path: locksPath, Content: indented(lockPolicy{1, slices.Clone(s.locks)}, true)}
}

func (s *notebookSim) lock(r *Revision, a Actor, lock bool) bool {
	if lock {
		if len(s.locks) >= 6 {
			return false
		}
		target := pick(s.r, nbFolders[3:len(nbFolders)-1])
		if s.r.permille(400) {
			n := s.pickNote()
			if n == nil {
				return false
			}
			target = n.path
		}
		if matchesPrefix(target, s.locks) {
			return false
		}
		covered := 0
		for _, n := range s.live {
			if matchesPrefix(n.path, append(slices.Clone(s.locks), target)) {
				covered++
			}
		}
		if covered*10 > len(s.live)*3 {
			return false // keep most of the notebook editable
		}
		s.locks = append(s.locks, target)
		slices.Sort(s.locks)
	} else {
		if len(s.locks) == 0 {
			return false
		}
		i := s.r.intn(len(s.locks))
		s.locks = slices.Delete(s.locks, i, i+1)
	}
	r.Changes = []Change{s.lockFile()}
	s.sign(r, a, "Update "+locksPath, false)
	return true
}

func (s *notebookSim) trashFile() Change {
	return Change{Path: trashIndexPath, Content: indented(s.index, true)}
}

// history is what workspace.trashHistory finds for n at the current head:
// every commit that changed n under each name it had, newest first, and the
// history recorded when it was trashed before.
func (n *nbNote) history() map[string][]string {
	history, seen := map[string][]string{}, map[string]bool{}
	add := func(p, commit string) {
		if !seen[p+"\x00"+commit] {
			seen[p+"\x00"+commit] = true
			history[p] = append(history[p], commit)
		}
	}
	for i := len(n.events) - 1; i >= 0; i-- {
		add(n.events[i].path, n.events[i].commit)
	}
	for _, p := range slices.Sorted(func(yield func(string) bool) {
		for p := range n.restored {
			if !yield(p) {
				return
			}
		}
	}) {
		for _, commit := range n.restored[p] {
			add(p, commit)
		}
	}
	return history
}

func (s *notebookSim) trashNotes(r *Revision, a Actor) bool {
	count := s.r.between(1, 3)
	var notes []*nbNote
	for range 32 {
		if len(notes) == count || len(s.live)-len(notes) <= 10 {
			break
		}
		if n := s.pickNote(); n != nil && !slices.Contains(notes, n) {
			notes = append(notes, n)
		}
	}
	if len(notes) == 0 {
		return false
	}
	// The notebook ends with o.Notes live notes: trashed ones are replaced.
	s.quota[KindNoteCreate] += len(notes)
	for _, n := range notes {
		entry := trashEntry{ID: s.r.hexID(16), OriginalPath: n.path, Title: n.title, DeletedAt: s.now.UTC().Format(time.RFC3339), DeletedBy: a.Subject}
		entry.Path = "Trash/" + entry.ID + ".md"
		s.index.Records = append(s.index.Records, trashRecord{trashEntry: entry, History: n.history()})
		from := n.path
		n.path, n.trash = entry.Path, &entry
		n.content = n.render()
		r.Changes = append(r.Changes, Change{Path: entry.Path, OldPath: from, Content: n.content})
		if len(n.comments) > 0 {
			r.Changes = append(r.Changes, Change{Path: commentPath(entry.Path), OldPath: commentPath(from), Content: s.discussionContent(n)})
		}
		s.live = slices.DeleteFunc(s.live, func(x *nbNote) bool { return x == n })
		s.trashed[entry.Path] = n
		n.events, n.restored = nil, nil
	}
	r.Changes = append(r.Changes, s.trashFile())
	s.sign(r, a, "Move notes to Trash", false)
	return true
}

func (s *notebookSim) restore(r *Revision, a Actor) bool {
	var candidates []int
	for i, rec := range s.index.Records {
		if !rec.Emptied && !rec.Restored && s.editable(rec.OriginalPath) && !s.occupied(rec.OriginalPath) {
			candidates = append(candidates, i)
		}
	}
	if len(candidates) == 0 || s.quota[KindNoteCreate] == 0 {
		return false
	}
	s.quota[KindNoteCreate]--
	i := pick(s.r, candidates)
	rec := &s.index.Records[i]
	n := s.trashed[rec.Path]
	delete(s.trashed, rec.Path)
	rec.Restored = true
	n.path, n.trash = rec.OriginalPath, nil
	s.live = append(s.live, n)
	n.restored = rec.History
	s.writeNote(r, n, rec.Path)
	if len(n.comments) > 0 {
		r.Changes = append(r.Changes, Change{Path: commentPath(n.path), OldPath: commentPath(rec.Path), Content: s.discussionContent(n)})
	}
	r.Changes = append(r.Changes, s.trashFile())
	s.sign(r, a, "Restore note from Trash", false)
	return true
}

func (s *notebookSim) emptyTrash(r *Revision, a Actor) bool {
	var pending []int
	for i, rec := range s.index.Records {
		if !rec.Emptied && !rec.Restored {
			if !s.editable(rec.OriginalPath) {
				return false
			}
			pending = append(pending, i)
		}
	}
	if len(pending) < 2 {
		return false
	}
	for _, i := range pending {
		rec := &s.index.Records[i]
		n := s.trashed[rec.Path]
		delete(s.trashed, rec.Path)
		rec.Emptied = true
		r.Changes = append(r.Changes, Change{Path: rec.Path})
		if len(n.comments) > 0 {
			r.Changes = append(r.Changes, Change{Path: commentPath(rec.Path)})
		}
		n.trash = nil
	}
	r.Changes = append(r.Changes, s.trashFile())
	s.sign(r, a, "Empty Trash", false)
	return true
}
