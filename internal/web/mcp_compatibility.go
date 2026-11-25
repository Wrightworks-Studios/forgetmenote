package web

import (
	"bytes"
	"context"
	"encoding"
	"encoding/json"
	"reflect"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Before SEP-2106, structured results and their advertised schemas had to be
// objects. Keep other values in the SDK's JSON text fallback for those clients.
func legacyMCPResults(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		result, err := next(ctx, method, req)
		if err != nil || result == nil {
			return result, err
		}
		if versioned, ok := req.(interface{ ProtocolVersion() string }); ok && versioned.ProtocolVersion() >= "2026-07-28" {
			return result, nil
		}
		switch result := result.(type) {
		case *mcp.ListToolsResult:
			response := *result
			response.Tools = make([]*mcp.Tool, len(result.Tools))
			for i, tool := range result.Tools {
				copy := *tool
				if tool.OutputSchema != nil {
					object, err := objectSchema(tool.OutputSchema)
					if err != nil {
						return nil, err
					}
					if !object {
						copy.OutputSchema = nil
					}
				}
				response.Tools[i] = &copy
			}
			return &response, nil
		case *mcp.CallToolResult:
			if result.StructuredContent != nil {
				object, err := jsonObject(result.StructuredContent)
				if err != nil {
					return nil, err
				}
				if !object {
					response := *result
					response.StructuredContent = nil
					return &response, nil
				}
			}
		}
		return result, nil
	}
}

// objectSchema reports whether a schema's "type" is "object".
func objectSchema(schema any) (bool, error) {
	if s, ok := schema.(*jsonschema.Schema); ok {
		return s != nil && s.Type == "object", nil
	}
	raw, err := json.Marshal(schema)
	if err != nil {
		return false, err
	}
	var typed struct {
		Type string `json:"type"`
	}
	return json.Unmarshal(raw, &typed) == nil && typed.Type == "object", nil
}

var (
	jsonMarshaler = reflect.TypeFor[json.Marshaler]()
	textMarshaler = reflect.TypeFor[encoding.TextMarshaler]()
)

// jsonObject reports whether value encodes as a JSON object. The SDK hands
// tool output over already encoded, so the common case reads one byte instead
// of re-encoding a result that may hold a whole document.
func jsonObject(value any) (bool, error) {
	if raw, ok := value.(json.RawMessage); ok {
		raw = bytes.TrimLeft(raw, " \t\r\n")
		return len(raw) > 0 && raw[0] == '{', nil
	}
	v := reflect.ValueOf(value)
	for v.IsValid() && !customJSON(v.Type()) {
		switch v.Kind() {
		case reflect.Pointer, reflect.Interface:
			if v.IsNil() {
				return false, nil
			}
			v = v.Elem()
		case reflect.Struct:
			return true, nil
		case reflect.Map:
			if v.Type().Key().Kind() != reflect.String {
				return marshalsToObject(value)
			}
			return !v.IsNil(), nil
		default:
			return false, nil
		}
	}
	return marshalsToObject(value)
}

// customJSON reports a type whose encoding its own methods choose.
func customJSON(t reflect.Type) bool {
	pointer := reflect.PointerTo(t)
	return t.Implements(jsonMarshaler) || t.Implements(textMarshaler) || pointer.Implements(jsonMarshaler) || pointer.Implements(textMarshaler)
}

func marshalsToObject(value any) (bool, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return false, err
	}
	return bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")), nil
}
