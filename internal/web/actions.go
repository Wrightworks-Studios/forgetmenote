package web

import (
	"bytes"
	"context"
	"encoding/json"
	"forgetmenote/internal/actions"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"

	"forgetmenote/internal/chat"
	"forgetmenote/internal/workspace"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type registeredAction = actions.Action

// MCP clients can reject boolean schemas, including nested properties. Keep
// their equivalent object forms at the protocol boundary without changing data
// values in defaults, examples, enum or const.
//
// Output schemas are listed open. A client checks each result against the
// schema it listed when it connected, so an output field added by a later
// release would fail it until it reconnects. Add output fields freely; never
// remove a required one or change a field's type. Input schemas stay closed, so
// a misspelled argument is still refused.
func mcpToolSchema(schema *jsonschema.Schema, output bool) any {
	raw, err := json.Marshal(schema)
	if err != nil {
		panic(err)
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		panic(err)
	}
	return mcpSchemaObjects(value, output)
}

// The SDK resolves the schemas of every tool a server registers. A tool's
// schemas follow from its name and types, so the process keeps one of each:
// every server registers the same schema, mcpSchemaCache resolves it once, and
// tools/list sends the object form mcpToolSchema made for it.
var (
	mcpSchemaCache = mcp.NewSchemaCache()
	mcpSchemas     sync.Map // mcpSchemaKey -> *jsonschema.Schema
	mcpListed      sync.Map // *jsonschema.Schema -> its mcpToolSchema form
)

type mcpSchemaKey struct {
	tool   string
	typ    reflect.Type
	output bool
}

func sharedMCPSchema(key mcpSchemaKey, schema *jsonschema.Schema) *jsonschema.Schema {
	if shared, ok := mcpSchemas.Load(key); ok {
		return shared.(*jsonschema.Schema)
	}
	mcpListed.Store(schema, mcpToolSchema(schema, key.output))
	shared, _ := mcpSchemas.LoadOrStore(key, schema)
	return shared.(*jsonschema.Schema)
}

// mcpListedTool is a registered tool as clients see it.
func mcpListedTool(tool *mcp.Tool) *mcp.Tool {
	listed := *tool
	for i, field := range []*any{&listed.InputSchema, &listed.OutputSchema} {
		if schema, ok := (*field).(*jsonschema.Schema); ok {
			form, ok := mcpListed.Load(schema)
			if !ok {
				form = mcpToolSchema(schema, i == 1)
			}
			*field = form
		}
	}
	return &listed
}

func mcpSchemaObjects(value any, open bool) any {
	if flag, ok := value.(bool); ok {
		if flag {
			return map[string]any{}
		}
		return map[string]any{"not": map[string]any{}}
	}
	schema, ok := value.(map[string]any)
	if !ok {
		return value
	}
	for key, value := range schema {
		switch key {
		case "properties", "patternProperties", "$defs", "definitions", "dependentSchemas":
			if properties, ok := value.(map[string]any); ok {
				for name, property := range properties {
					properties[name] = mcpSchemaObjects(property, open)
				}
			}
		case "additionalProperties":
			if value = mcpSchemaObjects(value, open); open && mcpClosedSchema(value) {
				delete(schema, key)
			} else {
				schema[key] = value
			}
		case "items", "additionalItems", "unevaluatedItems", "unevaluatedProperties", "propertyNames", "contains", "not", "if", "then", "else", "contentSchema":
			schema[key] = mcpSchemaObjects(value, open)
		case "allOf", "anyOf", "oneOf", "prefixItems":
			if choices, ok := value.([]any); ok {
				for i, choice := range choices {
					choices[i] = mcpSchemaObjects(choice, open)
				}
			}
		}
	}
	return schema
}

// mcpClosedSchema reports whether schema is {"not":{}}, which no value meets.
func mcpClosedSchema(schema any) bool {
	m, _ := schema.(map[string]any)
	not, _ := m["not"].(map[string]any)
	return len(m) == 1 && not != nil && len(not) == 0
}

// Every transport calls the same typed operation. MCP request/session details
// are translated only in the SDK adapter, never reconstructed by other callers.
func addAction[I, O any](s *Server, server *mcp.Server, tool *mcp.Tool, fn func(context.Context, I) (O, error), scope ...string) {
	schema, outputSchema, resolved := actions.Schemas[I, O]()
	original := fn
	readOnly := tool.Annotations != nil && tool.Annotations.ReadOnlyHint
	if readOnly {
		original = withPurgedRevisions(s, original)
	}
	required := clientActionScope(tool)
	if len(scope) > 0 {
		required = scope[0]
	}
	fn = func(ctx context.Context, in I) (O, error) {
		var zero O
		raw, err := json.Marshal(in)
		if err != nil {
			return zero, err
		}
		if err = s.guardClientAction(ctx, tool.Name, required, raw); err != nil {
			return zero, err
		}
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		admission := s.rpcWork
		if readOnly {
			admission = s.rpcReadWork
		}
		switch tool.Name {
		case "list_data_connections", "list_data_operations", "call_data_operation":
			// Forward data I/O independently of notebook reads and mutations.
			// Input decoding, authority and each connector's own policy still apply.
			admission = nil
		}
		release, err := admitRPC(ctx, admission)
		if err != nil {
			return zero, err
		}
		defer release()
		var target struct {
			Project    string `json:"project"`
			Generation uint64 `json:"authorityGeneration"`
		}
		if json.Unmarshal(raw, &target) == nil && target.Project != "" {
			generation := s.catalog.Authority(target.Project).Generation
			if target.Generation != 0 {
				generation = target.Generation
			}
			ctx = workspace.WithAuthority(ctx, generation)
		}
		out, err := original(ctx, in)
		if err == nil {
			err = s.checkActionCredential(ctx, tool.Name)
		}
		return out, err
	}
	if s.actions == nil {
		s.actions = actions.Registry{}
	}
	s.actions[tool.Name] = registeredAction{Name: tool.Name, Description: tool.Description, Policy: policy(tool.Name), Schema: schema, OutputSchema: outputSchema, ClientScope: required, Resolved: resolved, Call: actions.TypedCall(fn)}
	alias := actions.NotebookAlias(tool.Name)
	if alias != "" {
		action := s.actions[tool.Name]
		action.Name = alias
		action.Description = strings.NewReplacer("projects", "notebooks", "project", "notebook").Replace(tool.Description)
		s.actions[alias] = action
	}
	// The SDK re-resolves every schema it registers. API, chat and automation
	// callers use the registry above, so MCP pays for that on its first request.
	s.mcpTools = append(s.mcpTools, func() {
		tool.InputSchema = sharedMCPSchema(mcpSchemaKey{tool.Name, reflect.TypeFor[I](), false}, schema)
		tool.OutputSchema = sharedMCPSchema(mcpSchemaKey{tool.Name, reflect.TypeFor[O](), true}, outputSchema)
		register := func(tool *mcp.Tool) {
			mcp.AddTool(server, tool, func(ctx context.Context, request *mcp.CallToolRequest, in I) (*mcp.CallToolResult, O, error) {
				if tool.Name == "call_data_operation" && request != nil && request.Session != nil {
					ctx = context.WithValue(ctx, learningSessionKey{}, request.Session.ID())
				}
				out, err := fn(ctx, in)
				return nil, out, err
			})
		}
		register(tool)
		if alias != "" {
			aliasTool := *tool
			aliasTool.Name = alias
			aliasTool.Description = s.actions[alias].Description
			register(&aliasTool)
		}
	})
}

// Self-revocation must disclose its success after invalidating the caller.
func (s *Server) checkActionCredential(ctx context.Context, name string) error {
	if name == "revoke_api_credential" {
		return nil
	}
	return s.checkCredential(ctx)
}
func (s *Server) actionArguments(project, name string, args json.RawMessage) (registeredAction, json.RawMessage, error) {
	return s.actions.Arguments(project, name, args)
}
func (s *Server) dispatchAction(ctx context.Context, project, name string, args json.RawMessage) (json.RawMessage, error) {
	result, err := s.dispatchActionOutcome(ctx, project, name, args)
	if err != nil {
		return nil, err
	}
	if _, err = s.catalog.Access(subject(ctx), project, "read"); err != nil {
		return nil, err
	}
	return result, nil
}

// dispatchActionOutcome is for private receipt recording. Its callers must check
// current authorization again before disclosing the result or continuing work.
func (s *Server) dispatchActionOutcome(ctx context.Context, project, name string, args json.RawMessage) (out json.RawMessage, callErr error) {
	if scope := chatScopeFrom(ctx); scope != nil {
		if _, known := s.actions[name]; known {
			defer func() { chatToolDiagnostic(scope.Run.ID, name, callErr) }()
		}
	}
	if _, err := s.catalog.Access(subject(ctx), project, "read"); err != nil {
		return nil, err
	}
	a, raw, err := s.actionArguments(project, name, args)
	if err != nil {
		return nil, err
	}
	raw, err = scopeChatActionArguments(ctx, name, raw)
	if err != nil {
		return nil, err
	}
	ctx, err = s.guardChatAction(ctx, project, name, raw)
	if err != nil {
		return nil, err
	}
	result, err := a.Call(ctx, raw)
	if err != nil {
		return nil, err
	}
	if len(result) > 1<<20 {
		return json.Marshal(map[string]any{"truncated": true, "message": "Result exceeds chat limit; retrieve selected passages or inspect it in the notebook.", "bytes": len(result), "sha256": chat.Hash(result)})
	}
	return result, nil
}

type actionScope = actions.Scope

const (
	currentNotebook      = actions.CurrentNotebook
	selectedRepository   = actions.SelectedRepository
	selectedRepositories = actions.SelectedRepositories
	selectedDocument     = actions.SelectedDocument
	selectedNotebooks    = actions.SelectedNotebooks
)

type actionPolicy = actions.Policy

func policy(name string) actionPolicy { return actions.LookupPolicy(name) }
func allowedAction(mode, name string, args json.RawMessage, enabled []string, approved bool) bool {
	return actions.Allowed(mode, name, args, enabled, approved)
}

func (s *Server) actionTool(name string) (chat.Tool, bool) {
	a, ok := s.actions[name]
	if !ok || !a.Policy.ChatAvailable() {
		return chat.Tool{}, false
	}
	raw, _ := json.Marshal(a.Schema)
	var obj map[string]any
	json.Unmarshal(raw, &obj)
	props, _ := obj["properties"].(map[string]any)
	if name == "call_data_operation" {
		delete(props, "schemaHash")
	}
	if name == "call_data_operation" || name == "read_personal_source" {
		props["arguments"] = map[string]any{"type": "string", "description": "A JSON object encoded as a string, matching the input schema returned by list_data_operations. The connection's configured maxBytes applies."}
		if name == "read_personal_source" {
			props["arguments"] = map[string]any{"type": "string", "description": "A JSON object encoded as a string, matching the operation inputSchema from assistant_status.operations. Use the provider and current generation from your connected account."}
		}
	}
	if a.Policy.Scope != selectedRepository && a.Policy.Scope != selectedDocument {
		delete(props, "project")
	}
	required, _ := obj["required"].([]any)
	filtered := []any{}
	for _, v := range required {
		if name == "call_data_operation" && v == "schemaHash" {
			continue
		}
		if v != "project" || a.Policy.Scope == selectedRepository || a.Policy.Scope == selectedDocument {
			filtered = append(filtered, v)
		}
	}
	obj["required"] = filtered
	raw, _ = json.Marshal(obj)
	description := a.Description
	if len(description) > 360 {
		description = description[:360]
	}
	return chat.Tool{Name: name, Description: description, Schema: raw}, true
}
func (s *Server) chatTools(mode string, enabled []string) []chat.Tool {
	names := []string{"read_note", "get_note_sources", "project_status", "note_history", "read_project_changes"}
	if mode != "ask" {
		names = append(names, "submit_change_group", "file_review_finding")
	}
	if mode == "edit" && slices.Contains(enabled, "live") {
		names = append(names, "read_live_note", "patch_live_note", "checkpoint_live_note")
	}
	if mode == "edit" && slices.Contains(enabled, "comments") {
		names = append(names, "list_comments", "add_comment", "resolve_comment")
	}
	out := []chat.Tool{}
	for _, name := range names {
		if tool, ok := s.actionTool(name); ok {
			out = append(out, tool)
		}
	}
	return out
}
func (s *Server) actionCatalog() []map[string]any {
	names := []string{}
	for n := range s.actions {
		names = append(names, n)
	}
	sort.Strings(names)
	out := []map[string]any{}
	for _, n := range names {
		p := s.actions[n].Policy
		if !p.ChatAvailable() {
			continue
		}
		out = append(out, map[string]any{"name": n, "description": strings.Split(s.actions[n].Description, ".")[0], "read": p.Read, "control": p.Control})
	}
	return out
}
