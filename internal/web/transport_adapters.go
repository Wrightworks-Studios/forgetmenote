package web

import (
	"context"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"

	"forgetmenote/internal/webcore"
	"forgetmenote/internal/workspace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// A notebook operation is defined once, as func(ctx, store, in) (out, error),
// and every transport serves it through a thin adapter: notebookJSON for the
// browser API, addProjectTool for MCP (which also registers it for the action
// API and chat). Each adapter authorizes the caller for the notebook before the
// operation runs, so an operation never reveals notebook state to a caller who
// may not see it, and both classify its errors with webcore.ErrorOf.

type projectArg struct {
	Project string `json:"project"`
}

func (p projectArg) projectName() string { return p.Project }

// setNotebook binds a browser input to the notebook its URL selected. A body or
// query that names another notebook is refused rather than silently redirected.
func (p *projectArg) setNotebook(id string) error {
	if p.Project != "" && p.Project != id {
		return webcore.Invalid("project must be the notebook in the request URL")
	}
	p.Project = id
	return nil
}

// Notebook authorization precedes data access and mutation for every transport.
func addProjectTool[I interface{ projectName() string }, O any](s *Server, server *mcp.Server, name, description, role string, fn func(context.Context, *workspace.Store, I) (O, error)) {
	tool := &mcp.Tool{Name: name, Description: description}
	if role == "read" {
		tool.Annotations = &mcp.ToolAnnotations{ReadOnlyHint: true}
	}
	addAction(s, server, tool, func(ctx context.Context, in I) (O, error) {
		var zero O
		store, err := s.toolStore(ctx, in.projectName(), role)
		if err != nil {
			return zero, err
		}
		out, err := fn(ctx, store, in)
		return out, err
	})
}

// notebookJSON serves op as a browser route of the notebook selected by the
// URL. A GET reads its input from query parameters named like the input's
// JSON fields; other methods decode a JSON body. Route selection authorized
// role before the body arrived; like MCP, the operation runs only after the
// caller is authorized again with its input in hand, so access revoked while
// a slow body was read is not used.
func notebookJSON[I, O any](s *Server, pattern, role string, op func(context.Context, *workspace.Store, I) (O, error)) {
	s.handleProject(pattern, role, notebookJSONHandler(s, pattern, role, op))
}

// notebookJSONRoute is notebookJSON for a module's routes, which register
// through the module's routeRegistrar.
func notebookJSONRoute[I, O any](s *Server, r routeRegistrar, pattern, role string, op func(context.Context, *workspace.Store, I) (O, error)) {
	r.project(pattern, role, notebookJSONHandler(s, pattern, role, op))
}

// notebookJSONHandler is notebookJSON's handler, for a route that prepares
// the request's context first.
func notebookJSONHandler[I, O any](s *Server, pattern, role string, op func(context.Context, *workspace.Store, I) (O, error)) http.HandlerFunc {
	get := strings.HasPrefix(pattern, http.MethodGet+" ")
	return func(w http.ResponseWriter, r *http.Request) {
		var in I
		if get {
			if err := decodeQuery(r.URL.Query(), &in); err != nil {
				fail(w, err)
				return
			}
		} else if !decode(w, r, &in) {
			return
		}
		store, err := s.catalog.Access(subject(r.Context()), projectID(r.Context()), role)
		if err != nil {
			fail(w, err)
			return
		}
		if target, ok := any(&in).(interface{ setNotebook(string) error }); ok {
			if err := target.setNotebook(projectID(r.Context())); err != nil {
				fail(w, err)
				return
			}
		}
		out, err := op(r.Context(), store, in)
		respond(w, out, err)
	}
}

// decodeQuery fills the string, integer and boolean fields of *in, including
// those of embedded structs, from the query parameters named by their JSON
// tags. Parameters the input does not name are ignored, as before.
func decodeQuery(values url.Values, in any) error {
	return decodeQueryFields(values, reflect.ValueOf(in).Elem())
}

func decodeQueryFields(values url.Values, v reflect.Value) error {
	if v.Kind() != reflect.Struct {
		return nil
	}
	for i := range v.NumField() {
		field, value := v.Type().Field(i), v.Field(i)
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if field.Anonymous && name == "" {
			if err := decodeQueryFields(values, value); err != nil {
				return err
			}
			continue
		}
		raw := values.Get(name)
		if !field.IsExported() || name == "" || name == "-" || raw == "" {
			continue
		}
		switch value.Kind() {
		case reflect.String:
			value.SetString(raw)
		case reflect.Int, reflect.Int64:
			n, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || value.OverflowInt(n) {
				return webcore.Invalid(name + " must be an integer")
			}
			value.SetInt(n)
		case reflect.Uint64:
			n, err := strconv.ParseUint(raw, 10, 64)
			if err != nil {
				return webcore.Invalid(name + " must be a non-negative integer")
			}
			value.SetUint(n)
		case reflect.Bool:
			b, err := strconv.ParseBool(raw)
			if err != nil {
				return webcore.Invalid(name + " must be true or false")
			}
			value.SetBool(b)
		default:
			return webcore.Invalid(name + " cannot be given in the URL")
		}
	}
	return nil
}

// mcpErrorMeta names the classification of a failed tool call in its _meta.
const mcpErrorMeta = "forgetmenote/error"

// typedToolErrors gives a failed tool call the classification a browser
// request gets as its status: {"status", "code"} under _meta, plus
// "retryable": true when the same call may succeed later (capacity, HTTP 429,
// or a timeout). The message text is unchanged; an unclassified error
// carries no type.
func typedToolErrors(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		result, err := next(ctx, method, req)
		call, ok := result.(*mcp.CallToolResult)
		if !ok || call == nil || !call.IsError || call.GetError() == nil {
			return result, err
		}
		detail := webcore.ErrorOf(call.GetError(), 0)
		if detail.Status == 0 {
			return result, err
		}
		typed := map[string]any{"status": detail.Status, "code": detail.Code}
		if detail.Retryable() {
			typed["retryable"] = true
		}
		if call.Meta == nil {
			call.Meta = mcp.Meta{}
		}
		call.Meta[mcpErrorMeta] = typed
		return result, err
	}
}
