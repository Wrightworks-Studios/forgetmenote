package web

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type upperName string

func (n upperName) MarshalText() ([]byte, error) { return []byte(strings.ToUpper(string(n))), nil }

type objectAsArray struct{ A int }

func (objectAsArray) MarshalJSON() ([]byte, error) { return []byte(`[1]`), nil }

type pointerMarshaler struct{ A int }

func (*pointerMarshaler) MarshalJSON() ([]byte, error) { return []byte(`"text"`), nil }

// legacyResult passes one tool result through the compatibility middleware as
// a pre-SEP-2106 client sees it.
func legacyResult(t testing.TB, result mcp.Result) mcp.Result {
	t.Helper()
	handler := legacyMCPResults(func(context.Context, string, mcp.Request) (mcp.Result, error) { return result, nil })
	out, err := handler(context.Background(), "tools/call", &mcp.CallToolRequest{})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestLegacyClientsReceiveOnlyObjectStructuredContent(t *testing.T) {
	var nilStruct *objectAsArray
	var nilMap map[string]any
	for i, value := range []any{
		json.RawMessage(`{"a":1}`), json.RawMessage(" \n\t{}"), json.RawMessage(`[{"a":1}]`), json.RawMessage(`"text"`), json.RawMessage(`null`),
		map[string]any{"a": 1}, map[string]int{}, nilMap, map[int]string{1: "a"}, map[upperName]int{"a": 1},
		struct{ A int }{1}, &struct{ A int }{1}, nilStruct, []any{1}, []byte("raw"), "text", 1.5, true,
		objectAsArray{}, &objectAsArray{}, &pointerMarshaler{}, pointerMarshaler{}, upperName("a"), time.Time{}, &jsonschema.Schema{Type: "array"},
	} {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(i, err)
		}
		want := bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{"))
		out := legacyResult(t, &mcp.CallToolResult{StructuredContent: value}).(*mcp.CallToolResult)
		if kept := out.StructuredContent != nil; kept != want {
			t.Errorf("%d %T %s: kept=%v", i, value, raw, kept)
		}
	}
	for i, schema := range []any{&jsonschema.Schema{Type: "object"}, &jsonschema.Schema{Type: "array"}, &jsonschema.Schema{Types: []string{"object", "null"}}, json.RawMessage(`{"type":"object"}`), map[string]any{"type": "string"}} {
		raw, _ := json.Marshal(schema)
		var typed struct {
			Type string `json:"type"`
		}
		want := json.Unmarshal(raw, &typed) == nil && typed.Type == "object"
		out := legacyResult(t, &mcp.ListToolsResult{Tools: []*mcp.Tool{{Name: "tool", OutputSchema: schema}}}).(*mcp.ListToolsResult)
		if kept := out.Tools[0].OutputSchema != nil; kept != want {
			t.Errorf("schema %d %s: kept=%v", i, raw, kept)
		}
	}
}

// BenchmarkLegacyStructuredContent measures the middleware on a tool result
// that carries a large document, as read_note does for a manuscript.
func BenchmarkLegacyStructuredContent(b *testing.B) {
	document, _ := json.Marshal(map[string]string{"path": "manuscript.md", "content": strings.Repeat("Call me Ishmael. ", 64<<10)})
	result := &mcp.CallToolResult{StructuredContent: json.RawMessage(document)}
	b.SetBytes(int64(len(document)))
	b.ReportAllocs()
	for b.Loop() {
		legacyResult(b, result)
	}
}
