package workspace

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"forgetmenote/internal/attribution"
)

// Group E: archived sources, project instructions and review findings for
// notebooks kept in the notes store. Each method answers what its Git
// original answers.

// ImportSource commits the original bytes, the provenance and the extracted
// text as one attributed revision, or none of them.
func (n *dbNotebook) ImportSource(ctx context.Context, capture SourceCapture, target, expected, title string, folder *string) (Document, error) {
	ctx = n.mutationContext(ctx)
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(capture.Original) == 0 || len(capture.Original) > MaxSourceBytes() || len(capture.Text) > MaxSourceText() || !utf8.ValidString(capture.Text) || strings.ContainsRune(capture.Text, 0) {
		return Document{}, invalidInput("source exceeds limits or extracted text is invalid")
	}
	if capture.OperationID != "" {
		if len(capture.OperationID) > 160 || capture.Record.ID == "" {
			return Document{}, invalidInput("source operation requires a stable capture ID")
		}
		raw, err := json.Marshal([]any{capture, target, expected, title, folder})
		if err != nil {
			return Document{}, err
		}
		op := operationIdentity{ID: ContentHash([]byte(attribution.From(ctx).ID + "\x00source:" + capture.OperationID)), Request: ContentHash(raw)}
		receipt, err := n.operationReceipt(ctx, op)
		if err != nil {
			return Document{}, err
		}
		if receipt.Replayed {
			destination := target
			if destination == "" {
				root := "sources"
				if folder != nil {
					root = *folder
				}
				destination = path.Join(root, capture.Record.ID+".md")
			}
			if err = n.resolveAtHead(ctx, destination); err != nil {
				return Document{}, err
			}
			return n.read(ctx, destination, receipt.Revision)
		}
		ctx = context.WithValue(ctx, operationKey{}, op)
	}
	if !SourceURL(capture.Record.URL) || !SourceURL(capture.Record.FinalURL) {
		return Document{}, invalidInput("invalid source URL")
	}
	if title == "" {
		title = capture.Title
	}
	title = strings.TrimSpace(title)
	if title == "" || len(title) > 300 || strings.ContainsAny(title, "\r\n\x00") || !utf8.ValidString(title) {
		return Document{}, invalidInput("provide a title of 1–300 bytes")
	}
	format := SourceFormatForMIME(capture.Record.MIME)
	if format.MIME == "" {
		return Document{}, invalidInput("unsupported source format")
	}
	ext := format.Extensions[0]

	if err := n.loadLocks(ctx); err != nil {
		return Document{}, err
	}
	if err := n.clean(ctx); err != nil {
		return Document{}, err
	}
	head, err := n.mutateBase(ctx)
	if err != nil {
		return Document{}, err
	}
	id := ""
	container := ""
	if target != "" {
		if folder != nil {
			return Document{}, invalidInput("folder applies only to new imports; use Move to relocate an existing source")
		}
		if !strings.HasSuffix(target, ".md") || expected == "" || !revisionPattern.MatchString(expected) {
			return Document{}, ErrConflict
		}
		ancestor, err := n.isAncestor(ctx, expected, head)
		if err != nil {
			return Document{}, err
		}
		if !ancestor {
			return Document{}, ErrConflict
		}
		if err = n.resolveAtHead(ctx, target); err != nil {
			return Document{}, err
		}
		old, e := n.read(ctx, target, head)
		if e != nil {
			return Document{}, e
		}
		record := ParseSource(old.Content)
		if record == nil {
			return Document{}, invalidInput("target is not an archived source")
		}
		before, e := n.fileVersion(ctx, target, expected)
		if e != nil || before != old.Version {
			return Document{}, ErrConflict
		}
		container, e = ParseContainer(old.Content)
		if e != nil {
			return Document{}, e
		}
		id = record.ID
	} else {
		if expected != "" {
			return Document{}, ErrConflict
		}
		id = capture.Record.ID
		if id == "" {
			b := make([]byte, 16)
			rand.Read(b)
			id = hex.EncodeToString(b)
		}
		if decoded, e := hex.DecodeString(id); e != nil || len(decoded) != 16 {
			return Document{}, invalidInput("invalid source capture ID")
		}
		destination := "sources"
		if folder != nil {
			destination = *folder
		}
		if destination != "" {
			if path.Clean(destination) != destination {
				return Document{}, ErrForbidden
			}
			if _, err := n.resolve(destination, true); err != nil {
				return Document{}, err
			}
			if err := n.resolveAtHead(ctx, destination); err != nil {
				return Document{}, err
			}
			if file, err := n.exists(ctx, head, destination); err != nil {
				return Document{}, err
			} else if file {
				return Document{}, invalidInput("source destination is not a folder")
			}
		}
		target = path.Join(destination, id+".md")
		// A preview identifies one capture even if its destination changes on retry.
		entries, err := n.list(ctx)
		if err != nil {
			return Document{}, err
		}
		for _, entry := range entries {
			if entry.Metadata != nil && entry.Metadata.Source != nil && entry.Metadata.Source.ID == id {
				return Document{}, ErrConflict
			}
		}
		if _, e := n.read(ctx, target, head); e == nil {
			return Document{}, ErrConflict
		} else if !errors.Is(e, os.ErrNotExist) {
			return Document{}, e
		}
	}
	if _, err = n.resolve(target, true); err != nil {
		return Document{}, err
	}
	if err = n.resolveAtHead(ctx, target); err != nil {
		return Document{}, err
	}
	r := capture.Record
	r.ID = id
	r.SHA256 = ContentHash(capture.Original)
	r.TextSHA256 = ContentHash([]byte(capture.Text))
	r.ImportedBy = attribution.From(ctx).ID
	if r.CapturedAt == "" {
		r.CapturedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	r.Original = "assets/sources/" + id + "/" + r.SHA256 + ext
	if _, err = n.resolve(r.Original, true); err != nil {
		return Document{}, err
	}
	if err = n.resolveAtHead(ctx, r.Original); err != nil {
		return Document{}, err
	}
	var content strings.Builder
	fmt.Fprintf(&content, "---\ntitle: %s\nkind: archived-source\nstatus: unreviewed\n", quoteSource(title))
	if container != "" {
		fmt.Fprintf(&content, "fmn-container: %s\n", container)
	}
	fields := sourceFields(&r)
	for _, key := range []string{"id", "url", "final_url", "captured_at", "imported_by", "method", "original", "sha256", "mime", "extractor", "extraction", "warning", "text_sha256"} {
		fmt.Fprintf(&content, "source_%s: %s\n", key, quoteSource(*fields[key]))
	}
	// External text has already been converted to inert Markdown by the capture layer.
	fmt.Fprintf(&content, "---\n\n# %s\n\n[Original file](/%s)\n\n", EscapeSourceText(title), r.Original)
	content.WriteString("## Extracted content\n\n")
	content.WriteString(capture.Text)
	changes := map[string][]byte{target: []byte(content.String())}
	existing, e := n.read(ctx, r.Original, head)
	if errors.Is(e, os.ErrNotExist) {
		changes[r.Original] = capture.Original
	} else if e != nil {
		return Document{}, e
	} else if ContentHash(existing.Content) != r.SHA256 {
		return Document{}, errors.New("archived original checksum mismatch")
	}
	result, err := n.applyChanges(ctx, head, "Archive source: "+title, orderedChanges(changes, nil))
	if err != nil {
		return Document{}, err
	}
	return n.read(ctx, target, result.Head.Commit.String())
}

// VerifySource compares an archived source's original and extracted text
// with the checksums its provenance records.
func (n *dbNotebook) VerifySource(ctx context.Context, target, revision string) (SourceVerification, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := n.resolveAtHead(ctx, target); err != nil {
		return SourceVerification{}, err
	}
	doc, err := n.read(ctx, target, revision)
	if err != nil {
		return SourceVerification{}, err
	}
	record := ParseSource(doc.Content)
	if record == nil {
		return SourceVerification{}, invalidInput("note is not an archived source")
	}
	result := SourceVerification{OriginalStatus: "unavailable", TextStatus: "changed", Revision: doc.Revision, OriginalState: "Original unavailable", TextState: "Extracted text changed"}
	err = n.resolveAtHead(ctx, record.Original)
	var original Document
	if err == nil {
		original, err = n.read(ctx, record.Original, doc.Revision)
	}
	if err == nil {
		result.OriginalStatus = "changed"
		result.OriginalState = "Original checksum mismatch"
		if ContentHash(original.Content) == record.SHA256 {
			result.OriginalStatus = "match"
			result.OriginalState = "Original checksum matches"
		}
	}
	_, text, ok := strings.Cut(string(MarkdownBody(doc.Content)), "\n## Extracted content\n\n")
	if ok && ContentHash([]byte(text)) == record.TextSHA256 {
		result.TextStatus = "match"
		result.TextState = "Extracted text checksum matches"
	}
	return result, nil
}

// WriteInstructions is called only by Owner- and Admin-authorized handlers.
// Generic content APIs cannot promote imported text into project-level
// instructions.
func (n *dbNotebook) WriteInstructions(ctx context.Context, content []byte, expected, actor string) (Document, error) {
	ctx = n.mutationContext(ctx)
	if len(content) > 65536 {
		return Document{}, invalidInput("project instructions are limited to 64 KiB")
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := n.loadLocks(ctx); err != nil {
		return Document{}, err
	}
	if _, err := n.resolve("AGENTS.md", true); err != nil {
		return Document{}, err
	}
	if err := n.resolveAtHead(ctx, "AGENTS.md"); err != nil {
		return Document{}, err
	}
	return n.save(ctx, "AGENTS.md", content, expected)
}

// AddFinding records a review finding as a comment on its note, pinned to
// the note's content and its evidence; a retry with the same key replays.
func (n *dbNotebook) AddFinding(ctx context.Context, in FindingInput, actor string) (ReviewFinding, error) {
	ctx = n.mutationContext(ctx)
	if len(in.Key) < 1 || len(in.Key) > 128 || strings.TrimSpace(in.Title) == "" || len(in.Title) > 300 || strings.TrimSpace(in.Text) == "" || len(in.Text) > 10000 || len(in.Evidence) > 12 {
		return ReviewFinding{}, invalidInput("finding needs a retry key, title, explanation and at most 12 evidence references")
	}
	raw, _ := json.Marshal(in)
	requestHash := ContentHash(raw)
	id := ContentHash([]byte(actor + "\x00" + in.Key))
	d, err := n.Comments(ctx, in.Path)
	if err != nil {
		return ReviewFinding{}, err
	}
	for _, c := range d.Comments {
		if c.ID == id {
			if c.Finding == nil || c.Finding.RequestHash != requestHash {
				return ReviewFinding{}, ErrConflict
			}
			return ReviewFinding{Path: in.Path, Comment: c, Revision: d.Revision}, nil
		}
	}
	note, err := n.KnowledgeRead(ctx, in.Path, in.Revision)
	if err != nil {
		return ReviewFinding{}, err
	}
	if in.Revision == "" || in.Hash != ContentHash(note.Content) {
		return ReviewFinding{}, ErrConflict
	}
	if err = n.AssertSnapshot(ctx, in.Revision); err != nil {
		return ReviewFinding{}, err
	}
	for i := range in.Evidence {
		ref := &in.Evidence[i]
		if len(ref.Explanation) > 2000 || len(ref.URL) > 4000 || len(ref.Heading) > 500 {
			return ReviewFinding{}, invalidInput("evidence reference exceeds limits")
		}
		if ref.URL != "" {
			u, e := url.Parse(ref.URL)
			if e != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				return ReviewFinding{}, invalidInput("external evidence must be an HTTP(S) URL")
			}
		}
		if ref.Path != "" {
			doc, e := n.KnowledgeRead(ctx, ref.Path, ref.Revision)
			if e != nil {
				return ReviewFinding{}, e
			}
			if ref.Hash != "" && ref.Hash != ContentHash(doc.Content) {
				return ReviewFinding{}, ErrConflict
			}
			ref.Revision = doc.Revision
			ref.Hash = ContentHash(doc.Content)
		}
	}
	if len(d.Comments) >= 1000 {
		return ReviewFinding{}, capacityReached("discussion limit reached")
	}
	c := Comment{ID: id, Author: actor, Text: in.Text, Created: time.Now().UTC().Format(time.RFC3339), Finding: &Finding{Key: in.Key, Path: in.Path, Title: in.Title, Origin: "agent", SourcePath: in.Path, SourceRevision: in.Revision, NoteHash: in.Hash, Evidence: in.Evidence, RequestHash: requestHash}}
	d.Comments = append(d.Comments, c)
	saved, err := n.saveComments(ctx, in.Path, d, actor)
	return ReviewFinding{Path: in.Path, Comment: c, Revision: saved.Revision}, err
}

// Findings is the review inbox: every finding in the head's discussions,
// marked stale when its note or pinned evidence changed since.
func (n *dbNotebook) Findings(ctx context.Context) ([]ReviewFinding, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	head, err := n.head(ctx)
	if err != nil {
		return nil, err
	}
	// The discussions "git ls-tree -r head -- comments" lists.
	var discussions []string
	contents := map[string][]byte{}
	err = n.forEachCommittedContent(ctx, head, func(p string) bool {
		return strings.HasPrefix(p, "comments/") && strings.HasSuffix(p, ".json")
	}, func(p string, content []byte) error {
		discussions = append(discussions, p)
		contents[p] = content
		return nil
	})
	if err != nil {
		return nil, err
	}
	result := []ReviewFinding{}
	for _, p := range discussions {
		var comments []Comment
		if json.Unmarshal(contents[p], &comments) != nil {
			continue
		}
		for _, c := range comments {
			if c.Finding == nil || p != commentPath(c.Finding.Path) {
				continue
			}
			if visible, err := n.visibleAtHead(ctx, c.Finding.Path); err != nil {
				return nil, err
			} else if !visible {
				continue
			}
			current, err := n.read(ctx, c.Finding.Path, head)
			c.Finding.Stale = err != nil || ContentHash(current.Content) != c.Finding.NoteHash
			for i := range c.Finding.Evidence {
				ref := &c.Finding.Evidence[i]
				if ref.Path == "" {
					continue
				}
				visible, err := n.visibleAtHead(ctx, ref.Path)
				if err != nil {
					return nil, err
				}
				if !visible {
					*ref = SourceEvidence{Explanation: "Evidence unavailable"}
					c.Finding.Stale = true
					continue
				}
				now, e := n.read(ctx, ref.Path, head)
				old, e2 := n.read(ctx, ref.Path, ref.Revision)
				if e != nil || e2 != nil || ContentHash(now.Content) != ContentHash(old.Content) {
					c.Finding.Stale = true
				}
			}
			result = append(result, ReviewFinding{Path: c.Finding.Path, Comment: c, Revision: head})
			if len(result) > 1000 {
				return nil, capacityReached("review inbox exceeds 1000 findings")
			}
		}
	}
	return result, nil
}

// DecideFinding resolves or dismisses a finding, as its author or as an
// editor.
func (n *dbNotebook) DecideFinding(ctx context.Context, path, id, revision, actor, decision string, canEdit bool) (ReviewFinding, error) {
	ctx = n.mutationContext(ctx)
	n.mu.Lock()
	visible, err := n.visibleAtHead(ctx, path)
	n.mu.Unlock()
	if err != nil {
		return ReviewFinding{}, err
	}
	if !visible {
		return ReviewFinding{}, ErrForbidden
	}
	doc, err := n.Read(ctx, commentPath(path), "")
	if err != nil {
		return ReviewFinding{}, err
	}
	if doc.Revision != revision {
		return ReviewFinding{}, ErrConflict
	}
	var comments []Comment
	if err = json.Unmarshal(doc.Content, &comments); err != nil {
		return ReviewFinding{}, err
	}
	for i := range comments {
		c := &comments[i]
		if c.ID != id || c.Finding == nil || c.Finding.Path != path {
			continue
		}
		if c.Author != actor && !canEdit {
			return ReviewFinding{}, ErrForbidden
		}
		if decision != "resolve" && decision != "dismiss" {
			return ReviewFinding{}, invalidInput("choose resolve or dismiss")
		}
		c.Resolved = true
		c.Finding.Dismissed = decision == "dismiss"
		saved, err := n.saveComments(ctx, path, Discussion{Comments: comments, Revision: revision}, actor)
		return ReviewFinding{Path: path, Comment: *c, Revision: saved.Revision}, err
	}
	return ReviewFinding{}, os.ErrNotExist
}
