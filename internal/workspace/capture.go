package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

// CaptureMove is an exact, immutable transfer intent. Gateway authorization owns
// both project IDs; owners accept it only through the authenticated typed RPC.
var ErrCaptureInput = errors.New("invalid Quicknotes input")

type CaptureMove struct {
	ID            string `json:"id"`
	From          string `json:"from"`
	Version       string `json:"version"`
	Hash          string `json:"hash"`
	To            string `json:"to"`
	SourceProject string `json:"sourceProject"`
	TargetProject string `json:"targetProject"`
}
type CaptureReceipt struct {
	State    string `json:"state"`
	Revision string `json:"revision,omitempty"`
}
type captureRecord struct {
	Version int         `json:"version"`
	Actor   string      `json:"actor"`
	Kind    string      `json:"kind"`
	Request CaptureMove `json:"request"`
	CaptureReceipt
}
type captureMutationKey struct{}

func validCaptureMove(in CaptureMove) bool {
	return len(in.ID) > 0 && len(in.ID) <= 128 && !strings.ContainsAny(in.ID, "\x00\r\n") &&
		in.From != "AGENTS.md" && in.To != "AGENTS.md" && !IsTrash(in.From) && !IsTrash(in.To) &&
		strings.HasSuffix(in.From, ".md") && strings.HasSuffix(in.To, ".md") &&
		!strings.HasPrefix(in.From, "comments/") && !strings.HasPrefix(in.To, "comments/") &&
		revisionPattern.MatchString(in.Version) && len(in.Hash) == 64 &&
		len(in.SourceProject) > 0 && len(in.SourceProject) <= 64 && len(in.TargetProject) > 0 && len(in.TargetProject) <= 64
}
func captureKey(actor, kind, id string) string {
	return ContentHash([]byte(actor + "\x00" + kind + "\x00" + id))
}
func captureOperation(r captureRecord) operationIdentity {
	raw, _ := json.Marshal(struct {
		Actor, Kind string
		Request     CaptureMove
	}{r.Actor, r.Kind, r.Request})
	return operationIdentity{ID: captureKey(r.Actor, r.Kind, r.Request.ID), Request: ContentHash(raw)}
}
func captureReservedPaths(r captureRecord) []string {
	switch r.State {
	case "prepared":
		return []string{r.Request.From}
	case "receiving":
		return []string{r.Request.To}
	case "moving":
		return []string{r.Request.From, r.Request.To}
	}
	return nil
}

func capturePortable(content []byte) error {
	metadata, err := ParseMetadata(content)
	if err != nil || metadata.Container != "" || metadata.ContainerError != "" || metadata.Source != nil || metadata.Cover != nil {
		return fmt.Errorf("%w: filing needs an active note without source or media dependencies", ErrCaptureInput)
	}
	tree := goldmark.DefaultParser().Parse(text.NewReader(MarkdownBody(content)))
	return ast.Walk(tree, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		var destination []byte
		switch v := n.(type) {
		case *ast.Link:
			destination = v.Destination
		case *ast.Image:
			destination = v.Destination
		case *ast.RawHTML, *ast.HTMLBlock:
			return ast.WalkStop, fmt.Errorf("%w: filing cannot move embedded HTML dependencies", ErrCaptureInput)
		}
		if len(destination) > 0 {
			u, e := url.Parse(string(destination))
			if e != nil || (!u.IsAbs() && u.Host == "" && u.Path != "") {
				return ast.WalkStop, fmt.Errorf("%w: filing cannot rewrite notebook links or attachments", ErrCaptureInput)
			}
		}
		return ast.WalkContinue, nil
	})
}
func (s *Store) captureContext(ctx context.Context, r captureRecord) context.Context {
	return context.WithValue(context.WithValue(ctx, captureMutationKey{}, captureKey(r.Actor, r.Kind, r.Request.ID)), operationKey{}, captureOperation(r))
}

func captureCreateGroup(id, path string, content []byte) EditGroup {
	return EditGroup{ID: "quicknote-create:" + id, Summary: "Save Quicknote", Edits: []NoteEdit{{Path: path, Content: string(content)}}}
}
