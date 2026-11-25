package workspace

import (
	"bytes"
	"errors"
	"html"
	"net/url"
	"path"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// Verify the semantic destinations parsed by the renderer as well as the edits
// made by the existing source-preserving rewriter. Unsupported valid Markdown
// (for example an escaped closing bracket in a label) must never leave a broken
// dependency merely because the rewriter did not recognize its source spelling.
// ignore, when not nil, reports whether a destination the parser cannot
// resolve may be left out of the result instead of failing it.
func organizationLinkTargets(content []byte, note, from, to string, ignore func(href string) bool) ([]string, error) {
	result := []string{}
	tree := goldmark.DefaultParser().Parse(text.NewReader(MarkdownBody(content)))
	err := ast.Walk(tree, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		var raw []byte
		switch n := node.(type) {
		case *ast.Link:
			raw = n.Destination
		case *ast.Image:
			raw = n.Destination
		default:
			return ast.WalkContinue, nil
		}
		href := html.UnescapeString(string(util.UnescapePunctuations(raw)))
		u, e := url.Parse(href)
		if e != nil {
			if ignore != nil && ignore(href) {
				return ast.WalkContinue, nil
			}
			return ast.WalkStop, errors.New("unresolved Markdown destination")
		}
		if !u.IsAbs() && u.Host == "" && u.Opaque == "" && u.Path != "" {
			target := path.Clean(path.Join(path.Dir(note), u.Path))
			if u.Path[0] == '/' {
				target = path.Clean(u.Path[1:])
			}
			if !revisionPattern.MatchString(u.Query().Get("fmn-rev")) {
				target = movedPath(target, from, to)
			}
			u.Path = "/" + target
			u.RawPath = ""
		}
		result = append(result, u.String())
		return ast.WalkContinue, nil
	})
	return result, err
}
func verifyOrganizationLinks(before, after []byte, oldNote, newNote, from, to string) error {
	return verifyLinkTargets(before, after, oldNote, newNote, from, to, nil)
}

// verifyLinkTargets is verifyOrganizationLinks leaving out, before and
// after alike, each destination the parser cannot resolve that ignore
// accepts.
func verifyLinkTargets(before, after []byte, oldNote, newNote, from, to string, ignore func(href string) bool) error {
	want, e := organizationLinkTargets(before, oldNote, from, to, ignore)
	if e != nil {
		return e
	}
	got, e := organizationLinkTargets(after, newNote, "__none__", "__none__", ignore)
	if e != nil {
		return e
	}
	if len(want) != len(got) {
		return errors.New("Markdown dependencies could not be preserved")
	}
	for i := range want {
		if !bytes.Equal([]byte(want[i]), []byte(got[i])) {
			return errors.New("Markdown dependency requires a manual repair")
		}
	}
	return nil
}
