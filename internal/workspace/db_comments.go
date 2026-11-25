package workspace

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"time"
)

// Discussions for notebooks kept in the notes store: each note's comments
// stay in comments/<sha256(path)>.json in the same revision as the note.
// Writes go through saveComments, which commits the discussion with Save
// against the revision the discussion was read at, as "Update comments/…".

// Comments reads a note's discussion at the revision its note is read at.
func (n *dbNotebook) Comments(ctx context.Context, path string) (Discussion, error) {
	if !strings.HasSuffix(path, ".md") {
		return Discussion{}, ErrForbidden
	}
	note, err := n.Read(ctx, path, "")
	if err != nil {
		return Discussion{}, err
	}
	discussion := Discussion{Comments: []Comment{}, Revision: note.Revision}
	doc, err := n.Read(ctx, commentPath(path), note.Revision)
	if errors.Is(err, os.ErrNotExist) {
		return discussion, nil
	}
	if err != nil {
		return Discussion{}, err
	}
	if err = json.Unmarshal(doc.Content, &discussion.Comments); err != nil {
		return Discussion{}, errors.New("invalid discussion file")
	}
	return discussion, nil
}

// AddComment adds a thread, or a reply to one, to a note's discussion at
// revision, the discussion's revision; an anchor must still name exactly one
// passage of the note there.
func (n *dbNotebook) AddComment(ctx context.Context, path, text, revision, author string, options ...CommentOptions) (Discussion, error) {
	ctx = n.mutationContext(ctx)

	if strings.TrimSpace(text) == "" || len(text) > 10000 {
		return Discussion{}, invalidInput("comments must contain 1–10000 bytes")
	}
	d, err := n.Comments(ctx, path)
	if err != nil {
		return Discussion{}, err
	}
	if revision != d.Revision {
		return Discussion{}, ErrConflict
	}
	var option CommentOptions
	if len(options) > 1 {
		return Discussion{}, invalidInput("one comment option set required")
	}
	if len(options) == 1 {
		option = options[0]
	}
	if option.Anchor != nil && option.ReplyTo != "" {
		return Discussion{}, invalidInput("replies use their thread's passage")
	}
	if option.Anchor != nil {
		note, readErr := n.Read(ctx, path, d.Revision)
		if readErr != nil {
			return Discussion{}, readErr
		}
		anchor := *option.Anchor
		if !validAnchor(commentText(note.Content), &anchor) {
			return Discussion{}, invalidInput("passage changed or is ambiguous; select it again")
		}
		anchor.Revision = d.Revision
		option.Anchor = &anchor
	}
	id := make([]byte, 16)
	rand.Read(id)
	created := time.Now().UTC().Format(time.RFC3339)
	if option.ReplyTo != "" {
		found := false
		for i := range d.Comments {
			c := &d.Comments[i]
			if c.ID != option.ReplyTo {
				continue
			}
			if c.Resolved {
				return Discussion{}, invalidInput("this thread is resolved")
			}
			if len(c.Replies) >= 1000 {
				return Discussion{}, capacityReached("reply limit reached")
			}
			c.Replies = append(c.Replies, CommentReply{ID: hex.EncodeToString(id), Author: author, Text: text, Created: created})
			found = true
			break
		}
		if !found {
			return Discussion{}, os.ErrNotExist
		}
	} else {
		if len(d.Comments) >= 1000 {
			return Discussion{}, capacityReached("discussion limit reached")
		}
		d.Comments = append(d.Comments, Comment{ID: hex.EncodeToString(id), Author: author, Text: text, Created: created, Anchor: option.Anchor})
	}
	return n.saveComments(ctx, path, d, author)
}

// ResolveComment resolves a thread its author or an editor chose.
func (n *dbNotebook) ResolveComment(ctx context.Context, path, id, revision, author string, canEdit bool) (Discussion, error) {
	ctx = n.mutationContext(ctx)

	d, err := n.Comments(ctx, path)
	if err != nil {
		return Discussion{}, err
	}
	if revision != d.Revision {
		return Discussion{}, ErrConflict
	}
	for i, c := range d.Comments {
		if c.ID == id {
			if c.Author != author && !canEdit {
				return Discussion{}, ErrForbidden
			}
			d.Comments[i].Resolved = true
			return n.saveComments(ctx, path, d, author)
		}
	}
	return Discussion{}, os.ErrNotExist
}

// DeleteComment removes only the selected author's content. A deleted parent
// remains as a thread placeholder until its final reply is also deleted.
func (n *dbNotebook) DeleteComment(ctx context.Context, path, id, replyID, revision, author string, canModerate bool) (Discussion, error) {
	ctx = n.mutationContext(ctx)

	d, err := n.Comments(ctx, path)
	if err != nil {
		return Discussion{}, err
	}
	if revision != d.Revision {
		return Discussion{}, ErrConflict
	}
	for i := range d.Comments {
		c := &d.Comments[i]
		if c.ID != id {
			continue
		}
		if replyID != "" {
			for j, reply := range c.Replies {
				if reply.ID != replyID {
					continue
				}
				if reply.Author != author && !canModerate {
					return Discussion{}, ErrForbidden
				}
				c.Replies = append(c.Replies[:j], c.Replies[j+1:]...)
				if c.Deleted && len(c.Replies) == 0 {
					d.Comments = append(d.Comments[:i], d.Comments[i+1:]...)
				}
				return n.saveComments(ctx, path, d, author)
			}
			return Discussion{}, os.ErrNotExist
		}
		if c.Deleted {
			return Discussion{}, os.ErrNotExist
		}
		if c.Author != author && !canModerate {
			return Discussion{}, ErrForbidden
		}
		if len(c.Replies) == 0 {
			d.Comments = append(d.Comments[:i], d.Comments[i+1:]...)
		} else {
			c.Text = ""
			c.Finding = nil
			c.Deleted = true
		}
		return n.saveComments(ctx, path, d, author)
	}
	return Discussion{}, os.ErrNotExist
}
