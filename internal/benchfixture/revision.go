package benchfixture

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The committer of every ForgetMeNote commit, as internal/workspace configures
// Git and internal/notestore writes commits: the author's time, this identity.
const (
	CommitterName  = "ForgetMeNote"
	CommitterEmail = "forgetmenote@localhost"
)

// Revision kinds. KindInit is the empty commit every notebook starts with.
const (
	KindInit = "init"
	// F1h
	KindCreate          = "create"
	KindWordEdit        = "word-edit"
	KindSentenceEdit    = "sentence-edit"
	KindParagraphInsert = "paragraph-insert"
	KindParagraphDelete = "paragraph-delete"
	KindChapterMove     = "chapter-move"
	KindFrontMatter     = "front-matter"
	KindRename          = "rename"
	// F2
	KindNoteCreate     = "note-create"
	KindNoteEdit       = "note-edit"
	KindAssetAdd       = "asset-add"
	KindAssetReplace   = "asset-replace"
	KindComment        = "comment"
	KindCommentReply   = "comment-reply"
	KindCommentResolve = "comment-resolve"
	KindMove           = "move"
	KindLock           = "lock"
	KindUnlock         = "unlock"
	KindTrash          = "trash"
	KindRestore        = "restore"
	KindEmptyTrash     = "empty-trash"
)

// Trailer is one "Key: Value" line of a commit message's final trailer block.
type Trailer struct{ Key, Value string }

// Change writes or deletes one file. OldPath names a rename source that the
// change removes; Content nil deletes Path.
type Change struct {
	Path    string
	OldPath string
	Content []byte
}

// Revision is one commit any store can replay: apply Changes in order, author
// it as AuthorName/AuthorEmail at Time (committer CommitterName at the same
// time) with Message(). Commit is the SHA-1 Git assigns to exactly that commit
// on top of the previous revision, which the Git materializer verifies.
type Revision struct {
	Seq         int    // 1-based position in the stream
	Kind        string // one of the Kind constants; ground truth for history benchmarks
	Actor       string // key of the simulated actor (see Actors)
	AuthorName  string
	AuthorEmail string
	Time        time.Time // author and committer time, in the author's zone
	Subject     string
	Trailers    []Trailer
	Changes     []Change
	Commit      string
}

// Message is the commit message Git stores for Subject and Trailers.
func (r Revision) Message() string {
	var b strings.Builder
	b.WriteString(r.Subject)
	b.WriteByte('\n')
	if len(r.Trailers) > 0 {
		b.WriteByte('\n')
		for _, t := range r.Trailers {
			b.WriteString(t.Key + ": " + t.Value + "\n")
		}
	}
	return b.String()
}

func gitDate(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) + " " + t.Format("-0700") }

// gitState tracks the tree of a replayed stream and predicts each commit ID
// with Git's object encoding, so generators can reference earlier revisions
// (Trash history, comment anchors, live receipts) the way the app records them.
type gitState struct {
	root   *treeNode
	parent string
}

type treeNode struct {
	children map[string]*treeNode // nil for a file
	blob     [20]byte
	id       [20]byte
	dirty    bool
}

func newGitState() *gitState {
	return &gitState{root: &treeNode{children: map[string]*treeNode{}, dirty: true}}
}

func gitHash(kind string, body []byte) [20]byte {
	h := sha1.New()
	h.Write([]byte(kind + " " + strconv.Itoa(len(body)) + "\x00"))
	h.Write(body)
	var id [20]byte
	h.Sum(id[:0])
	return id
}

func (n *treeNode) set(parts []string, blob [20]byte) {
	n.dirty = true
	child := n.children[parts[0]]
	if len(parts) == 1 {
		if child != nil && child.children != nil {
			panic("benchfixture: file replaces directory " + parts[0])
		}
		n.children[parts[0]] = &treeNode{blob: blob}
		return
	}
	if child == nil {
		child = &treeNode{children: map[string]*treeNode{}}
		n.children[parts[0]] = child
	} else if child.children == nil {
		panic("benchfixture: directory replaces file " + parts[0])
	}
	child.set(parts[1:], blob)
}

func (n *treeNode) remove(parts []string) {
	child := n.children[parts[0]]
	if child == nil {
		panic("benchfixture: delete of missing path " + strings.Join(parts, "/"))
	}
	n.dirty = true
	if len(parts) == 1 {
		delete(n.children, parts[0])
		return
	}
	child.remove(parts[1:])
	if len(child.children) == 0 {
		delete(n.children, parts[0]) // Git has no empty trees.
	}
}

func (n *treeNode) hash() [20]byte {
	if !n.dirty {
		return n.id
	}
	names := make([]string, 0, len(n.children))
	for name := range n.children {
		names = append(names, name)
	}
	key := func(name string) string {
		if n.children[name].children != nil {
			return name + "/"
		}
		return name
	}
	slices.SortFunc(names, func(a, b string) int { return strings.Compare(key(a), key(b)) })
	var body []byte
	for _, name := range names {
		child := n.children[name]
		mode, id := "100644", child.blob
		if child.children != nil {
			mode, id = "40000", child.hash()
		}
		body = append(body, mode+" "+name+"\x00"...)
		body = append(body, id[:]...)
	}
	n.id, n.dirty = gitHash("tree", body), false
	return n.id
}

// commit applies r's changes and sets r.Commit.
func (g *gitState) commit(r *Revision) {
	for _, c := range r.Changes {
		if c.OldPath != "" {
			g.root.remove(strings.Split(c.OldPath, "/"))
		}
		if c.Content == nil {
			g.root.remove(strings.Split(c.Path, "/"))
		} else {
			g.root.set(strings.Split(c.Path, "/"), gitHash("blob", c.Content))
		}
	}
	tree := g.root.hash()
	var b strings.Builder
	b.WriteString("tree " + hex.EncodeToString(tree[:]) + "\n")
	if g.parent != "" {
		b.WriteString("parent " + g.parent + "\n")
	}
	date := gitDate(r.Time)
	fmt.Fprintf(&b, "author %s <%s> %s\ncommitter %s <%s> %s\n\n", r.AuthorName, r.AuthorEmail, date, CommitterName, CommitterEmail, date)
	b.WriteString(r.Message())
	id := gitHash("commit", []byte(b.String()))
	r.Commit = hex.EncodeToString(id[:])
	g.parent = r.Commit
}
