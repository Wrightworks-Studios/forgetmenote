package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

type CommentAnchor struct {
	Quote    string `json:"quote"`
	Prefix   string `json:"prefix,omitempty"`
	Suffix   string `json:"suffix,omitempty"`
	Revision string `json:"revision,omitempty"`
}
type CommentOptions struct {
	Anchor  *CommentAnchor `json:"anchor,omitempty"`
	ReplyTo string         `json:"replyTo,omitempty"`
}
type CommentReply struct {
	ID      string `json:"id"`
	Author  string `json:"author"`
	Text    string `json:"text"`
	Created string `json:"created"`
}
type Comment struct {
	Finding  *Finding       `json:"finding,omitempty"`
	Anchor   *CommentAnchor `json:"anchor,omitempty"`
	Replies  []CommentReply `json:"replies,omitempty"`
	ID       string         `json:"id"`
	Author   string         `json:"author"`
	Text     string         `json:"text"`
	Created  string         `json:"created"`
	Resolved bool           `json:"resolved"`
	Deleted  bool           `json:"deleted,omitempty"`
}
type Discussion struct {
	Comments []Comment `json:"comments"`
	Revision string    `json:"revision"`
}

func commentPath(path string) string {
	sum := sha256.Sum256([]byte(path))
	return "comments/" + hex.EncodeToString(sum[:]) + ".json"
}

func (s *Store) saveComments(ctx context.Context, path string, d Discussion, author string) (Discussion, error) {
	if IsTrash(path) {
		return Discussion{}, ErrForbidden
	}
	if scope := mutationScopeFrom(ctx); scope != nil {
		scope.Target = path
		ctx = WithMutationScope(ctx, *scope)
	}
	raw, err := json.MarshalIndent(d.Comments, "", "  ")
	if err != nil {
		return Discussion{}, err
	}
	doc, err := s.Save(ctx, commentPath(path), raw, d.Revision, author+" comment")
	if err != nil {
		return Discussion{}, err
	}
	d.Revision = doc.Revision
	return d, nil
}
