package actions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"forgetmenote/internal/workspace"
	"github.com/google/jsonschema-go/jsonschema"
	"io"
	"reflect"
	"sync"
)

type Action struct {
	Name, Description string
	Policy            Policy
	Schema            *jsonschema.Schema
	OutputSchema      *jsonschema.Schema
	ClientScope       string
	Resolved          *jsonschema.Resolved
	Call              func(context.Context, json.RawMessage) (json.RawMessage, error)
}

// Registry owns the single operation table used by every transport.
type Registry map[string]Action

var schemas sync.Map // [2]reflect.Type of input and output -> schemaSet

type schemaSet struct {
	input, output *jsonschema.Schema
	resolved      *jsonschema.Resolved
}

func TypedCall[I, O any](fn func(context.Context, I) (O, error)) func(context.Context, json.RawMessage) (json.RawMessage, error) {
	return func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
		var input I
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&input) != nil {
			return nil, errors.New("invalid action arguments")
		}
		var extra any
		if decoder.Decode(&extra) != io.EOF {
			return nil, errors.New("invalid trailing action data")
		}
		output, err := fn(ctx, input)
		if err != nil {
			return nil, err
		}
		return json.Marshal(output)
	}
}
func (registry Registry) Arguments(project, name string, args json.RawMessage) (Action, json.RawMessage, error) {
	a, ok := registry[name]
	if !ok || a.Schema.Properties["project"] == nil && a.Policy.Scope != SelectedRepositories && a.Policy.Scope != SelectedNotebooks {
		return a, nil, workspace.ErrForbidden
	}
	if len(args) > 256<<10 {
		return a, nil, errors.New("action arguments exceed 256 KiB")
	}
	var obj map[string]json.RawMessage
	d := json.NewDecoder(bytes.NewReader(args))
	if err := d.Decode(&obj); err != nil || obj == nil {
		return a, nil, errors.New("action arguments must be an object")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return a, nil, errors.New("invalid trailing action data")
	}
	if p, ok := obj["project"]; ok {
		var supplied string
		if json.Unmarshal(p, &supplied) != nil || supplied != project && a.Policy.Scope != SelectedRepository && a.Policy.Scope != SelectedDocument {
			return a, nil, workspace.ErrForbidden
		}
	}
	if a.Policy.Scope != SelectedRepositories && a.Policy.Scope != SelectedNotebooks && obj["project"] == nil {
		obj["project"], _ = json.Marshal(project)
	}
	if name == "call_data_operation" || name == "read_personal_source" {
		// Models receive a JSON string for this dynamic object so strict tool
		// schemas remain closed. API and MCP callers keep ordinary JSON objects.
		var encoded string
		if json.Unmarshal(obj["arguments"], &encoded) == nil {
			obj["arguments"] = json.RawMessage(encoded)
		}
	}
	raw, _ := json.Marshal(obj)
	var value any
	if json.Unmarshal(raw, &value) != nil || a.Resolved.Validate(value) != nil {
		return a, nil, errors.New("action arguments do not match its schema")
	}
	return a, raw, nil
}

// Schemas returns the input schema, output schema and resolved input of an
// action. They follow from the types alone, and building them is slow enough to
// show when tests start hundreds of servers, so the process keeps one set of
// each. Every caller shares it, so none may change it.
func Schemas[I, O any]() (*jsonschema.Schema, *jsonschema.Schema, *jsonschema.Resolved) {
	key := [2]reflect.Type{reflect.TypeFor[I](), reflect.TypeFor[O]()}
	if built, ok := schemas.Load(key); ok {
		set := built.(schemaSet)
		return set.input, set.output, set.resolved
	}
	options := &jsonschema.ForOptions{TypeSchemas: map[reflect.Type]*jsonschema.Schema{reflect.TypeFor[json.RawMessage](): {}}}
	schema, err := jsonschema.For[I](options)
	if err != nil {
		panic(err)
	}
	resolved, err := schema.Resolve(&jsonschema.ResolveOptions{ValidateDefaults: true})
	if err != nil {
		panic(err)
	}
	output, err := jsonschema.For[O](options)
	if err != nil {
		panic(err)
	}
	built, _ := schemas.LoadOrStore(key, schemaSet{schema, output, resolved})
	set := built.(schemaSet)
	return set.input, set.output, set.resolved
}
