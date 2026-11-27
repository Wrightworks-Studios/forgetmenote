package web

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"forgetmenote/internal/knowledge"
	"forgetmenote/internal/workspace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type writingOutlineRequest struct {
	documentRef
	After int `json:"after,omitempty"`
	Limit int `json:"limit,omitempty"`
}
type writingOutlineResult struct {
	documentRef
	Version  int                        `json:"version"`
	Hash     string                     `json:"hash"`
	Citation string                     `json:"citation"`
	Stats    knowledge.WritingStats     `json:"stats"`
	Sections []knowledge.WritingSection `json:"sections"`
	Total    int                        `json:"total"`
	Next     int                        `json:"next,omitempty"`
}
type writingTermsRequest struct {
	Project  string `json:"project"`
	Origin   string `json:"origin,omitempty"`
	Source   string `json:"source,omitempty"`
	NoteID   string `json:"noteId,omitempty"`
	Path     string `json:"path,omitempty"`
	Revision string `json:"revision,omitempty"`
	knowledge.ContainerSelection
	Terms      []knowledge.WritingTerm `json:"terms"`
	PathPrefix string                  `json:"pathPrefix,omitempty"`
	After      string                  `json:"after,omitempty"`
	Limit      int                     `json:"limit,omitempty"`
}
type writingMention struct {
	documentRef
	knowledge.TermMention
	Hash     string `json:"hash"`
	Citation string `json:"citation"`
}
type writingTermsResult struct {
	Project  string           `json:"project"`
	Origin   string           `json:"origin"`
	Source   string           `json:"source,omitempty"`
	Revision string           `json:"revision"`
	Matches  []writingMention `json:"matches"`
	Scanned  int              `json:"scanned"`
	Next     string           `json:"next,omitempty"`
	Complete bool             `json:"complete"`
}
type writingCursor struct {
	Key   string `json:"key"`
	Page  string `json:"page,omitempty"`
	File  int    `json:"file"`
	Match int    `json:"match"`
}

func writingMarkdown(path string) bool {
	return strings.HasSuffix(strings.ToLower(path), ".md") || strings.HasSuffix(strings.ToLower(path), ".markdown")
}
func (s *Server) writingOutline(ctx context.Context, in writingOutlineRequest) (writingOutlineResult, error) {
	out := writingOutlineResult{}
	if in.After < 0 || in.Limit < 0 || in.Limit > 100 || in.After > 0 && in.Revision == "" {
		return out, errors.New("outline requires 0–100 sections and a revision for continuation")
	}
	if !writingMarkdown(in.Path) {
		return out, errors.New("writing tools require a Markdown document")
	}
	doc, err := s.readDocument(ctx, documentRequest{documentRef: in.documentRef})
	if err != nil {
		return out, err
	}
	if doc.More {
		return out, errors.New("document is incomplete; writing analysis requires the complete Markdown file")
	}
	store, err := s.toolStore(ctx, in.Project, "read")
	if err != nil {
		return out, err
	}
	analysis, err := withEngine(s, ctx, store, func(e *knowledge.Engine) (knowledge.WritingAnalysis, error) {
		return e.Writing([]byte(doc.Content)), nil
	})
	if err != nil {
		return out, err
	}
	if in.After > len(analysis.Sections) {
		return out, workspace.ErrConflict
	}
	if in.Limit == 0 {
		in.Limit = 100
	}
	end := min(in.After+in.Limit, len(analysis.Sections))
	out = writingOutlineResult{documentRef: doc.DocumentRef, Version: analysis.Version, Hash: doc.Hash, Citation: doc.Citation, Stats: analysis.Stats, Sections: analysis.Sections[in.After:end], Total: len(analysis.Sections)}
	if end < len(analysis.Sections) {
		out.Next = end
	}
	if _, err = s.toolStore(ctx, in.Project, "read"); err != nil {
		return writingOutlineResult{}, err
	}
	return out, nil
}
func (s *Server) writingTerms(ctx context.Context, in writingTermsRequest) (writingTermsResult, error) {
	out := writingTermsResult{Project: in.Project, Matches: []writingMention{}}
	if err := knowledge.ValidateWritingTerms(in.Terms); err != nil {
		return out, err
	}
	if in.Limit < 0 || in.Limit > 100 || len(in.After) > 4096 || len(in.PathPrefix) > 512 || in.After != "" && in.Revision == "" {
		return out, errors.New("term search requires at most 100 matches and a revision for continuation")
	}
	if in.Limit == 0 {
		in.Limit = 100
	}
	store, err := s.toolStore(ctx, in.Project, "read")
	if err != nil {
		return out, err
	}
	engine, releaseEngine, err := s.acquireEngine(ctx, store)
	if err != nil {
		return out, err
	}
	defer releaseEngine()
	in.Origin, err = s.documentOrigin(in.Project, in.Origin)
	if err != nil {
		return out, err
	}
	ctx, err = s.chatrun.DocumentScope(ctx, in.Project, in.Origin, in.Source, in.Revision, in.Path)
	if err != nil {
		return out, err
	}
	visibility, err := store.HistoryVisibilityID(ctx)
	if err != nil {
		return out, err
	}
	scope, err := s.documentContentScope(ctx, store, in.ContainerSelection)
	if err != nil {
		return out, err
	}
	cursor := writingCursor{}
	if in.After != "" {
		raw, e := base64.RawURLEncoding.DecodeString(in.After)
		if e != nil || json.Unmarshal(raw, &cursor) != nil || len(cursor.Key) != 64 || cursor.File < 0 || cursor.File > 8 || cursor.Match < 0 || cursor.Match > workspace.MaxSnapshotBytes() {
			return out, workspace.ErrConflict
		}
	}
	var page documentList
	if in.Path != "" || in.NoteID != "" {
		if cursor.Page != "" || cursor.File != 0 {
			return out, workspace.ErrConflict
		}
		doc, e := s.readDocument(ctx, documentRequest{documentRef: documentRef{Project: in.Project, Origin: in.Origin, Source: in.Source, Path: in.Path, NoteID: in.NoteID, Revision: in.Revision}})
		if e != nil {
			return out, e
		}
		in.Path, in.NoteID, in.Origin = doc.Path, doc.NoteID, doc.Origin
		page = documentList{Revision: doc.Revision, Source: doc.Source, Entries: []documentListEntry{{documentRef: doc.DocumentRef}}}
	} else {
		page, err = s.listDocuments(ctx, notebookMapRequest{Project: in.Project, Origin: in.Origin, Source: in.Source, MapQuery: knowledge.MapQuery{ContainerSelection: in.ContainerSelection, Scope: &scope, Revision: in.Revision, PathPrefix: in.PathPrefix, Limit: 8, After: cursor.Page}})
		if err != nil {
			return out, err
		}
	}
	in.Revision, in.Source = page.Revision, page.Source
	out.Origin, out.Source, out.Revision = in.Origin, in.Source, in.Revision
	binding := in
	binding.After = ""
	binding.Limit = 0
	raw, _ := json.Marshal(binding)
	key := workspace.ContentHash(append(raw, []byte(visibility+scope.ID())...))
	if cursor.Key != "" && cursor.Key != key || cursor.File > len(page.Entries) {
		return out, workspace.ErrConflict
	}
	next := func(file, match int, page string) {
		raw, _ := json.Marshal(writingCursor{Key: key, Page: page, File: file, Match: match})
		out.Next = base64.RawURLEncoding.EncodeToString(raw)
	}
	for i := cursor.File; i < len(page.Entries); i++ {
		if err = ctx.Err(); err != nil {
			return out, err
		}
		ref := page.Entries[i].documentRef
		if !writingMarkdown(ref.Path) {
			continue
		}
		doc, e := s.readDocument(ctx, documentRequest{documentRef: ref})
		if e != nil {
			return out, e
		}
		if doc.More {
			return out, errors.New("document is incomplete; narrow the search to complete Markdown files")
		}
		out.Scanned++
		after := 0
		if i == cursor.File {
			after = cursor.Match
		}
		matches, more := engine.Writing([]byte(doc.Content)).Mentions([]byte(doc.Content), in.Terms, after, in.Limit-len(out.Matches))
		for _, match := range matches {
			citation, _ := url.Parse(doc.Citation)
			if ref.Origin == "repository" {
				citation.Fragment = "L" + strconv.Itoa(match.StartLine) + "-L" + strconv.Itoa(match.EndLine)
			} else {
				q := citation.Query()
				q.Set("startLine", strconv.Itoa(match.StartLine))
				q.Set("endLine", strconv.Itoa(match.EndLine))
				citation.RawQuery = q.Encode()
			}
			out.Matches = append(out.Matches, writingMention{documentRef: doc.DocumentRef, TermMention: match, Hash: doc.Hash, Citation: citation.String()})
		}
		if more {
			next(i, after+len(matches), cursor.Page)
			break
		}
	}
	if out.Next == "" && page.Next != "" {
		next(0, 0, page.Next)
	}
	out.Complete = out.Next == ""
	if _, err = s.toolStore(ctx, in.Project, "read"); err != nil {
		return writingTermsResult{}, err
	}
	now, err := store.HistoryVisibilityID(ctx)
	if err != nil {
		return writingTermsResult{}, err
	}
	if now != visibility {
		return writingTermsResult{}, workspace.ErrConflict
	}
	return out, nil
}
func (s *Server) writingRoutes() {
	s.handleProject("POST /api/writing/outline", "read", func(w http.ResponseWriter, r *http.Request) {
		var in writingOutlineRequest
		if !decode(w, r, &in) {
			return
		}
		in.Project = projectID(r.Context())
		out, err := s.writingOutline(r.Context(), in)
		respond(w, out, err)
	})
	s.handleProject("POST /api/writing/terms", "read", func(w http.ResponseWriter, r *http.Request) {
		var in writingTermsRequest
		if !decode(w, r, &in) {
			return
		}
		in.Project = projectID(r.Context())
		out, err := s.writingTerms(r.Context(), in)
		respond(w, out, err)
	})
}
func (s *Server) writingTools(server *mcp.Server) {
	addAction(s, server, &mcp.Tool{Name: "document_outline", Description: "Detailed Markdown heading and scene outline with prose statistics and UTF-8 byte ranges. Reads committed content. Follow next as after with the same revision.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, func(ctx context.Context, in writingOutlineRequest) (writingOutlineResult, error) {
		out, err := s.writingOutline(ctx, in)
		return out, err
	})
	addAction(s, server, &mcp.Tool{Name: "find_term_mentions", Description: "Find literal terms and aliases in visible committed Markdown prose, with exact byte/line ranges and revision citations. Omit path to scan the notebook. Follow next with the same revision, source and arguments until complete; matches are occurrences, not semantic claims.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, func(ctx context.Context, in writingTermsRequest) (writingTermsResult, error) {
		out, err := s.writingTerms(ctx, in)
		return out, err
	})
}
